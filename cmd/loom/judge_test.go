package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/system-inc/loom/judge"
)

func TestADryRunPrintsTheBatchAndPostsNothing(t *testing.T) {
	var out bytes.Buffer
	tree := strings.Repeat("f", 40)
	if err := (printedQueue{out: &out}).PostVerdicts(tree, judge.FuturePost{Change: "chg_A", Run: "future-" + tree + "-1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "dry run, not posted: /futures/"+tree+"/verdicts {") || !strings.Contains(out.String(), `"change":"chg_A"`) {
		t.Fatalf("printed %q", out.String())
	}
}

type keptQueue struct{ futures []string }

func (queue *keptQueue) PostVerdicts(future string, post judge.FuturePost) error {
	queue.futures = append(queue.futures, future)
	return nil
}

func TestPostSendsOnlyTheNamedFutureAndPrintsTheRest(t *testing.T) {
	var out bytes.Buffer
	named, other := strings.Repeat("a", 40), strings.Repeat("b", 40)
	live := &keptQueue{}
	queue := scopedQueue{post: named, live: live, dry: printedQueue{out: &out}}
	for _, future := range []string{named, other} {
		if err := queue.PostVerdicts(future, judge.FuturePost{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(live.futures) != 1 || live.futures[0] != named {
		t.Fatalf("posted %v, want the named future alone", live.futures)
	}
	if !strings.Contains(out.String(), "/futures/"+other+"/verdicts") || strings.Contains(out.String(), named) {
		t.Fatalf("printed %q, want the other future only", out.String())
	}
}
