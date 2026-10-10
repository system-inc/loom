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
	read := flags.String("read", "https://artifacts.loom.system.inc", "the public store, read direct")
	write := flags.String("write", "", "loom's action store, https://<pipeline>/actions")
	home, _ := os.UserHomeDir()
	tokenFile := flags.String("token-file", filepath.Join(home, ".loom", "build-token"), "file holding this builder's build token")
	packages := flags.String("packages", "", "comma-separated import paths to build (default: every package with a product test)")
	scratch := flags.String("scratch", "", "where each action's build log goes (default: the system temporary directory)")
	cache := flags.String("cache", filepath.Join(home, "loom-builder", "cache"), "the base of each tree's own buildcache directory, <base>/<tree hash>, shared only by that tree's actions")
	keepCaches := flags.Int("keep-caches", 2, "tree caches kept under --cache, newest first")
	jobs := flags.Int("jobs", 4, "actions built at once")
	indexDirectory := flags.String("index", filepath.Join(home, "loom-builder", "index"), "Workshop's index of what the store holds, so deciding reads nothing ('' for none)")
	trustIndex := flags.Bool("trust-index", true, "an index miss builds without asking the store (the store's 200 or 409 still checks it)")
	list := flags.Bool("list", false, "print each action and its productKey, and build nothing")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return 2
	}
	if *tree == "" || *gateTools == "" || (*write == "" && !*list) {
		fmt.Fprintln(stderr, "usage: loom build-actions --tree <dir> --gate-tools <dir> --write <https://pipeline/actions> [--read <url>] [--token-file <path>] [--packages a,b] [--cache <dir>] [--keep-caches N] [--jobs N] [--index <dir>] [--scratch <dir>] [--list]")
		return 2
	}
	selected := []string{}
	if *packages != "" {
		selected = strings.Split(*packages, ",")
	}
	tests, err := planner.ListProductTests(*tree, selected)
	if err != nil {
		fmt.Fprintln(stderr, "build-actions:", err)
		return 1
	}
	actions := make([]builder.Action, len(tests))
	for index, test := range tests {
		actions[index] = builder.Action(test)
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
	requests := &builder.Requests{}
	var index *builder.Index
	if *indexDirectory != "" {
		if index, err = builder.OpenIndex(*indexDirectory); err != nil {
			fmt.Fprintln(stderr, "build-actions:", err)
			return 1
		}
		defer index.Close()
	}
	treeCache, err := builder.TreeCache(*cache, *tree, *keepCaches)
	if err != nil {
		fmt.Fprintln(stderr, "build-actions:", err)
		return 1
	}
	work := builder.Builder{
		Tree:       *tree,
		Index:      index,
		TrustIndex: *trustIndex,
		Store:      builder.Store{Read: strings.TrimSuffix(*read, "/"), Write: strings.TrimSuffix(*write, "/"), Token: strings.TrimSpace(string(token)), Requests: requests},
		Key:        key,
		Run:        builder.GoTest(*tree),
		Cache:      treeCache,
		Scratch:    *scratch,
		Jobs:       *jobs,
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
	// What this build cost the store, by our own count: reads of refs and blobs, writes of blobs and refs.
	fmt.Fprintf(stderr, "build-actions: %d actions, %d store reads, %d store writes\n", len(actions), requests.Reads.Load(), requests.Writes.Load())
	if failed > 0 {
		fmt.Fprintf(stderr, "build-actions: %d of %d actions failed\n", failed, len(actions))
		return 1
	}
	return 0
}
