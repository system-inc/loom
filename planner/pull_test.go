package planner

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// A stub Queue: two unplanned futures of one tree (one uncached), the verdict index, and the plans posted back.
func TestPullOncePlansEveryFutureAgainstTheIndex(t *testing.T) {
	t.Parallel()
	tree, gateTools := planFixture(t)
	tools := Tools{Runner: strings.Repeat("d", 64), Go: "go1.27.0"}
	first, err := PlanTree(tree, gateTools, tools, MemoryIndex{}, false)
	if err != nil {
		t.Fatal(err)
	}
	passed := map[string]Verdict{}
	for _, result := range first {
		if result.Name == "example.com/plan/a" {
			passed[result.UnitKey] = Verdict{UnitKey: result.UnitKey, Status: "passed", Run: "run-7"}
		}
	}
	var lock sync.Mutex
	posted := map[string][]PlannedResult{}
	queue := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer coordinator-token" {
			http.Error(writer, "no token", http.StatusUnauthorized)
			return
		}
		switch {
		case request.Method == "GET" && request.URL.Path == "/futures" && request.URL.Query().Get("state") == "unplanned":
			json.NewEncoder(writer).Encode([]Future{{Future: "fut-1", Tree: "tree-sha"}, {Future: "fut-2", Tree: "tree-sha", Uncached: true}})
		case request.Method == "GET" && strings.HasPrefix(request.URL.Path, "/verdicts/"):
			verdict, found := passed[strings.TrimPrefix(request.URL.Path, "/verdicts/")]
			if !found {
				http.NotFound(writer, request)
				return
			}
			json.NewEncoder(writer).Encode(verdict)
		case request.Method == "POST" && strings.HasSuffix(request.URL.Path, "/plan"):
			var body struct{ Units []PlannedResult }
			json.NewDecoder(request.Body).Decode(&body)
			lock.Lock()
			posted[strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/futures/"), "/plan")] = body.Units
			lock.Unlock()
			writer.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer queue.Close()
	client := QueueClient{Base: queue.URL, Token: "coordinator-token"}
	checkout := func(sha string) (string, func(), error) { return tree, func() {}, nil }
	count, err := PullOnce(client, checkout, gateTools, tools, HTTPIndex{Client: client})
	if err != nil || count != 2 {
		t.Fatalf("planned %d futures (%v), want 2", count, err)
	}
	decisions := func(future string) map[string]string {
		result := map[string]string{}
		for _, unit := range posted[future] {
			if !Sha256Hex(unit.UnitKey) {
				t.Errorf("%s posted a unit without a key: %+v", future, unit)
			}
			result[unit.Name] = unit.Decision
		}
		return result
	}
	if got := decisions("fut-1"); got["example.com/plan/a"] != "reuse" || got["example.com/plan/b"] != "run" {
		t.Errorf("fut-1 reuses a's passed key and runs b: %v", got)
	}
	if got := decisions("fut-2"); got["example.com/plan/a"] != "run" || got["example.com/plan/b"] != "run" {
		t.Errorf("fut-2 is uncached and reuses nothing: %v", got)
	}
}
