package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/planner"
)

// buildActions is Builder on Workshop (#egbs6we): every product test on a checked-out tree whose productKey the action
// store doesn't hold runs once here and goes up, so test runners only fetch. One JSON line per action, and the exit
// is 1 when any action failed (a product test that failed, or a 409 naming two builders), so a caller can't miss it.
func buildActions(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("build-actions", flag.ContinueOnError)
	flags.SetOutput(stderr)
	tree := flags.String("tree", "", "the checked-out tree to build")
	gateTools := flags.String("gate-tools", "", "the gate tools checkout, for executors.txt's reads lines (as loom plan)")
	read := flags.String("read", "https://adamic-store.kirkouimet.com", "the public store, read direct")
	write := flags.String("write", "", "loom-pipeline's action store, https://<pipeline>/actions")
	home, _ := os.UserHomeDir()
	tokenFile := flags.String("token-file", filepath.Join(home, ".loom", "build-token"), "file holding this builder's build token")
	packages := flags.String("packages", "", "comma-separated import paths to build (default: every package with a product test)")
	scratch := flags.String("scratch", "", "where each action's build log goes (default: the system temporary directory)")
	cache := flags.String("cache", filepath.Join(home, "loom-builder", "cache"), "the build's buildcache directory, shared by its actions and kept between builds")
	jobs := flags.Int("jobs", 4, "actions built at once")
	list := flags.Bool("list", false, "print each action and its productKey, and build nothing")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return 2
	}
	if *tree == "" || *gateTools == "" || (*write == "" && !*list) {
		fmt.Fprintln(stderr, "usage: loom build-actions --tree <dir> --gate-tools <dir> --write <https://pipeline/actions> [--read <url>] [--token-file <path>] [--packages a,b] [--cache <dir>] [--jobs N] [--scratch <dir>] [--list]")
		return 2
	}
	selected := []string{}
	if *packages != "" {
		selected = strings.Split(*packages, ",")
	}
	actions, err := builder.ListActions(*tree, selected)
	if err != nil {
		fmt.Fprintln(stderr, "build-actions:", err)
		return 1
	}
	declared, err := planner.CompilerDeclarations(*tree)
	if err != nil {
		fmt.Fprintln(stderr, "build-actions:", err)
		return 1
	}
	tools, err := planner.ProbeTools("")
	if err != nil {
		fmt.Fprintln(stderr, "build-actions: tools:", err)
		return 1
	}
	tests := make([]planner.ProductTest, len(actions))
	for index, action := range actions {
		tests[index] = planner.ProductTest(action)
	}
	keys, err := planner.ProductKeys(*tree, *gateTools, tools, tests, declared)
	if err != nil {
		fmt.Fprintln(stderr, "build-actions:", err)
		return 1
	}
	key := func(action builder.Action) (string, error) {
		if productKey, known := keys[planner.ProductTest(action)]; known {
			return productKey, nil
		}
		return "", fmt.Errorf("no productKey for %s %s", action.Package, action.Test)
	}
	encoder := json.NewEncoder(stdout)
	if *list {
		for _, action := range actions {
			productKey, err := key(action)
			if err != nil {
				fmt.Fprintln(stderr, "build-actions:", err)
				return 1
			}
			encoder.Encode(map[string]string{"package": action.Package, "test": action.Test, "key": productKey})
		}
		return 0
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(stderr, "build-actions:", err)
		return 1
	}
	work := builder.Builder{
		Tree:    *tree,
		Store:   builder.Store{Read: strings.TrimSuffix(*read, "/"), Write: strings.TrimSuffix(*write, "/"), Token: strings.TrimSpace(string(token))},
		Key:     key,
		Run:     builder.GoTest(*tree),
		Cache:   *cache,
		Scratch: *scratch,
		Jobs:    *jobs,
	}
	// Each result is printed as its action finishes, so a long build shows its progress and a stopped one its record.
	failed := 0
	work.Report = func(result builder.Result) {
		encoder.Encode(result)
		if result.Outcome == "failed" {
			failed++
		}
	}
	work.Build(actions)
	if failed > 0 {
		fmt.Fprintf(stderr, "build-actions: %d of %d actions failed\n", failed, len(actions))
		return 1
	}
	return 0
}
