// Package queuebridge is the queue's bridge to git and to today's gate (#jtdwm5n, #vwsgnxm), until the planner and the
// judge decide every future. `loom queue-bridge` runs one pass a minute on the machine whose ~/.loom/queue-bridge.conf
// makes it the bridge:
//
//  1. Every submitted change still unchecked gets git's facts from this machine's clone (the sha exists, its base is
//     its ancestor and on main, the paths of base..sha, the history beyond its diff, the Python tests gate logic names,
//     the main commit it reverts, and main's head with the queue's seq read before git), since no GitHub credential
//     lives in Cloudflare. The queue decides on them.
//  2. While the bridge decides (decides = yes, the default until slice 2), every unplanned future goes through today's
//     gate. A change that touches only Markdown takes the docs ruling and one that touches only test paths the
//     test-only lane, each posted passed once push-main's own checks for the lane pass in check-only mode. Any other
//     future takes today's fast gate exactly as a cut does: a cloud/land-queue-<tree8> branch at the tree, which
//     fast-gate-watch gates like any cloud/land-* tip, and its newest finished record (gate-logs/<tree12>/<stamp>/fast)
//     is the verdict input. Green is passed once push-main --fast-gate passes it in check-only mode, void is void, and
//     red follows the contract's judge stub, "any second failure is the change": a first red is posted void and
//     served again (requeue.sh), and only a red after that is failed with cause change. A red whose every failing test
//     is a main red the judge ruled is failed with cause mainRed. Main moves often, so the gate usually tests the change
//     merged onto a newer main (a gate merge, second parent the change's sha): that merge becomes the change's future,
//     posted with its first parent as gateMerge.base. A record of any other tree is void for this future.
//
// With decides = no, once Judge decides every future and outside verdicts are refused (#xvvf6cn), the bridge only
// carries git's facts. It never lands: the pusher on Workshop holds main (package lander). The queue decides; this only
// carries, and no credential that moves main lives in Cloudflare.
package queuebridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/system-inc/loom/protocol"
)

// Rule names today's fast gate as a verdict's rule; DocsRule and TestOnlyRule name the two ruled lanes.
const (
	Rule         = "todays-gate-v0"
	DocsRule     = "ruled-gate-docs-v0"
	TestOnlyRule = "test-only-lane-v0"
	docsRuling   = "docs only: Markdown no product test reads (Loom, Oct 10 00:18Z)"
)

// A Queue is the Queue's coordinator seam as the bridge calls it (wire/source/Queue.ts): a method, a path and a body
// (nil for none), answered with the status and the body's bytes. An error is a call that never got an answer.
type Queue interface {
	Call(method, path string, body any) (int, []byte, error)
}

// HTTPQueue calls the Queue at Base with a coordinator token minted from Secret for each call, good for ten minutes,
// the way the wire verifies it (wire/source/Token.ts).
type HTTPQueue struct {
	Base   string
	Secret []byte
	HTTP   *http.Client
}

func (queue HTTPQueue) Call(method, path string, body any) (int, []byte, error) {
	token, err := protocol.MintToken(queue.Secret, protocol.TokenClaims{Run: "queue-bridge", Scope: protocol.ScopeCoordinator, Expires: time.Now().Add(10 * time.Minute).Unix()})
	if err != nil {
		return 0, nil, err
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, strings.TrimSuffix(queue.Base, "/")+path, reader)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "loom-queue-bridge")
	client := queue.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, answer, nil
}

// call is Call with a transport error read as status 0, its text the answer, so every caller logs one shape.
func call(queue Queue, method, path string, body any) (int, []byte) {
	status, answer, err := queue.Call(method, path, body)
	if err != nil {
		return 0, []byte(fmt.Sprintf("%s %s: %v", method, path, err))
	}
	return status, answer
}

// answerText is an answer as one log line, cut at 300 bytes.
func answerText(answer []byte) string {
	return first(strings.Join(strings.Fields(string(answer)), " "), 300)
}

