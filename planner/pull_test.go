package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// A stub Queue in loom-pipeline's shapes (wire/source/Queue.ts): two unplanned futures of one tree (one uncached),
// the verdict index, and the plans posted back as bare lists, each unit's key recomputed from its posted keyParts.
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
			json.NewEncoder(writer).Encode(map[string][]Future{"futures": {{Future: "fut-1", Tree: "tree-sha"}, {Future: "fut-2", Tree: "tree-sha", Uncached: true},
				{Future: "fut-3", Tree: "tree-sha", Changes: []string{"chg-3"}, Parity: true, Select: &ParitySelect{Packages: []string{"example.com/plan/a"}}}}})
		case request.Method == "GET" && request.URL.Path == "/changes/chg-3":
			json.NewEncoder(writer).Encode(map[string]any{"record": map[string]any{"paths": []string{"a/a.go"}}})
		case request.Method == "GET" && strings.HasPrefix(request.URL.Path, "/verdicts/"):
			verdict, found := passed[strings.TrimPrefix(request.URL.Path, "/verdicts/")]
			if !found {
				http.NotFound(writer, request)
				return
			}
			json.NewEncoder(writer).Encode(verdict)
		case request.Method == "POST" && strings.HasSuffix(request.URL.Path, "/plan"):
			var units []struct {
				PlannedResult
				KeyParts map[string]any `json:"keyParts"`
			}
			if err := json.NewDecoder(request.Body).Decode(&units); err != nil {
				http.Error(writer, "the plan is a JSON list of units", http.StatusUnprocessableEntity)
				return
			}
			results := []PlannedResult{}
			for _, unit := range units {
				// Queue keys the posted keyParts as JSON, so a null where the key had [] or {} is a mismatch.
				canonical, _ := Canonical(unit.KeyParts)
				if sum := sha256.Sum256(append([]byte(unitKeyVersion+"\n"), canonical...)); hex.EncodeToString(sum[:]) != unit.UnitKey {
					http.Error(writer, "unitKey is not its keyParts' key", http.StatusUnprocessableEntity)
					return
				}
				results = append(results, unit.PlannedResult)
			}
			lock.Lock()
			posted[strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/futures/"), "/plan")] = results
			lock.Unlock()
			writer.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer queue.Close()
	client := QueueClient{Base: queue.URL, Token: "coordinator-token"}
	checkout := func(sha string) (string, func(), error) { return tree, func() {}, nil }
	if count, err := PullOnce(client, checkout, gateTools, tools, HTTPIndex{Client: client}, "fut-2", ""); err != nil || count != 1 || len(posted) != 1 || posted["fut-2"] == nil {
		t.Fatalf("planning fut-2 alone planned %d futures (%v), posted %d", count, err, len(posted))
	}
	delete(posted, "fut-2")
	count, err := PullOnce(client, checkout, gateTools, tools, HTTPIndex{Client: client}, "", strings.Repeat("4", 64))
	if err != nil || count != 3 {
		t.Fatalf("planned %d futures (%v), want 3", count, err)
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
	if got := decisions("fut-3"); len(got) != 1 || got["example.com/plan/a"] != "run" {
		t.Errorf("fut-3 is a parity run of a alone, run though a's key passed: %v", got)
	}
}
