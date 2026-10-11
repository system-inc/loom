package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/resident"
)

// residentCheck is the resident builder's rule on a real tree (#d1gp9ze): in the clone at --tree, it keys --from whole
// (the warm tree), then --to against it, then --to cold as Workshop keys a tree without a resident, and compares every
// test package, every closure key and every closure file's sha256 with the file on disk. It prints each difference and
// one JSON line of what each way cost, and exits 1 on any difference. --mutant <path> makes the resident keep --from's
// object for path, a stale hash: then the check must find it, and exits 0 only when it did.
func residentCheck(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("resident-check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	tree := flags.String("tree", "", "a clone of the repository whose commits are compared; each is checked out in it, keyless")
	from := flags.String("from", "", "the commit held warm")
	to := flags.String("to", "", "the commit keyed against it, and cold")
	mutant := flags.String("mutant", "", "a path --to changes whose stale hash the resident keeps: the check must catch it")
	if err := flags.Parse(arguments); err != nil || *tree == "" || *from == "" || *to == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: loom resident-check --tree <clone> --from <commit> --to <commit> [--mutant <path>]")
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintln(stderr, "resident-check:", err)
		return 1
	}
	// Either commit may be named any way git names one (a branch, HEAD~1); the resident keys a commit by its sha.
	for _, revision := range []*string{from, to} {
		sha, err := planner.LocalGit(*tree, "rev-parse", "--verify", "--quiet", *revision+"^{commit}").Output()
		if err != nil {
			return fail(fmt.Errorf("%s isn't a commit in %s", *revision, *tree))
		}
		*revision = strings.TrimSpace(string(sha))
	}
	checkout := planner.GitCheckout(*tree)
	holder := resident.New()
	if *mutant != "" {
		holder.Stale(*mutant)
	}
	if _, _, err := checkout(*from); err != nil {
		return fail(err)
	}
	warm, err := holder.Key(*tree, *from)
	if err != nil {
		return fail(err)
	}
	if _, _, err = checkout(*to); err != nil {
		return fail(err)
	}
	keyed, err := holder.Key(*tree, *to)
	if err != nil {
		return fail(err)
	}
	cold, err := resident.Cold(*tree)
	if err != nil {
		return fail(err)
	}
	checkStarted := time.Now()
	differences, files := resident.Check(*tree, holder, keyed, cold)
	for _, difference := range differences {
		fmt.Fprintln(stdout, difference)
	}
	json.NewEncoder(stdout).Encode(map[string]any{
		"from": *from, "to": *to, "changed": len(keyed.Changed), "packages": len(keyed.Packages), "relisted": len(keyed.Relisted),
		"warmSeconds": warm.Seconds, "residentSeconds": keyed.Seconds, "coldSeconds": cold.Seconds,
		"checkSeconds": time.Since(checkStarted).Seconds(), "filesChecked": files, "differences": len(differences),
	})
	if files == 0 {
		return fail(fmt.Errorf("the check read no closure file from disk, so it proved nothing"))
	}
	if *mutant == "" {
		if len(differences) > 0 {
			return 1
		}
		return 0
	}
	// A stale hash needs an object to be stale: a path --from lacks (one --to adds) leaves the resident nothing to keep.
	_, warmHeld := warm.Object(*mutant)
	_, keyedHeld := keyed.Object(*mutant)
	if !slices.Contains(keyed.Changed, *mutant) || !warmHeld || !keyedHeld {
		return fail(fmt.Errorf("--to doesn't change %s from an object --from holds, so its stale hash proves nothing", *mutant))
	}
	// And a key has to read it: a stale hash of a file no closure holds (a main package no test imports) moves nothing.
	if !slices.ContainsFunc(keyed.Packages, func(test planner.ProductTest) bool {
		return slices.Contains(slices.Collect(maps.Values(keyed.ClosureFiles(test.Package))), *mutant)
	}) {
		return fail(fmt.Errorf("no test package's closure holds %s, so its stale hash moves no key and proves nothing", *mutant))
	}
	if slices.ContainsFunc(differences, func(difference string) bool { return strings.HasPrefix(difference, "file "+*mutant+":") }) {
		fmt.Fprintln(stdout, "the check caught it")
		return 0
	}
	fmt.Fprintln(stdout, "the check missed the stale hash of", *mutant)
	return 1
}
