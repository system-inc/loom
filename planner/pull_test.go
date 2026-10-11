package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// A stub Queue in loom's shapes (wire/source/Queue.ts): two unplanned futures of one tree (one uncached),
// the verdict index, and the plans posted back as bare lists, each unit's key recomputed from its posted keyParts.
func TestPullOncePlansEveryFutureAgainstTheIndex(t *testing.T) {
	t.Parallel()
	tree, gateTools := planFixture(t)
	tools := Tools{Runner: strings.Repeat("d", 64), Go: localGo(t)}
	gateInputs := strings.Repeat("4", 64)
	first, err := PlanChange(tree, gateTools, tools, MemoryIndex{}, false, ParityInputs{GateInputs: gateInputs})
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
				unit.PlannedResult.KeyParts.Kind, _ = unit.KeyParts["kind"].(string)
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
	if count, err := PullOnce(client, checkout, gateTools, tools, HTTPIndex{Client: client}, "fut-2", gateInputs); err != nil || count != 1 || len(posted) != 1 || posted["fut-2"] == nil {
		t.Fatalf("planning fut-2 alone planned %d futures (%v), posted %d", count, err, len(posted))
	}
	delete(posted, "fut-2")
	count, err := PullOnce(client, checkout, gateTools, tools, HTTPIndex{Client: client}, "", gateInputs)
	if err != nil || count != 3 {
		t.Fatalf("planned %d futures (%v), want 3", count, err)
	}
	decisions := func(future string) map[string]string {
		result := map[string]string{}
		for _, unit := range posted[future] {
			if !Sha256Hex(unit.UnitKey) {
				t.Errorf("%s posted a unit without a key: %+v", future, unit)
			}
			// Phase units ride every plan, always run; the decisions here are the test units'.
			if strings.HasPrefix(unit.Name, "phase:") {
				if unit.Decision != "run" || unit.KeyParts.Kind != "phase" {
					t.Errorf("%s posted %s as %s, kind %s", future, unit.Name, unit.Decision, unit.KeyParts.Kind)
				}
				continue
			}
			result[unit.Name] = unit.Decision
		}
		if phases := len(posted[future]) - len(result); phases != 3 {
			t.Errorf("%s posted %d phase units, want build, vet and gofmt", future, phases)
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
	// Every unit carries the tree key build-tree writes this tree's index under (a phase unit for its npm packages
	// alone, #v03v751): its git tree hash, the Go release and platform go reports inside it, and the gate environment,
	// read here apart from the planner's code.
	want := buildTreeKey(t, tree)
	for future, units := range posted {
		for _, unit := range units {
			if unit.Tree != want {
				t.Errorf("%s posted %s (%s) with tree %q, want %q", future, unit.Name, unit.KeyParts.Kind, unit.Tree, want)
			}
		}
	}
}

// buildTreeKey is the key build-tree writes a checked-out tree's index under, read with git and go themselves: sha256
// of "loom-tree-v2", the tree hash, the local go's release, the runners' linux/amd64, and the gate environment's sorted
// lines.
func buildTreeKey(t *testing.T, tree string) string {
	t.Helper()
	read := func(name string, arguments ...string) string {
		command := exec.Command(name, arguments...)
		command.Dir = tree
		command.Env = append(os.Environ(), "GOTOOLCHAIN=local")
		output, err := command.Output()
		if err != nil {
			t.Fatalf("%s %v: %v", name, arguments, err)
		}
		return strings.TrimSpace(string(output))
	}
	lines := []string{}
	for name, value := range GateEnvironment {
		lines = append(lines, name+"="+value)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte("loom-tree-v2\n" + read("git", "rev-parse", "HEAD^{tree}") + "\n" + read("go", "env", "GOVERSION") + "\n" +
		"linux/amd64" + "\n" + strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// A future moving no unit's key posts an empty plan, decided from the keys: a Markdown edit nothing reads is empty,
// a Markdown file a unit reads (its testdata) plans its reader, and a Go edit plans normally.
func TestAFutureMovingNoKeyPostsAnEmptyPlan(t *testing.T) {
	t.Parallel()
	tree, gateTools := planFixture(t)
	writeFiles(t, tree, map[string]string{"README.md": "readme\n", "a/testdata/notes.md": "notes\n"})
	if output, err := exec.Command("git", "-C", tree, "add", ".").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v %s", err, output)
	}
	tools := Tools{Runner: strings.Repeat("d", 64), Go: localGo(t)}
	changes := map[string][]string{"chg-readme": {"README.md"}, "chg-notes": {"a/testdata/notes.md"}, "chg-go": {"a/a.go"}}
	var lock sync.Mutex
	posted := map[string]string{}
	queue := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == "GET" && request.URL.Path == "/futures":
			json.NewEncoder(writer).Encode(map[string][]Future{"futures": {
				{Future: "readme", Tree: "readme", Base: "base-readme", Changes: []string{"chg-readme"}},
				{Future: "notes", Tree: "notes", Base: "base-notes", Changes: []string{"chg-notes"}},
				{Future: "go", Tree: "go", Base: "base-go", Changes: []string{"chg-go"}},
			}})
		case request.Method == "GET" && strings.HasPrefix(request.URL.Path, "/changes/"):
			json.NewEncoder(writer).Encode(map[string]any{"record": map[string]any{"paths": changes[strings.TrimPrefix(request.URL.Path, "/changes/")]}})
		case request.Method == "GET" && strings.HasPrefix(request.URL.Path, "/verdicts/"):
			http.NotFound(writer, request)
		case request.Method == "POST":
			body, _ := io.ReadAll(request.Body)
			lock.Lock()
			posted[strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/futures/"), "/plan")] = string(body)
			lock.Unlock()
		}
	}))
	defer queue.Close()
	// Each future's tree edits its change's file; its base doesn't.
	edited := map[string]string{"readme": "README.md", "notes": "a/testdata/notes.md", "go": "a/a.go"}
	originals := map[string][]byte{}
	for _, path := range edited {
		originals[path], _ = os.ReadFile(filepath.Join(tree, path))
	}
	checkout := func(sha string) (string, func(), error) {
		for future, path := range edited {
			content := originals[path]
			if sha == future {
				content = append(append([]byte{}, content...), "\n// moved\n"...)
			}
			os.WriteFile(filepath.Join(tree, path), content, 0o644)
		}
		// Each checkout is a commit, as GitCheckout's is: a tree with changes to tracked files has no tree key.
		for _, arguments := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", sha}} {
			if output, err := exec.Command("git", append([]string{"-C", tree}, arguments...)...).CombinedOutput(); err != nil {
				return "", nil, fmt.Errorf("git %v: %v %s", arguments, err, output)
			}
		}
		return tree, func() {}, nil
	}
	client := QueueClient{Base: queue.URL, Token: "t"}
	if _, err := PullOnce(client, checkout, gateTools, tools, HTTPIndex{Client: client}, "", strings.Repeat("4", 64)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(posted["readme"], `"empty":true`) {
		t.Errorf("a Markdown edit nothing reads posted %s, want an empty plan", posted["readme"])
	}
	for _, future := range []string{"notes", "go"} {
		if !strings.HasPrefix(posted[future], "[") {
			t.Errorf("%s posted %s, want its plan's units", future, posted[future])
		}
	}
}

// localGo is the local go's release, as a planner pinned to GOTOOLCHAIN=local keys every unit on.
func localGo(t *testing.T) string {
	t.Helper()
	command := exec.Command("go", "env", "GOVERSION")
	command.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}

// A tree whose build's go isn't the release its units are keyed on isn't planned, naming both: the plan carries the
// release (keyParts.tools.go) to the builder, which refuses another. Mutant: the release not compared.
func TestATreeWhoseGoIsntItsUnitsIsNotPlanned(t *testing.T) {
	tree, _ := planFixture(t)
	results := []PlannedResult{{Name: "a", KeyParts: KeyParts{Kind: "test"}}}
	if err := carryTree(tree, "go1.0.0", results); err == nil || !strings.Contains(err.Error(), "keyed on go1.0.0") || results[0].Tree != "" {
		t.Fatalf("another release: %v, tree %q", err, results[0].Tree)
	}
	if err := carryTree(tree, localGo(t), results); err != nil || results[0].Tree == "" {
		t.Fatalf("its own release: %v, tree %q", err, results[0].Tree)
	}
}

// Unplan posts {by, reason} to the future's unplan route with the client's token, and a refusal comes back as an error
// naming Queue's answer (#r12yqbg).
func TestUnplanAsksQueueByTheFuturesRouteAndReportsARefusal(t *testing.T) {
	t.Parallel()
	var asked []string
	refuse := false
	queue := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		asked = append(asked, request.Method+" "+request.URL.Path+" "+request.Header.Get("Authorization")+" "+string(body))
		if refuse {
			http.Error(writer, `{"error":"future was decided green or red, so its plan stands"}`, http.StatusConflict)
		}
	}))
	defer queue.Close()
	client := QueueClient{Base: queue.URL, Token: "placer-token"}
	if err := client.Unplan("abc", "loom place", "not placed: stale"); err != nil {
		t.Fatal(err)
	}
	if want := `POST /futures/abc/unplan Bearer placer-token {"by":"loom place","reason":"not placed: stale"}`; len(asked) != 1 || asked[0] != want {
		t.Fatalf("asked %q, want %q", asked, want)
	}
	refuse = true
	if err := client.Unplan("abc", "loom place", "again"); err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "its plan stands") {
		t.Fatalf("a refusal: %v", err)
	}
}

// ChangeState reads a change's state and its future from Queue's record of it; a change Queue doesn't know, or an
// answer with no state, is an error, never read as gone (#drrnnkh).
func TestChangeStateReadsTheStateAndFutureAndNeverGuesses(t *testing.T) {
	t.Parallel()
	queue := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/changes/withdrawn":
			writer.Write([]byte(`{"state":"withdrawn","future":"aaaa","record":{}}`))
		case "/changes/unchecked":
			writer.Write([]byte(`{"state":"queued","future":null}`))
		case "/changes/blank":
			writer.Write([]byte(`{"future":"aaaa"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer queue.Close()
	client := QueueClient{Base: queue.URL, Token: "t"}
	if state, future, err := client.ChangeState("withdrawn"); err != nil || state != "withdrawn" || future != "aaaa" {
		t.Fatalf("withdrawn: %q %q %v", state, future, err)
	}
	if state, future, err := client.ChangeState("unchecked"); err != nil || state != "queued" || future != "" {
		t.Fatalf("unchecked: %q %q %v", state, future, err)
	}
	for _, change := range []string{"blank", "unknown"} {
		if state, _, err := client.ChangeState(change); err == nil {
			t.Errorf("%s: read as %q", change, state)
		}
	}
}
