package queuebridge

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The bridge's pass against a stand-in queue and gate: what it queues, posts and checks, and what it must not (ported
// from queue_bridge_test.py). Each mutant below must make a test here fail:
//
//	facts posted without the queue's seq, or with a seq that isn't a JSON number: TestMainsHeadRidesWithTheQueuesSeqReadBeforeGitOrTheChangeWaitsATick
//	the seq read after git: TestMainsHeadRidesWithTheQueuesSeqReadBeforeGitOrTheChangeWaitsATick
//	a tree queued every pass: TestAFutureWithNoRecordIsQueuedOnceAndDecidedByNothing
//	a parity run decided: TestAParityRunIsNeverSentThroughTodaysGate
//	a red excused when one failure isn't a ruled main red: TestARedThatIsOnlyMainsRuledRedIsExcusedAndAnyOtherFailureIsNot
//	a failure outside any test excusable: TestAFailureOutsideAnyTestMakesARecordUnexcusable
//	a first red the change's: TestAFirstRedIsServedAgainAndOnlyASecondRedIsTheChanges
//	a green posted before push-main's checks pass: TestNothingReadsGreenUntilPushMainsOwnChecksPass
//	a check that didn't run posted as a red: TestNothingReadsGreenUntilPushMainsOwnChecksPass
//	a lane decided again on the same tree, or never on a new one: TestNothingReadsGreenUntilPushMainsOwnChecksPass
//	decides = no still deciding any lane: TestOnceJudgeDecidesEveryFutureTheBridgeOnlyCarriesGitsFacts
//	a void served again twice: TestAVoidIsServedOnceMoreAndOnlyOnce
//	a gate merge's first parent not posted as gateMerge.base, or another tree's record taken: TestAGateMergeOfTheTreeIsItsFutureAndAnyOtherTreeIsVoid

var (
	tree   = strings.Repeat("1", 40)
	other  = strings.Repeat("2", 40)
	older  = strings.Repeat("a", 40)
	newer  = strings.Repeat("b", 40)
	change = "chg_" + strings.Repeat("c", 26)
)

type request struct {
	Method string
	Path   string
	Body   map[string]any
}

// A fakeQueue answers the bridge's reads from its fields and records every call, each body as the JSON the wire reads.
type fakeQueue struct {
	futures   []map[string]any
	unchecked []map[string]any
	paths     []string
	// head is GET /head's answer: status and body.
	head     func() (int, string)
	requests []request
}

func newQueue(futures ...map[string]any) *fakeQueue {
	return &fakeQueue{futures: futures, paths: []string{"a.go"}}
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
		return answer(map[string]any{"futures": append([]map[string]any{}, queue.futures...)})
	case method == "GET" && strings.HasPrefix(path, "/changes/"):
		return answer(map[string]any{"record": map[string]any{"paths": queue.paths}})
	case path == "/landings":
		return answer(map[string]any{"landings": []any{}})
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
		paths = append(paths, made.Path)
	}
	return paths
}

// verdicts are the posted verdicts' (status, cause) pairs, a nil cause as "".
func (queue *fakeQueue) verdicts() [][2]string {
	var pairs [][2]string
	for _, made := range queue.posts() {
		verdict := made.Body["verdict"].(map[string]any)
		cause, _ := verdict["cause"].(string)
		pairs = append(pairs, [2]string{verdict["status"].(string), cause})
	}
	return pairs
}

