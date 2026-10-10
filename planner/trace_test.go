package planner

import (
	"os"
	"slices"
	"testing"
)

// A real trace, strace 6.8 on Workshop (the unit-reads review's): a Go program in tree/p reads sub/data.txt, reads
// through sub/link and the directory link sub/dl, stats and lists, writes sub/out.txt and reads it back, and runs a
// shell that changes into sub. It was recorded with the earlier call list, before the forks and chdir joined
// TraceCalls, so where the shell's child runs isn't asserted here. Every form parses, the files opened through links
// are read where they led, and the file the run wrote is no input.
func TestARealStraceTraceParses(t *testing.T) {
	t.Parallel()
	file, err := os.Open("testdata/strace-6.8-review.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	tree := "/home/ahra/review-unit-reads/tree"
	accesses, err := TraceAccesses(file, tree+"/p")
	if err != nil {
		t.Fatal(err)
	}
	for _, read := range []string{"sub/data.txt", "sub/link", "sub/dl/a.txt", "sub/dir/a.txt"} {
		if !slices.Contains(accesses.Reads, tree+"/"+read) {
			t.Errorf("%s isn't read; reads %q", read, accesses.Reads)
		}
	}
	for _, lookup := range []string{"sub/missing.txt", "sub/miss", "sub/dir/a.sh"} {
		if !slices.Contains(accesses.Lookups, tree+"/"+lookup) {
			t.Errorf("%s isn't looked up; lookups %q", lookup, accesses.Lookups)
		}
	}
	if !slices.Contains(accesses.Listings, tree+"/sub/dir") {
		t.Errorf("sub/dir isn't listed; listings %q", accesses.Listings)
	}
	if slices.Contains(accesses.Reads, tree+"/sub/out.txt") || slices.Contains(accesses.Lookups, tree+"/sub/out.txt") {
		t.Error("sub/out.txt, which the run wrote, is an input")
	}
}
