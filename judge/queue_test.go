package judge

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// queueServer answers as Queue's checkJudgeBatch does for the fields the judge controls, so a body Queue would refuse
// fails here first.
func queueServer(t *testing.T, received *[]map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		match := regexp.MustCompile(`^/futures/([0-9a-f]{40})/verdicts$`).FindStringSubmatch(request.URL.Path)
		if request.Method != "POST" || match == nil || request.Header.Get("Authorization") != "Bearer coordinator-token" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(request.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(writer, "not JSON", http.StatusUnprocessableEntity)
			return
		}
		refuse := func(why string) { http.Error(writer, why, http.StatusUnprocessableEntity) }
		for _, field := range []string{"change", "run", "rule"} {
			if value, _ := body[field].(string); value == "" {
				refuse(field + " is named")
				return
			}
		}
		plan, _ := body["plan"].([]any)
		for _, key := range plan {
			if text, _ := key.(string); !hashPattern.MatchString(text) {
				refuse("plan is a list of unit keys")
				return
			}
		}
		decision, _ := body["decision"].(map[string]any)
		status, _ := decision["status"].(string)
		if status != "green" && status != "red" && status != "void" {
			refuse("decision is {status, red, excused, problems}")
			return
		}
		for _, field := range []string{"red", "excused", "problems"} {
			if _, isList := decision[field].([]any); !isList {
				refuse("decision." + field + " is a list")
				return
			}
		}
		verdicts, _ := body["verdicts"].([]any)
		for _, value := range verdicts {
			record, _ := value.(map[string]any)
			if record["future"] != match[1] || record["change"] != body["change"] || record["run"] != body["run"] {
				refuse("a verdict for another batch")
				return
			}
		}
		*received = append(*received, body)
		writer.WriteHeader(http.StatusCreated)
	}))
}

func TestTheLoopsPostIsABatchQueueTakes(t *testing.T) {
	received := []map[string]any{}
	server := queueServer(t, &received)
	defer server.Close()
	unit, other := strings.Repeat("1", 64), strings.Repeat("2", 64)
	h := newHarness()
	h.runs[unit], h.runs[other] = failedWith("TestB"), passed()
	h.script(unit, futureTree, failedWith("TestB"))
	h.script(unit, baseTree, passed())
	loop := Loop{Runs: h.runs, Fabric: h.fabric, Main: h.main, Queue: HTTPQueue{Base: server.URL, Token: "coordinator-token"}, Blobs: &StubBlobs{},
		Now: func() time.Time { return time.Date(2026, 10, 9, 23, 50, 0, 0, time.UTC) }}
	_, err := loop.JudgeFuture(Job{Record: ChangeRecord{Change: "chg_A", Sha: futureTree, Base: baseTree, Owner: "system_adamic_library"},
		Change: "chg_A", Future: futureTree, Base: baseTree, Run: "run-1", Plan: []PlanUnit{{UnitKey: unit}, {UnitKey: other}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 {
		t.Fatalf("queue took %d batches, want 1", len(received))
	}
	decision := received[0]["decision"].(map[string]any)
	if decision["status"] != "red" || len(decision["kicks"].(map[string]any)) != 1 {
		t.Fatalf("decision %v", decision)
	}
}

func TestARefusedBatchIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, "future is decided by another run", http.StatusConflict)
	}))
	defer server.Close()
	err := HTTPQueue{Base: server.URL, Token: "t"}.PostVerdicts(futureTree, FuturePost{})
	if err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "another run") {
		t.Fatalf("err %v, want the 409 and Queue's reason", err)
	}
}

func TestHTTPBlobsPutsTheListWithABuildToken(t *testing.T) {
	var path, authorization, body string
	status := http.StatusCreated
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		content, _ := io.ReadAll(request.Body)
		path, authorization, body = request.Method+" "+request.URL.Path, request.Header.Get("Authorization"), string(content)
		writer.WriteHeader(status)
	}))
	defer server.Close()
	blobs := HTTPBlobs{Base: server.URL, Token: func() (string, error) { return "build-token", nil }}
	content, ref := TestsList([]TestOutcome{outcome("TestA", "pass")})
	if err := blobs.Put(ref.Sha256, content); err != nil {
		t.Fatal(err)
	}
	if path != "PUT /actions/blobs/"+ref.Sha256 || authorization != "Bearer build-token" || body != string(content) {
		t.Fatalf("%s %s %s", path, authorization, body)
	}
	status = http.StatusBadRequest
	if err := blobs.Put(ref.Sha256, content); err == nil {
		t.Fatal("a refused put read as stored")
	}
}
