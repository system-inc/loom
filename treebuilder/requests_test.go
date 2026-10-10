package treebuilder

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A base tree Judge asks for is built like a listed one and ahead of the listing's, once whoever wants it, and not
// at all when the store holds it; a request that isn't a tree key, a commit and a Go release is named, never checked
// out. Mutants: requests left unread; requested after the listing; a malformed request built.
func TestABaseTreeJudgeAsksForIsBuiltFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	requests, err := OpenRequests(path)
	if err != nil {
		t.Fatal(err)
	}
	defer requests.Close()
	base, bad := strings.Repeat("e", 40), strings.Repeat("f", 40)
	for _, request := range []Request{
		{Tree: keyC, Commit: base, Go: "go1.27.2", At: "2026-10-10T12:00:00Z"},
		{Tree: "not a key", Commit: bad, Go: "go1.27.2", At: "2026-10-10T12:00:00Z"},
		{Tree: keyD, Commit: "main", Go: "go1.27.2", At: "2026-10-10T12:00:00Z"},
		{Tree: keyA, Commit: base, Go: "go1.27.2", At: "2026-10-10T12:00:00Z"},
		{Tree: keyB, Commit: base, Go: "go1.27.2", At: "2026-10-10T12:00:00Z"},
	} {
		if _, err := requests.Ask(request); err != nil {
			t.Fatal(err)
		}
	}
	// Asked again, a tree keeps its first request.
	if first, err := requests.Ask(Request{Tree: keyC, Commit: base, Go: "go1.27.2", At: "2026-10-10T12:30:00Z"}); err != nil || first.At != "2026-10-10T12:00:00Z" {
		t.Fatalf("asked again: %+v %v", first, err)
	}
	source := listedFutures{future("1", unit(t, "test", "run", keyB))}
	ledger := openLedger(t, filepath.Join(t.TempDir(), "trees.jsonl"), time.Now())
	h := newHarness(t, source, ledger)
	h.builder.Requests = func() ([]Request, error) { return ReadRequests(path, 24*time.Hour, h.now) }
	h.indexed[keyA] = true
	for range 2 {
		h.buildOnce(t, true)
	}
	h.buildOnce(t, false)
	if len(h.builds) != 2 || h.builds[0] != (Want{Tree: keyC, Future: base, Go: "go1.27.2"}) || h.builds[1] != (Want{Tree: keyB, Future: base, Go: "go1.27.2"}) {
		t.Fatalf("built %+v, want the requested trees c then b, b once, a (indexed) and the malformed never", h.builds)
	}
	if newest, _ := ledger.Newest(keyC); newest.Event != Built || newest.Future != base {
		t.Fatalf("tree c's newest record is %+v", newest)
	}
	// A request older than the builder keeps is left: Judge has asked anew if it still waits.
	h.now = time.Date(2026, 10, 12, 13, 0, 0, 0, time.UTC)
	h.indexed = map[string]bool{}
	h.builds = nil
	h.builder.Source = listedFutures{}
	h.buildOnce(t, false)
	if len(h.builds) != 0 {
		t.Fatalf("built %+v from requests two days old", h.builds)
	}
}

// Judge compacts its requests: one older than it keeps goes, so the tree is asked for anew, its wait from then.
func TestJudgesRequestsAreCompacted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	requests, err := OpenRequests(path)
	if err != nil {
		t.Fatal(err)
	}
	defer requests.Close()
	requests.Ask(Request{Tree: keyA, Commit: strings.Repeat("e", 40), Go: "go1.27.2", At: "2026-10-09T12:00:00Z"})
	requests.Ask(Request{Tree: keyB, Commit: strings.Repeat("e", 40), Go: "go1.27.2", At: "2026-10-10T12:00:00Z"})
	if err := requests.Compact(24*time.Hour, time.Date(2026, 10, 10, 13, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if kept, _ := ReadRequests(path, 0, time.Now()); len(kept) != 1 || kept[0].Tree != keyB {
		t.Fatalf("kept %+v", kept)
	}
	if again, _ := requests.Ask(Request{Tree: keyA, Commit: strings.Repeat("e", 40), Go: "go1.27.2", At: "2026-10-10T13:00:00Z"}); again.At != "2026-10-10T13:00:00Z" {
		t.Fatalf("a compacted tree asked again kept its old request: %+v", again)
	}
}