// A fakeGate is today's gate with no git: a fixed record, failures, parents and facts, and a check result.
type fakeGate struct {
	found       *Record
	failures    []string
	checkCode   int
	checkStderr string
	queued      []string
	requeued    []string
	checks      [][]string
	// asked records the queue's paths each time facts were read.
	asked   [][]string
	queue   *fakeQueue
	noHead  bool
	headSha string
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

func (gate *fakeGate) Record(tree string) (*Record, error) {
	return gate.found, nil
}

func (gate *fakeGate) Failing(ref string) ([]string, bool) {
	return gate.failures, true
}

func (gate *fakeGate) Parents(sha string) ([]string, error) {
	return map[string][]string{other: {older, tree}, strings.Repeat("3", 40): {older, strings.Repeat("4", 40)}}[sha], nil
}

func (gate *fakeGate) Queue(tree string) bool {
	gate.queued = append(gate.queued, tree)
	return true
}

func (gate *fakeGate) Requeue(sha string) bool {
	gate.requeued = append(gate.requeued, sha)
	return true
}

func (gate *fakeGate) Check(arguments []string, label string) (int, string, string) {
	gate.checks = append(gate.checks, arguments)
	if gate.checkCode == 0 && gate.checkStderr == "" {
		return 0, "checked", ""
	}
	return gate.checkCode, "", gate.checkStderr
}

func newMemory() *Memory {
	return &Memory{}
}

func future(tree string) map[string]any {
	return map[string]any{"future": tree, "tree": tree, "base": older, "changes": []string{change}}
}

func tick(queue *fakeQueue, gate *fakeGate, memory *Memory) {
	Bridge{Queue: queue, Gate: gate, Decides: true, Log: func(string) {}}.Tick(memory)
}

func submitted() []map[string]any {
	return []map[string]any{{"change": change, "sha": tree, "base": older, "paths": []string{"a.go"}}}
}

func wantFacts() map[string]any {
	return map[string]any{"shaExists": true, "baseIsAncestor": true, "baseOnMain": true, "diffPaths": []any{"a.go"}, "mainHead": newer, "asOf": 7.0}
}

func TestEveryUncheckedChangeGetsGitsFacts(t *testing.T) {
	queue := newQueue()
	queue.unchecked = submitted()
	tick(queue, &fakeGate{}, newMemory())
	if posts := queue.posts(); len(posts) != 1 || posts[0].Path != "/submissions/"+change+"/facts" || !reflect.DeepEqual(posts[0].Body, wantFacts()) {
		t.Fatalf("posted %+v", posts)
	}
}

func TestMainsHeadRidesWithTheQueuesSeqReadBeforeGitOrTheChangeWaitsATick(t *testing.T) {
	queue := newQueue()
	queue.unchecked = submitted()
	gate := &fakeGate{queue: queue}
	tick(queue, gate, newMemory())
	if !reflect.DeepEqual(gate.asked, [][]string{{"/submissions?state=unchecked", "/head"}}) {
		t.Fatalf("git was asked after %q", gate.asked)
	}
	if posts := queue.posts(); len(posts) != 1 || !reflect.DeepEqual(posts[0].Body, wantFacts()) {
		t.Fatalf("posted %+v", posts)
	}
	// No seq to order it by, or no head: nothing is posted, so the change stays unchecked and is read again next tick.
	for _, answer := range []struct {
		status int
		body   string
		noHead bool
	}{{503, `{"error":"down"}`, false}, {429, `{"error":"slow"}`, false}, {200, `{"seq":"7"}`, false}, {200, `{"seq":true}`, false},
		{200, `{"seq":-1}`, false}, {200, `{"seq":7.5}`, false}, {200, `not json`, false}, {200, `{"seq":7}`, true}} {
		queue := newQueue()
		queue.unchecked = submitted()
		queue.head = func() (int, string) { return answer.status, answer.body }
		tick(queue, &fakeGate{noHead: answer.noHead}, newMemory())
		if posts := queue.posts(); len(posts) != 0 {
			t.Errorf("%+v: posted %+v", answer, posts)
		}
	}
}

func TestAFutureWithNoRecordIsQueuedOnceAndDecidedByNothing(t *testing.T) {
	queue, gate, memory := newQueue(future(tree)), &fakeGate{}, newMemory()
	tick(queue, gate, memory)
	tick(queue, gate, memory)
	if !reflect.DeepEqual(gate.queued, []string{tree}) || len(queue.posts()) != 0 {
		t.Fatalf("queued %q, posted %+v", gate.queued, queue.posts())
	}
}

func TestARecordOfTheTreeIsItsVerdictPostedOnce(t *testing.T) {
	for _, each := range []struct{ status, verdict, cause string }{{"green", "passed", ""}, {"red", "void", "flake"}, {"void", "void", "infra"}} {
		queue, memory := newQueue(future(tree)), newMemory()
		gate := &fakeGate{found: &Record{Ref: "gate-logs/r/fast", Status: each.status, Gated: tree}}
		tick(queue, gate, memory)
		tick(queue, gate, memory)
		var cause any
		if each.cause != "" {
			cause = each.cause
		}
		want := map[string]any{"change": change, "verdict": map[string]any{"future": tree, "run": "gate-logs/r/fast", "status": each.verdict, "cause": cause, "rule": "todays-gate-v0"}}
		if posts := queue.posts(); len(posts) != 1 || posts[0].Path != "/verdicts" || !reflect.DeepEqual(posts[0].Body, want) {
			t.Errorf("%s: posted %+v", each.status, posts)
		}
	}
}

func TestAParityRunIsNeverSentThroughTodaysGate(t *testing.T) {
	parity := future(tree)
	parity["parity"] = true
	queue, gate := newQueue(parity), &fakeGate{found: &Record{Ref: "gate-logs/r/fast", Status: "green", Gated: tree}}
	tick(queue, gate, newMemory())
	if len(gate.queued) != 0 || len(queue.posts()) != 0 || len(gate.checks) != 0 {
		t.Fatalf("queued %q, checked %q, posted %+v", gate.queued, gate.checks, queue.posts())
	}
}

func TestARedThatIsOnlyMainsRuledRedIsExcusedAndAnyOtherFailureIsNot(t *testing.T) {
	queue := newQueue(future(tree))
	gate := &fakeGate{found: &Record{Ref: "gate-logs/r/fast", Status: "red", Gated: tree}, failures: []string{"stage1/cohere/gitignore TestThePortAnswersAsGoCohereAndGitDo_022"}}
	tick(queue, gate, newMemory())
	if got := queue.verdicts(); !reflect.DeepEqual(got, [][2]string{{"failed", "mainRed"}}) {
		t.Fatalf("only main's ruled red: %q", got)
	}
	queue = newQueue(future(tree))
	gate.failures = append(gate.failures, "stage1/cohere/gitignore TestPatterns")
	tick(queue, gate, newMemory())
	if got := queue.verdicts(); !reflect.DeepEqual(got, [][2]string{{"void", "flake"}}) {
		t.Fatalf("with another failure: %q", got)
	}
	if got := excusedNames([]string{"stage1/cohere/gitignore TestThePortAnswersAsGoCohereAndGitDo_026"}); !reflect.DeepEqual(got,
		[]string{"stage1/cohere/gitignore TestThePortAnswersAsGoCohereAndGitDo_026=" + mainReds[0].Ruling}) {
		t.Fatalf("excused %q", got)
	}
	for _, failing := range [][]string{{"stage1/cohere/estree TestThePortAnswersAsGoCohereAndGitDo_026"}, {}, nil} {
		if got := excusedNames(failing); got != nil {
			t.Errorf("%q excused as %q", failing, got)
		}
	}
}

func TestAFailureOutsideAnyTestMakesARecordUnexcusable(t *testing.T) {
	event := func(action, test, packagePath string) string {
		fields := map[string]any{"Action": action, "Package": "github.com/system-inc/adamic/" + packagePath}
		if test != "" {
			fields["Test"] = test
		}
		encoded, _ := json.Marshal(fields)
		return string(encoded)
	}
	shard := "TestThePortAnswersAsGoCohereAndGitDo_022"
	clean := []string{event("run", shard, "stage1/cohere/gitignore"), event("fail", shard, "stage1/cohere/gitignore")}
	stages := func(build float64) map[string]any {
		return map[string]any{"stages_exit": map[string]any{"build": build, "tests": 1.0}}
	}
	if names, ok := failingTests(clean, stages(0)); !ok || !reflect.DeepEqual(names, []string{"stage1/cohere/gitignore " + shard}) {
		t.Fatalf("clean: %q, %v", names, ok)
	}
	for name, each := range map[string]struct {
		lines []string
		fast  map[string]any
	}{
		// A package-level fail (a build break, a binary dying outside any test) beside the ruled red.
		"a fail with no test":  {append(slices.Clone(clean), event("fail", "", "internal/lower")), map[string]any{}},
		"a test never ended":   {append(slices.Clone(clean), event("run", "TestOther", "internal/lower")), map[string]any{}},
		"a stage that failed":  {clean, stages(2)},
		"a stage with no code": {clean, map[string]any{"stages_exit": map[string]any{"build": nil}}},
		"no test record":       {nil, map[string]any{}},
	} {
		if names, ok := failingTests(each.lines, each.fast); ok {
			t.Errorf("%s: readable as %q", name, names)
		}
	}
}

func TestAFirstRedIsServedAgainAndOnlyASecondRedIsTheChanges(t *testing.T) {
	queue, memory := newQueue(future(tree)), newMemory()
	gate := &fakeGate{found: &Record{Ref: "gate-logs/r1/fast", Status: "red", Gated: tree}}
	tick(queue, gate, memory)
	gate.found = &Record{Ref: "gate-logs/r2/fast", Status: "red", Gated: tree}
	tick(queue, gate, memory)
	if !reflect.DeepEqual(gate.requeued, []string{tree}) || !reflect.DeepEqual(queue.verdicts(), [][2]string{{"void", "flake"}, {"failed", "change"}}) {
		t.Fatalf("requeued %q, verdicts %q", gate.requeued, queue.verdicts())
	}
}

func TestAMarkdownOnlyChangeTakesTheRuledGate(t *testing.T) {
	queue, gate, memory := newQueue(future(tree)), &fakeGate{}, newMemory()
	queue.paths = []string{"docs/front-door.md"}
	tick(queue, gate, memory)
	tick(queue, gate, memory)
	want := map[string]any{"change": change, "verdict": map[string]any{"future": tree, "run": "ruled-gate:docs", "status": "passed", "cause": nil, "rule": "ruled-gate-docs-v0"}}
	if posts := queue.posts(); len(gate.queued) != 0 || len(posts) != 1 || !reflect.DeepEqual(posts[0].Body, want) {
		t.Fatalf("queued %q, posted %+v", gate.queued, posts)
	}
	if !reflect.DeepEqual(gate.checks, [][]string{{"--ruled-gate", docsRuling, tree, "0", "0", "0", "0"}}) {
		t.Fatalf("checked %q", gate.checks)
	}
	// Code in the change keeps it on today's fast gate.
	queue, gate = newQueue(future(tree)), &fakeGate{}
	queue.paths = []string{"docs/front-door.md", "cmd/x/main.go"}
	tick(queue, gate, newMemory())
	if !reflect.DeepEqual(gate.queued, []string{tree}) {
		t.Fatalf("with code: queued %q", gate.queued)
	}
}

func TestATestOnlyChangeTakesTheTestOnlyLane(t *testing.T) {
	queue, gate := newQueue(future(tree)), &fakeGate{}
	queue.paths = []string{"internal/oracle/a_test.go", "internal/oracle/testdata/x.a"}
	tick(queue, gate, newMemory())
	if posts := queue.posts(); len(gate.queued) != 0 || len(posts) != 1 || posts[0].Body["verdict"].(map[string]any)["run"] != "test-only" {
		t.Fatalf("queued %q, posted %+v", gate.queued, posts)
	}
	queue, gate = newQueue(future(tree)), &fakeGate{}
	queue.paths = []string{"internal/oracle/a_test.go", "internal/oracle/a.go"}
	tick(queue, gate, newMemory())
	if !reflect.DeepEqual(gate.queued, []string{tree}) {
		t.Fatalf("with code: queued %q", gate.queued)
	}
	// push-main's lane checks refuse a test that isn't gofmt'd: the change's red, its reason in the run.
	queue, gate = newQueue(future(tree)), &fakeGate{checkCode: 1, checkStderr: "refused: internal/oracle/a_test.go isn't gofmt-formatted\n"}
	queue.paths = []string{"internal/oracle/a_test.go"}
	tick(queue, gate, newMemory())
	if !reflect.DeepEqual(gate.checks, [][]string{{"--test-only", tree}}) || !reflect.DeepEqual(queue.verdicts(), [][2]string{{"failed", "change"}}) ||
		!strings.Contains(queue.posts()[0].Body["verdict"].(map[string]any)["run"].(string), "gofmt") {
		t.Fatalf("checked %q, posted %+v", gate.checks, queue.posts())
	}
}

func TestNothingReadsGreenUntilPushMainsOwnChecksPass(t *testing.T) {
	// A green record that push-main refuses (zerorun: a unit ran none of its tests) is a void, never a pass.
	queue := newQueue(future(tree))
	gate := &fakeGate{found: &Record{Ref: "gate-logs/r/fast", Status: "green", Gated: tree}, checkCode: 1,
		checkStderr: "refused: gate-logs/r/fast has units that ran none of the tests they named"}
	tick(queue, gate, newMemory())
	verdict := queue.posts()[0].Body["verdict"].(map[string]any)
	if !reflect.DeepEqual(queue.verdicts(), [][2]string{{"void", "infra"}}) || !strings.Contains(verdict["rule"].(string), "ran none of the tests") ||
		!reflect.DeepEqual(gate.checks, [][]string{{"--fast-gate", "gate-logs/r/fast", tree}}) {
		t.Fatalf("checked %q, posted %+v", gate.checks, verdict)
	}
	// main's ruled red goes to push-main by name, which rechecks its output.
	queue = newQueue(future(tree))
	gate = &fakeGate{found: &Record{Ref: "gate-logs/r/fast", Status: "red", Gated: tree}, failures: []string{"stage1/cohere/gitignore TestThePortAnswersAsGoCohereAndGitDo_022"}}
	tick(queue, gate, newMemory())
	if !reflect.DeepEqual(gate.checks[0][:3], []string{"--fast-gate", "gate-logs/r/fast", "--infra-red"}) || !reflect.DeepEqual(queue.verdicts(), [][2]string{{"failed", "mainRed"}}) {
		t.Fatalf("checked %q, posted %q", gate.checks, queue.verdicts())
	}
	// A docs change takes the ruled gate's census; a hold posts nothing.
	queue, gate = newQueue(future(tree)), &fakeGate{checkCode: 3, checkStderr: "held"}
	queue.paths = []string{"docs/a.md"}
	tick(queue, gate, newMemory())
	if gate.checks[0][0] != "--ruled-gate" || len(queue.posts()) != 0 {
		t.Fatalf("on a hold: checked %q, posted %+v", gate.checks, queue.posts())
	}
	// A check that didn't run (git's lock race in the shared checkout) posts nothing and runs again; only push-main's
	// refusal is the change's red.
	queue, memory := newQueue(future(tree)), newMemory()
	queue.paths = []string{"docs/a.md"}
	gate = &fakeGate{checkCode: 1, checkStderr: "error: cannot lock ref 'refs/remotes/origin/x': is at 1 but expected 2"}
	tick(queue, gate, memory)
	if len(queue.posts()) != 0 || len(memory.Ruled) != 0 {
		t.Fatalf("a check that didn't run: posted %+v, ruled %q", queue.posts(), memory.Ruled)
	}
	gate.checkStderr = "refused: not test-only against main 0bf6186d: cmd/x/main.go"
	tick(queue, gate, memory)
	if !reflect.DeepEqual(queue.verdicts(), [][2]string{{"failed", "change"}}) {
		t.Fatalf("a refusal: %q", queue.verdicts())
	}
	// Decided once per tree: the same tree again posts nothing, and the change resubmitted on a new tree is decided.
	gate.checkCode, gate.checkStderr = 0, ""
	tick(queue, gate, memory)
	if len(queue.posts()) != 1 {
		t.Fatalf("the same tree again: posted %+v", queue.posts())
	}
	queue.futures = []map[string]any{future(other)}
	tick(queue, gate, memory)
	var futures []any
	for _, made := range queue.posts() {
		futures = append(futures, made.Body["verdict"].(map[string]any)["future"])
	}
	if !reflect.DeepEqual(futures, []any{tree, other}) {
		t.Fatalf("decided %q", futures)
	}
}

// The bridge never lands: the pusher on Workshop holds main.
func TestTheBridgeNeverReadsALandingOrder(t *testing.T) {
	queue := newQueue(future(tree))
	queue.unchecked = submitted()
	tick(queue, &fakeGate{found: &Record{Ref: "gate-logs/r/fast", Status: "green", Gated: tree}}, newMemory())
	if slices.Contains(queue.asked(), "/landings") {
		t.Fatalf("asked %q", queue.asked())
	}
}

func TestOnceJudgeDecidesEveryFutureTheBridgeOnlyCarriesGitsFacts(t *testing.T) {
	// Every lane today's gate decides: the fast gate (a record, or none yet to queue), docs and test-only.
	for _, each := range []struct {
		paths  []string
		record *Record
	}{{[]string{"a.go"}, &Record{Ref: "gate-logs/r/fast", Status: "green", Gated: tree}}, {[]string{"a.go"}, nil},
		{[]string{"docs/a.md"}, nil}, {[]string{"internal/x/a_test.go"}, nil}} {
		queue, gate := newQueue(future(tree)), &fakeGate{found: each.record}
		queue.unchecked, queue.paths = submitted(), each.paths
		Bridge{Queue: queue, Gate: gate, Decides: false, Log: func(string) {}}.Tick(newMemory())
		if posts := queue.posts(); len(posts) != 1 || posts[0].Path != "/submissions/"+change+"/facts" {
			t.Errorf("%q: posted %+v", each.paths, posts)
		}
		if len(gate.queued)+len(gate.checks)+len(gate.requeued) != 0 || slices.Contains(queue.asked(), "/futures?state=unplanned") {
			t.Errorf("%q: queued %q, checked %q, requeued %q, asked %q", each.paths, gate.queued, gate.checks, gate.requeued, queue.asked())
		}
	}
}

func TestAVoidIsServedOnceMoreAndOnlyOnce(t *testing.T) {
	queue, memory := newQueue(future(tree)), newMemory()
	gate := &fakeGate{found: &Record{Ref: "gate-logs/r1/fast", Status: "void", Gated: tree}}
	tick(queue, gate, memory)
	gate.found = &Record{Ref: "gate-logs/r2/fast", Status: "void", Gated: tree}
	tick(queue, gate, memory)
	if !reflect.DeepEqual(gate.requeued, []string{tree}) || len(queue.posts()) != 2 {
		t.Fatalf("requeued %q, posted %+v", gate.requeued, queue.posts())
	}
}

func TestAGateMergeOfTheTreeIsItsFutureAndAnyOtherTreeIsVoid(t *testing.T) {
	queue := newQueue(future(tree))
	tick(queue, &fakeGate{found: &Record{Ref: "gate-logs/r/fast", Status: "green", Gated: other}}, newMemory())
	want := map[string]any{"change": change, "gateMerge": map[string]any{"base": older}, "verdict": map[string]any{
		"future": other, "run": "gate-logs/r/fast", "status": "passed", "cause": nil, "rule": "todays-gate-v0"}}
	if posts := queue.posts(); len(posts) != 1 || !reflect.DeepEqual(posts[0].Body, want) {
		t.Fatalf("posted %+v", posts)
	}
	queue = newQueue(future(tree))
	tick(queue, &fakeGate{found: &Record{Ref: "gate-logs/r/fast", Status: "green", Gated: strings.Repeat("3", 40)}}, newMemory())
	if posts := queue.posts(); len(posts) != 1 || posts[0].Body["verdict"].(map[string]any)["future"] != tree || !reflect.DeepEqual(queue.verdicts(), [][2]string{{"void", "infra"}}) {
		t.Fatalf("another tree: posted %+v", posts)
	}
}
