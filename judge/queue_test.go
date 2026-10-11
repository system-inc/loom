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

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/r2/r2test"
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

func TestStoreBlobsPutsTheListStraightIntoTheBucketOnce(t *testing.T) {
	fake := r2test.New(t)
	bucket := fake.Bucket()
	blobs := StoreBlobs{Store: builder.Store{Bucket: &bucket}}
	content, ref := TestsList([]TestOutcome{outcome("TestA", "pass")})
	if err := blobs.Put(ref.Sha256, content); err != nil {
		t.Fatal(err)
	}
	if held, found := fake.Object("blobs/" + ref.Sha256); !found || string(held) != string(content) {
		t.Fatalf("the bucket holds %q", held)
	}
	if err := blobs.Put(ref.Sha256, content); err != nil || fake.Count("PUT", "blobs/") != 1 {
		t.Fatalf("a second put of a fresh list: %v, %v", err, fake.Requests())
	}
	if err := blobs.Put(strings.Repeat("b", 64), content); err == nil || fake.Count("PUT", "blobs/") != 1 {
		t.Fatalf("a list under another name: %v", err)
	}
}

// The index keeps each key's newest verdict (#ybxadkf: ce0ef503 planned a reuse of the verify's run 4, then 3282cdde
// decided the same key): a pass from a later run is the reuse, named by its run; a newest verdict that isn't a pass is
// unbacked; a record without tests by reference is an error. Mutants: the plan's run required again (the later pass
// errors); a failed newest verdict read as a pass; the run not carried.
func TestHTTPReusedReadsTheKeysNewestVerdict(t *testing.T) {
	sha := strings.Repeat("a", 64)
	body := `{"status":"passed","run":"run-2","tests":{"failed":0,"inline":[],"passed":3,"sha256":"` + sha + `","skipped":0}}`
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path = request.URL.Path
		writer.Write([]byte(body))
	}))
	defer server.Close()
	reused := HTTPReused{Base: server.URL, Token: "t"}
	for _, planned := range []string{"run-2", "run-1", "reused"} {
		verdict, err := reused.Tests("u", planned)
		if err != nil || path != "/verdicts/u" || verdict.Run != "run-2" || verdict.Unbacked != "" || !strings.Contains(string(verdict.Tests), `"passed":3`) {
			t.Fatalf("planned %s: %+v (%v) from %s", planned, verdict, err, path)
		}
	}
	for _, status := range []string{"failed", "void"} {
		body = `{"status":"` + status + `","run":"run-3","tests":{"sha256":"` + sha + `"}}`
		if verdict, err := reused.Tests("u", "run-2"); err != nil || verdict.Unbacked == "" || verdict.Tests != nil {
			t.Fatalf("a %s newest verdict: %+v (%v), want unbacked", status, verdict, err)
		}
	}
	body = `{"status":"passed","run":"run-2","tests":[]}`
	if _, err := reused.Tests("u", "run-2"); err == nil {
		t.Fatal("read a pass with no tests by reference as the reused verdict")
	}
}
