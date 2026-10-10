package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/livestatus"
	"github.com/system-inc/loom/treebuilder"
)

// The clock's laps end each phase where the next begins, so they add up to no more than the build's total, and each
// lap puts the phases ended so far in the live status, in the order they ran. Mutants: a lap that doesn't start the
// next phase (each counts from the build's start, and two add up past the total); a lap that never reaches the live
// status.
func TestTheTreeClocksLapsAddUpToTheBuild(t *testing.T) {
	started := time.Now()
	clock := newTreeClock(started)
	clock.live = livestatus.NewWriter(livestatus.TreePath(t.TempDir()), livestatus.Status{Kind: livestatus.KindTree, Tree: &livestatus.Tree{Phase: "readying"}})
	defer clock.live.Close()
	time.Sleep(30 * time.Millisecond)
	clock.lap(&clock.phases.Readying)
	time.Sleep(30 * time.Millisecond)
	clock.lap(&clock.phases.Listing)
	clock.finish()
	phases := clock.phases
	if phases.Readying < 0.03 || phases.Listing < 0.03 || phases.Readying+phases.Listing > phases.Total+1e-6 {
		t.Fatalf("readying %.3f and listing %.3f over a total of %.3f", phases.Readying, phases.Listing, phases.Total)
	}
	names := []string{}
	for _, phase := range clock.live.Status().Tree.Phases {
		names = append(names, phase.Name)
	}
	if !slices.Equal(names, []string{"readying", "listing", "total"}) {
		t.Fatalf("the live status holds %v", names)
	}
}

// The phase list holds only the phases a build has ended, in the order they run, a split that counted nothing left
// out, and the table prints each, the product splits with their count. Mutants: a phase that never ran listed at zero;
// fetched and built swapped; a counted split listed without its count.
func TestThePhaseListHoldsTheEndedPhasesInOrder(t *testing.T) {
	phases := builder.TreePhases{Readying: 1, Warm: 9, WarmTests: 6, WarmMains: 3, Products: 20, ProductsBuilt: 4, ProductsBuiltSeconds: 50, UploadIndex: 0.2, Removal: 0.8, Total: 32}
	listed := treePhaseList(phases)
	want := []livestatus.Phase{{Name: "readying", Seconds: 1}, {Name: "warm tests", Seconds: 6}, {Name: "warm mains", Seconds: 3}, {Name: "products", Seconds: 20},
		{Name: "built", Seconds: 50, Count: 4}, {Name: "upload index", Seconds: 0.2}, {Name: "removal", Seconds: 0.8}, {Name: "total", Seconds: 32}}
	if !slices.Equal(listed, want) {
		t.Fatalf("listed %+v, want %+v", listed, want)
	}
	var table bytes.Buffer
	writePhaseTable(&table, phases)
	if text := table.String(); !strings.Contains(text, "built") || !strings.Contains(text, "4 products") || strings.Contains(text, "fetched") ||
		strings.Count(text, "\n") != len(want)+1 {
		t.Fatalf("the table:\n%s", text)
	}
}

// The tree builder reads build-tree's phases from its summary line in the log, past the package lines before it and
// the table after it, and keeps the checkout's own seconds with them; a build that printed no summary still has its
// checkout's. Mutants: the first JSON line read (a package's, with no phases); the checkout's seconds dropped once the
// summary is read.
func TestTheTreeBuilderKeepsBuildTreesPhasesAndItsCheckouts(t *testing.T) {
	logs := t.TempDir()
	settings, err := parseBuildTreesFlags([]string{"--queue", "https://queue", "--token-file", "token", "--logs", logs}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	checkout := func(string) (string, func(), error) {
		time.Sleep(30 * time.Millisecond)
		return t.TempDir(), func() {}, nil
	}
	script := func(body string) string {
		path := filepath.Join(t.TempDir(), "loom")
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	built := script(`echo '{"package":"a","directory":"a","products":[],"seconds":1}'
echo '{"tree":"t","treeKey":"k","phases":{"readying":1.5,"warm":95.8,"productsBuilt":12,"total":700}}'
echo 'build-tree: phases' >&2
`)
	want := treebuilder.Want{Tree: strings.Repeat("b", 64), Future: strings.Repeat("2", 40), Go: "go1.27.1"}
	phases, err := buildWant(context.Background(), checkout, built, settings, want)
	if err != nil || phases == nil || phases.Checkout < 0.03 || phases.Warm != 95.8 || phases.ProductsBuilt != 12 || phases.Total != 700 {
		t.Fatalf("a build's phases %+v (%v)", phases, err)
	}
	failed := script("echo 'build-tree: not starting' >&2\nexit 1\n")
	phases, err = buildWant(context.Background(), checkout, failed, settings, want)
	if err == nil || phases == nil || phases.Checkout < 0.03 || phases.Total != 0 {
		t.Fatalf("a failed build's phases %+v (%v)", phases, err)
	}
	if readTreePhases(filepath.Join(logs, "absent.log")) != nil {
		t.Fatal("phases read from no log")
	}
}
