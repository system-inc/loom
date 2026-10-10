package queuebridge

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The bridge's pass against a stand-in queue and git: what it posts, and what it must not (ported from
// queue_bridge_test.py). Each mutant below must make a test here fail:
//
//	facts posted without the queue's seq, or with a seq that isn't a whole JSON number: TestMainsHeadRidesWithTheQueuesSeqReadBeforeGitOrTheChangeWaitsATick
//	the seq read after git: TestMainsHeadRidesWithTheQueuesSeqReadBeforeGitOrTheChangeWaitsATick
//	facts without main's head posted: TestMainsHeadRidesWithTheQueuesSeqReadBeforeGitOrTheChangeWaitsATick
//	anything asked or posted but facts: TestTheBridgeOnlyCarriesGitsFacts

var (
	tree   = strings.Repeat("1", 40)
	older  = strings.Repeat("a", 40)
	newer  = strings.Repeat("b", 40)
	change = "chg_" + strings.Repeat("c", 26)
)

type request struct {
	Method string
	Path   string
	Body   map[string]any
}

// A fakeQueue answers the bridge's reads and records every call, each body as the JSON the wire reads.
type fakeQueue struct {
	unchecked []map[string]any
	// head is GET /head's answer: status and body; nil answers seq 7.
	head     func() (int, string)
	requests []request
}

func (queue *fakeQueue) Call(method, path string, body any) (int, []byte, error) {
	decoded := map[string]any(nil)
	if body != nil {
		encoded, _ := json.Marshal(body)
		json.Unmarshal(encoded, &decoded)
	}
	queue.requests = append(queue.requests, request{method, path, decoded})
	answer := func(value any) (int, []byte, error) {
		encoded, _ := json.Marshal(value)
		return 200, encoded, nil
	}
	switch {
	case method == "GET" && strings.HasPrefix(path, "/submissions"):
		return answer(map[string]any{"changes": append([]map[string]any{}, queue.unchecked...)})
	case path == "/head":
		if queue.head != nil {
			status, text := queue.head()
			return status, []byte(text), nil
		}
		return answer(map[string]any{"seq": 7, "head": strings.Repeat("0", 64)})
	case strings.HasPrefix(path, "/futures"):
		return answer(map[string]any{"futures": []map[string]any{{"tree": tree, "changes": []string{change}}}})
	case path == "/landings":
		return answer(map[string]any{"landings": []map[string]any{{"change": change, "future": tree}}})
	}
	return answer(map[string]any{"ok": true})
}

func (queue *fakeQueue) posts() []request {
	var posts []request
	for _, made := range queue.requests {
		if made.Method == "POST" {
			posts = append(posts, made)
		}
	}
	return posts
}

func (queue *fakeQueue) asked() []string {
	var paths []string
	for _, made := range queue.requests {
		paths = append(paths, made.Method+" "+made.Path)
	}
	return paths
}

// A fakeGate is git with fixed facts, recording the queue's requests each time it was asked.
type fakeGate struct {
	queue  *fakeQueue
	asked  [][]string
	noHead bool
}

func (gate *fakeGate) Facts(sha, base string) (map[string]any, error) {
	if gate.queue != nil {
		gate.asked = append(gate.asked, gate.queue.asked())
	}
	facts := map[string]any{"shaExists": true, "baseIsAncestor": true, "baseOnMain": base == older, "diffPaths": []string{"a.go"}, "mainHead": newer}
	if gate.noHead {
		delete(facts, "mainHead")
	}
	return facts, nil
}

func submitted() []map[string]any {
	return []map[string]any{{"change": change, "sha": tree, "base": older, "paths": []string{"a.go"}}}
}

func wantFacts() map[string]any {
	return map[string]any{"shaExists": true, "baseIsAncestor": true, "baseOnMain": true, "diffPaths": []any{"a.go"}, "mainHead": newer, "asOf": 7.0}
}

func tick(queue Queue, gate Gate) bool {
	return Bridge{Queue: queue, Gate: gate, Log: func(string) {}}.Tick()
}

func TestMainsHeadRidesWithTheQueuesSeqReadBeforeGitOrTheChangeWaitsATick(t *testing.T) {
	queue := &fakeQueue{unchecked: submitted()}
	gate := &fakeGate{queue: queue}
	if !tick(queue, gate) {
		t.Fatal("the queue read as unreadable")
	}
	if !reflect.DeepEqual(gate.asked, [][]string{{"GET /submissions?state=unchecked", "GET /head"}}) {
		t.Fatalf("git was asked after %q", gate.asked)
	}
	if posts := queue.posts(); len(posts) != 1 || posts[0].Path != "/submissions/"+change+"/facts" || !reflect.DeepEqual(posts[0].Body, wantFacts()) {
		t.Fatalf("posted %+v", posts)
	}
	// No seq to order it by, or no head: nothing is posted, so the change stays unchecked and is read again next tick.
	for _, answer := range []struct {
		status int
		body   string
		noHead bool
	}{{503, `{"error":"down"}`, false}, {429, `{"error":"slow"}`, false}, {200, `{"seq":"7"}`, false}, {200, `{"seq":true}`, false},
		{200, `{"seq":-1}`, false}, {200, `{"seq":7.5}`, false}, {200, `not json`, false}, {200, `{"seq":7}`, true}} {
		queue := &fakeQueue{unchecked: submitted(), head: func() (int, string) { return answer.status, answer.body }}
		tick(queue, &fakeGate{noHead: answer.noHead})
		if posts := queue.posts(); len(posts) != 0 {
			t.Errorf("%+v: posted %+v", answer, posts)
		}
	}
}

// Judge decides every future and the pusher lands: the bridge reads the unchecked changes and the head, and posts only
// facts, even with futures and landing orders waiting.
func TestTheBridgeOnlyCarriesGitsFacts(t *testing.T) {
	queue := &fakeQueue{unchecked: submitted()}
	tick(queue, &fakeGate{})
	if asked := queue.asked(); !reflect.DeepEqual(asked, []string{"GET /submissions?state=unchecked", "GET /head", "POST /submissions/" + change + "/facts"}) {
		t.Fatalf("asked %q", asked)
	}
}

// A queue it can't read is a pass that carried nothing, which the command's exit says.
func TestAnUnreadableQueueIsAFailedPass(t *testing.T) {
	queue := &fakeQueue{}
	if tick(failingQueue{queue}, &fakeGate{}) {
		t.Fatal("a pass that couldn't read the queue passed")
	}
}

type failingQueue struct{ *fakeQueue }

func (queue failingQueue) Call(method, path string, body any) (int, []byte, error) {
	return 503, []byte(`{"error":"down"}`), nil
}
