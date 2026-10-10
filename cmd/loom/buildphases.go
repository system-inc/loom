package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/livestatus"
)

// A treeClock times build-tree's phases (builder.TreePhases): each lap ends a phase at the moment the next begins, so
// the laps add up to the build, and each puts the phases so far in the live status for `loom top`.
type treeClock struct {
	started time.Time
	last    time.Time
	phases  builder.TreePhases
	live    *livestatus.Writer
}

func newTreeClock(started time.Time) *treeClock {
	return &treeClock{started: started, last: started}
}

// lap ends the phase whose seconds phase holds, now, and shows every phase ended so far.
func (clock *treeClock) lap(phase *float64) {
	now := time.Now()
	*phase, clock.last = now.Sub(clock.last).Seconds(), now
	clock.show()
}

// show puts the phases so far in the live status.
func (clock *treeClock) show() {
	ended := treePhaseList(clock.phases)
	clock.live.Update(func(status *livestatus.Status) { status.Tree.Phases = ended })
}

// finish sets Total, from the build's start.
func (clock *treeClock) finish() {
	clock.phases.Total = time.Since(clock.started).Seconds()
	clock.show()
}

// treePhaseList is the phases a build has ended, in the order they run, by the names `loom top` shows: a phase not yet
// ended holds no seconds and is left out, as a split counting nothing is. Warm and Upload are shown by their parts,
// which say more than their sum.
func treePhaseList(phases builder.TreePhases) []livestatus.Phase {
	listed := []livestatus.Phase{}
	add := func(name string, seconds float64, count int) {
		if seconds > 0 || count > 0 {
			listed = append(listed, livestatus.Phase{Name: name, Seconds: seconds, Count: count})
		}
	}
	add("checkout", phases.Checkout, 0)
	add("keying", phases.Keying, 0)
	add("readying", phases.Readying, 0)
	add("npm install", phases.NpmInstall, 0)
	add("listing", phases.Listing, 0)
	add("warm tests", phases.WarmTests, 0)
	add("warm mains", phases.WarmMains, 0)
	add("products", phases.Products, 0)
	add("fetched", phases.ProductsFetchedSeconds, phases.ProductsFetched)
	add("built", phases.ProductsBuiltSeconds, phases.ProductsBuilt)
	add("binaries", phases.Binaries, 0)
	add("source chunks", phases.SourceChunks, 0)
	add("module cache", phases.ModuleCache, 0)
	add("upload modules", phases.UploadModules, 0)
	add("upload chunks", phases.UploadChunks, 0)
	add("upload products", phases.UploadProducts, 0)
	add("upload binaries", phases.UploadBinaries, 0)
	add("upload index", phases.UploadIndex, 0)
	add("removal", phases.Removal, 0)
	add("total", phases.Total, 0)
	return listed
}

// writePhaseTable prints the phases as a table, one a line, for a person reading build-tree's log: the counted splits
// of products indented under it, with their count.
func writePhaseTable(writer io.Writer, phases builder.TreePhases) {
	fmt.Fprintln(writer, "build-tree: phases")
	for _, phase := range treePhaseList(phases) {
		if phase.Name == "fetched" || phase.Name == "built" {
			fmt.Fprintf(writer, "  %-18s %9.1fs  %d products, their own seconds summed\n", "  "+phase.Name, phase.Seconds, phase.Count)
			continue
		}
		fmt.Fprintf(writer, "  %-18s %9.1fs\n", phase.Name, phase.Seconds)
	}
}

// readTreePhases is the phases build-tree's summary line holds in its log, the last line that names them; nil when the
// build printed none (it failed before its summary, or the log can't be read).
func readTreePhases(log string) *builder.TreePhases {
	content, err := os.ReadFile(log)
	if err != nil {
		return nil
	}
	var found *builder.TreePhases
	for _, line := range bytes.Split(content, []byte("\n")) {
		if !bytes.HasPrefix(bytes.TrimSpace(line), []byte("{")) {
			continue
		}
		var summary struct {
			Phases *builder.TreePhases `json:"phases"`
		}
		if json.Unmarshal(line, &summary) == nil && summary.Phases != nil {
			found = summary.Phases
		}
	}
	return found
}