// first is text cut to at most limit bytes, never inside a character.
func first(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

// A Record is a fast gate's newest finished record of a tree: its ref, its status (green, red or void) and the sha it
// gated, which is the tree or a gate merge of it.
type Record struct {
	Ref    string
	Status string
	Gated  string
}

// A Gate is today's gate as the bridge sees it: git's facts, the records on origin, the branch that queues a tree and
// push-main in check-only mode. An error is git unable to say, and nothing is decided from it this pass.
type Gate interface {
	// Facts are what git says about a submitted change, mainHead among them.
	Facts(sha, base string) (map[string]any, error)
	// Record is tree's newest finished fast record, or nil while none has finished.
	Record(tree string) (*Record, error)
	// Failing names a record's failing top-level tests, "<package under the module> <Test>", and false when a
	// failure could hide outside them (failingTests).
	Failing(ref string) ([]string, bool)
	// Parents are sha's parents, none when origin lacks it.
	Parents(sha string) ([]string, error)
	// Queue puts tree in front of fast-gate-watch as a cloud/land-* tip, true once the branch is there.
	Queue(tree string) bool
	// Requeue serves sha's fast job again, true when it started.
	Requeue(sha string) bool
	// Check runs push-main in check-only mode, every check it runs and nothing pushed: its exit code, stdout and stderr.
	Check(arguments []string, label string) (int, string, string)
}

// Memory is what the bridge has done or said, kept between passes in memory.json: trees it queued, records it posted
// a verdict on ("<change> <ref>"), trees it served again, and lane decisions it posted ("<change> <tree>").
type Memory struct {
	Queued   []string `json:"queued"`
	Posted   []string `json:"posted"`
	Requeued []string `json:"requeued"`
	Ruled    []string `json:"ruled"`
}

// A Bridge is one pass's hands: the queue, the gate, whether it decides, and where it logs.
type Bridge struct {
	Queue   Queue
	Gate    Gate
	Decides bool
	Log     func(string)
}

func (bridge Bridge) log(format string, arguments ...any) {
	bridge.Log(fmt.Sprintf(format, arguments...))
}

// Tick is one pass: git's facts for every unchecked change, then, while the bridge decides, a verdict for every
// unplanned future.
func (bridge Bridge) Tick(memory *Memory) {
	status, answer := call(bridge.Queue, "GET", "/submissions?state=unchecked", nil)
	var unchecked struct {
		Changes []struct {
			Change string `json:"change"`
			Sha    string `json:"sha"`
			Base   string `json:"base"`
		} `json:"changes"`
	}
	if status != 200 || json.Unmarshal(answer, &unchecked) != nil {
		bridge.log("submissions: %d %s", status, answerText(answer))
		return
	}
	for _, submitted := range unchecked.Changes {
		facts := bridge.withHead(func() (map[string]any, error) { return bridge.Gate.Facts(submitted.Sha, submitted.Base) })
		if facts == nil {
			bridge.log("facts for %s wait for the next tick", submitted.Change)
			continue
		}
		status, answer := call(bridge.Queue, "POST", "/submissions/"+submitted.Change+"/facts", facts)
		bridge.log("facts for %s: %d %s", submitted.Change, status, answerText(answer))
	}
	// Once outside verdicts are refused, Judge decides every future and this only carries git's facts (#hkmzefm).
	if !bridge.Decides {
		return
	}
	if err := bridge.decide(memory); err != nil {
		bridge.log("deciding stops for this tick, git can't say: %v", err)
	}
}

// withHead is git's facts, read after the queue's seq is noted: asOf orders their mainHead against the queue's own log,
// so a landing logged while git answered outranks the head it read. Facts that can't say main's head (the queue's seq
// or origin's main unreadable) are nil: posted, they'd check the change for good, and a witness checked without its
// head never records main.green or main.red, so the change waits for the next tick instead (#6gj7n9p).
func (bridge Bridge) withHead(read func() (map[string]any, error)) map[string]any {
	status, answer := call(bridge.Queue, "GET", "/head", nil)
	var head map[string]any
	seq := -1.0
	if status == 200 && json.Unmarshal(answer, &head) == nil {
		// A whole JSON number only: "7", true or 7.5 is no seq, and a negative one is refused below.
		if number, ok := head["seq"].(float64); ok && number == float64(int64(number)) {
			seq = number
		}
	}
	if seq < 0 {
		bridge.log("the queue's seq is unreadable (%d %s): no facts posted this tick", status, first(answerText(answer), 200))
		return nil
	}
	facts, err := read()
	if err != nil {
		bridge.log("git can't say: %v", err)
		return nil
	}
	if _, ok := facts["mainHead"]; !ok {
		bridge.log("origin/main is unreadable: no facts posted this tick")
		return nil
	}
	withSeq := map[string]any{"asOf": int64(seq)}
	for key, value := range facts {
		withSeq[key] = value
	}
	return withSeq
}

// mainRed is one of main's own reds the judge has ruled, by package and test pattern with its ruling.
type mainRed struct {
	Package string
	Test    *regexp.Regexp
	Ruling  string
}

// mainReds are main's reds the judge ruled. A red record whose every failing test is one of these is main's red, not the
// change's; push-main --infra-red checks each by name on the output again (a test-owned time limit, no assertion), so a
// ruled name can't hide a real failure of the same test.
var mainReds = []mainRed{
	{"stage1/cohere/gitignore", regexp.MustCompile(`^TestThePortAnswersAsGoCohereAndGitDo_\d+$`),
		"main's red: its run time at its own 90 s hard deadline, a deadline at its edge (Judge, Oct 10 00:33Z)"},
}

// excusedNames are the --infra-red names for failing ("<package> <Test>") when every one is a ruled main red, else nil.
func excusedNames(failing []string) []string {
	var names []string
	for _, name := range failing {
		packagePath, test, _ := strings.Cut(name, " ")
		ruling := ""
		for _, red := range mainReds {
			if red.Package == packagePath && red.Test.MatchString(test) {
				ruling = red.Ruling
				break
			}
		}
		if ruling == "" {
			return nil
		}
		names = append(names, fmt.Sprintf("%s %s=%s", packagePath, test, ruling))
	}
	return names
}

// infraRed is push-main's --infra-red arguments for a record's failures the judge ruled main's, none otherwise.
func (bridge Bridge) infraRed(ref string) []string {
	failing, _ := bridge.Gate.Failing(ref)
	var arguments []string
	for _, name := range excusedNames(failing) {
		arguments = append(arguments, "--infra-red", name)
	}
	return arguments
}

// failingTests are the failing top-level tests in a record's test events, and false when a failure could hide outside
// them (Loom's guard, 00:34Z): a fail with no test (a build failure, a binary dying outside any test), a test that
// started and never ended, a stage other than tests that didn't pass, or no test record at all (nil lines). False is
// never excusable.
func failingTests(lines []string, fast map[string]any) ([]string, bool) {
	if lines == nil {
		return nil, false
	}
	stages, _ := fast["stages_exit"].(map[string]any)
	for stage, code := range stages {
		if number, ok := code.(float64); stage != "tests" && (!ok || number != 0) {
			return nil, false
		}
	}
	type key struct{ packagePath, test string }
	names, started, ended := map[string]bool{}, map[key]bool{}, map[key]bool{}
	for _, line := range lines {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		action, _ := event["Action"].(string)
		test, _ := event["Test"].(string)
		packagePath, _ := event["Package"].(string)
		if action == "fail" && test == "" {
			return nil, false
		}
		if test != "" && action == "run" {
			started[key{packagePath, test}] = true
		}
		if test != "" && (action == "pass" || action == "fail" || action == "skip") {
			ended[key{packagePath, test}] = true
		}
		if action == "fail" && test != "" {
			parts := strings.Split(packagePath, "/adamic/")
			top, _, _ := strings.Cut(test, "/")
			names[parts[len(parts)-1]+" "+top] = true
		}
	}
	for begun := range started {
		if !ended[begun] {
			return nil, false
		}
	}
	sorted := []string{}
	for name := range names {
		sorted = append(sorted, name)
	}
	slices.Sort(sorted)
	return sorted, true
}

// docsOnly is whether a change touches only Markdown, which the ruled gate lands with its census and no product suite.
func docsOnly(paths []string) bool {
	return len(paths) > 0 && !slices.ContainsFunc(paths, func(path string) bool { return !strings.HasSuffix(path, ".md") })
}

// testOnlyPattern is push-main.sh's testOnlyPattern, the same list by ruling (Kirk, Oct 8); push-main checks it again on
// the merged tree.
var testOnlyPattern = regexp.MustCompile(`(_test\.go$|_test\.py$|-test\.py$|(^|/)test_[^/]*\.py$|/testdata/|^review/|(^|/)shards\.json$|^stage3/fixtures/|^stage3/meter/|^README\.md$)`)

// testOnly is whether a change touches only test paths, which the test-only lane lands with no gate in front of it.
func testOnly(paths []string) bool {
	return len(paths) > 0 && !slices.ContainsFunc(paths, func(path string) bool { return !testOnlyPattern.MatchString(path) })
}

// verdictOf is the body today's record gives the change at tree: its whole verdict, and gateMerge when it gated a merge
// of tree. served says the tree was already served again once, so a red now is the change's.
func (bridge Bridge) verdictOf(record Record, tree string, served bool) (map[string]any, error) {
	var merge map[string]any
	if record.Gated != tree {
		parents, err := bridge.Gate.Parents(record.Gated)
		if err != nil {
			return nil, err
		}
		if len(parents) != 2 || parents[1] != tree {
			return map[string]any{"verdict": map[string]any{"future": tree, "run": record.Ref, "status": "void", "cause": "infra", "rule": Rule}}, nil
		}
		merge = map[string]any{"base": parents[0]}
	}
	var status string
	var cause any
	switch record.Status {
	case "green":
		status, cause = "passed", nil
	case "red":
		status, cause = "void", "flake"
		if served {
			status, cause = "failed", "change"
		}
		// Every failure is a red the judge ruled main's: excused, as the judge's Green excuses it.
		if failing, _ := bridge.Gate.Failing(record.Ref); excusedNames(failing) != nil {
			status, cause = "failed", "mainRed"
		}
	default:
		status, cause = "void", "infra"
	}
	body := map[string]any{"verdict": map[string]any{"future": record.Gated, "run": record.Ref, "status": status, "cause": cause, "rule": Rule}}
	if merge != nil {
		body["gateMerge"] = merge
	}
	return body, nil
}

// lines are text's lines, with no empty last one for a closing newline.
func lines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// checked is push-main's whole judgment on a candidate, nothing pushed: its exit code and its reason. 3 is a hold.
func (bridge Bridge) checked(arguments []string, change string) (int, string) {
	code, stdout, stderr := bridge.Gate.Check(arguments, "queue "+change)
	reason := fmt.Sprintf("exit %d", code)
	if refusal := slices.IndexFunc(lines(stderr), func(line string) bool { return strings.HasPrefix(line, "refused") }); refusal >= 0 {
		reason = lines(stderr)[refusal]
	} else if said := lines(stderr); len(said) > 0 {
		reason = said[len(said)-1]
	} else if said := lines(stdout); len(said) > 0 {
		reason = said[len(said)-1]
	}
	shown := ""
	if code != 0 {
		shown = first(reason, 200)
	}
	bridge.log("push-main checked %s (%s): exit %d %s", change, strings.Join(arguments[:min(2, len(arguments))], " "), code, shown)
	return code, reason
}

// decide is today's gate's verdict for every unplanned future: the docs and test-only lanes by push-main's checks, the
// rest by a fast record. An error is git unable to say, which stops deciding for this pass.
func (bridge Bridge) decide(memory *Memory) error {
	status, answer := call(bridge.Queue, "GET", "/futures?state=unplanned", nil)
	var listed struct {
		Futures []struct {
			Tree    string   `json:"tree"`
			Changes []string `json:"changes"`
			Parity  bool     `json:"parity"`
		} `json:"futures"`
	}
	if status != 200 || json.Unmarshal(answer, &listed) != nil {
		bridge.log("futures: %d %s", status, answerText(answer))
		return nil
	}
	for _, future := range listed.Futures {
		// A parity run is Release's proof of the new path against a box record: today's gate never decides it.
		if future.Parity || len(future.Changes) == 0 {
			continue
		}
		tree, change := future.Tree, future.Changes[0]
		status, answer := call(bridge.Queue, "GET", "/changes/"+change, nil)
		var read struct {
			Record struct {
				Paths []string `json:"paths"`
			} `json:"record"`
		}
		lane, laneRule := "", ""
		if status == 200 && json.Unmarshal(answer, &read) == nil {
			if docsOnly(read.Record.Paths) {
				lane, laneRule = "ruled-gate:docs", DocsRule
			} else if testOnly(read.Record.Paths) {
				lane, laneRule = "test-only", TestOnlyRule
			}
		}
		if lane != "" {
			// Keyed by the tree too: a resubmitted change is a new tree, decided again.
			if slices.Contains(memory.Ruled, change+" "+tree) {
				continue
			}
			verdict := map[string]any{"future": tree, "run": lane, "status": "passed", "cause": nil, "rule": laneRule}
			// The pusher only fast-forwards, so push-main's own checks for the lane (the ruled gate's census on the
			// landing tree, or the test-only lane's checks) run here, before anything reads green (Loom, 00:54Z).
			arguments := []string{"--test-only", tree}
			if lane == "ruled-gate:docs" {
				arguments = []string{"--ruled-gate", docsRuling, tree, "0", "0", "0", "0"}
			}
			code, why := bridge.checked(arguments, change)
			if code == 3 {
				continue
			}
			// Only push-main's own refusal is the change's. Anything else (a git lock race in the shared checkout, a
			// fetch that failed) is the check not running, so it runs again next tick, never a red.
			if code != 0 && !strings.HasPrefix(why, "refused") {
				bridge.log("push-main couldn't check %s (exit %d), trying again: %s", change, code, first(why, 300))
				continue
			}
			if code != 0 {
				verdict["status"], verdict["cause"] = "failed", "change"
				verdict["run"] = fmt.Sprintf("%s refused by push-main's checks: %s", lane, first(why, 300))
			}
			status, answer := call(bridge.Queue, "POST", "/verdicts", map[string]any{"change": change, "verdict": verdict})
			bridge.log("verdict %s %s under %s: %d %s", change, verdict["status"], laneRule, status, answerText(answer))
			if status == 200 || status == 409 {
				memory.Ruled = append(memory.Ruled, change+" "+tree)
			}
			continue
		}
		record, err := bridge.Gate.Record(tree)
		if err != nil {
			return err
		}
		if record == nil {
			if !slices.Contains(memory.Queued, tree) && bridge.Gate.Queue(tree) {
				memory.Queued = append(memory.Queued, tree)
				bridge.log("queued %s (%s) for today's fast gate", tree[:min(12, len(tree))], change)
			}
			continue
		}
		key := change + " " + record.Ref
		if slices.Contains(memory.Posted, key) {
			continue
		}
		body, err := bridge.verdictOf(*record, tree, slices.Contains(memory.Requeued, tree))
		if err != nil {
			return err
		}
		body["change"] = change
		verdict := body["verdict"].(map[string]any)
		// Nothing reads green, or main's red, on today's record alone: push-main's checks on it (zerorun on the record's
		// job, --infra-red's recheck of each ruled red on its output, the pause rule) run first. A refusal is a void with
		// push-main's reason in its rule, never a pass.
		if verdict["status"] == "passed" || verdict["cause"] == "mainRed" {
			arguments := append(append([]string{"--fast-gate", record.Ref}, bridge.infraRed(record.Ref)...), verdict["future"].(string))
			code, why := bridge.checked(arguments, change)
			if code == 3 {
				continue
			}
			if code != 0 {
				verdict["status"], verdict["cause"] = "void", "infra"
				verdict["rule"] = fmt.Sprintf("%s; push-main refused: %s", Rule, first(why, 300))
			}
		}
		status, answer = call(bridge.Queue, "POST", "/verdicts", body)
		bridge.log("verdict %s %s on %s: %d %s", change, verdict["status"], record.Ref, status, answerText(answer))
		if status == 200 || status == 409 {
			memory.Posted = append(memory.Posted, key)
		}
		if verdict["status"] == "void" && status == 200 && !slices.Contains(memory.Requeued, tree) {
			memory.Requeued = append(memory.Requeued, tree)
			started := "requeue.sh refused"
			if bridge.Gate.Requeue(tree) {
				started = "started"
			}
			bridge.log("requeued %s after its void: %s", tree[:min(12, len(tree))], started)
		}
	}
	return nil
}
