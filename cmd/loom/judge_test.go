package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/system-inc/loom/judge"
	"github.com/system-inc/loom/protocol"
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

type fixedFutures []judge.PlannedFuture

func (futures fixedFutures) Planned() ([]judge.PlannedFuture, error) { return futures, nil }

func TestPostParityPostsOnlyParityFutures(t *testing.T) {
	var out bytes.Buffer
	parityTree, realTree := strings.Repeat("a", 40), strings.Repeat("b", 40)
	source := parityFutures{source: fixedFutures{{Future: parityTree, Parity: true}, {Future: realTree}}, trees: map[string]bool{}}
	if _, err := source.Planned(); err != nil {
		t.Fatal(err)
	}
	live := &keptQueue{}
	queue := scopedQueue{parity: true, trees: source.trees, live: live, dry: printedQueue{out: &out}}
	for _, future := range []string{parityTree, realTree} {
		if err := queue.PostVerdicts(future, judge.FuturePost{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(live.futures) != 1 || live.futures[0] != parityTree {
		t.Fatalf("posted %v, want the parity future alone", live.futures)
	}
	if !strings.Contains(out.String(), realTree) {
		t.Fatalf("printed %q, want the real change's future kept dry", out.String())
	}
}

func TestTheCensusConfigLoadsTheLiveRowsForThePoolsPlatform(t *testing.T) {
	config, err := censusConfig([]string{"../../judge/testdata/census/skips.json", "../../judge/testdata/census/census-extra.json"}, "../../judge/testdata/census/heavy-units.tsv", "")
	if err != nil || len(config.Rows) != 141 || len(config.Heavy) != 1 || config.Platform != "linux" || config.Landed != nil {
		t.Fatalf("config %d rows on %q (%v)", len(config.Rows), config.Platform, err)
	}
	if _, err := censusConfig([]string{"../../judge/testdata/census/plain-skips.jsonl"}, "", ""); err == nil {
		t.Fatal("a log was loaded as census rows")
	}
}

func TestJudgeWitnessExitsOneOnAKeyFaultAndThreeWhenIncomplete(t *testing.T) {
	directory := t.TempDir()
	write := func(name, content string) string {
		path := directory + "/" + name
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	plan := write("plan.json", `{"units":[{"unitKey":"a","decision":"reuse"},{"unitKey":"b","decision":"run"}]}`)
	record := func(key, status string) string {
		return `{"unitKey":"` + key + `","future":"t","status":"` + status + `","cause":null,"tests":{"sha256":"x"}}`
	}
	cases := []struct {
		name, verdicts string
		code           int
		says           string
	}{
		{"clean", record("a", "passed") + "\n" + record("b", "failed") + "\n", 0, "witness clean"},
		{"a reused key red", record("a", "failed") + "\n" + record("b", "passed") + "\n", 1, "keyFault a"},
		{"unwitnessed", record("b", "passed") + "\n", 1, "unwitnessed a"},
		{"a void to rerun", record("a", "void") + "\n" + record("b", "passed") + "\n", 3, "void a"},
		{"from the log", `{"seq":4,"type":"verdict.decided","data":{"decision":{}}}` + "\n" + `{"seq":5,"type":"verdict.decided","data":{"verdict":` + record("a", "failed") + `}}` + "\n", 1, "keyFault a"},
		{"two trees", record("a", "passed") + "\n" + strings.Replace(record("b", "passed"), `"t"`, `"u"`, 1) + "\n", 2, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errors bytes.Buffer
			code := judgeWitness([]string{"--plan", plan, "--verdicts", write(c.name+".jsonl", c.verdicts)}, &out, &errors)
			if code != c.code || !strings.Contains(out.String(), c.says) {
				t.Fatalf("exit %d, output %q %q", code, out.String(), errors.String())
			}
		})
	}
}

func TestTheGateReadsEachMutantsNewestDecidedBatchFromTheLog(t *testing.T) {
	plainSha, censusSha, canary := strings.Repeat("a", 40), strings.Repeat("c", 40), strings.Repeat("e", 40)
	suite := "plain-wrong-answer-leaf\t" + plainSha + "\ttests\tstatecopy Test[A-Z]\n" + "deferred-red-reaches-census\t" + censusSha + "\tcensus\n"
	unit := func(tree, run, key, status, cause, rule, inline string, passed int) judge.LogEvent {
		causeJSON := "null"
		if cause != "" {
			causeJSON = `"` + cause + `"`
		}
		record := `{"unitKey":"` + key + `","future":"` + tree + `","run":"` + run + `","status":"` + status + `","cause":` + causeJSON + `,"rule":"` + rule +
			`","tests":{"sha256":"` + strings.Repeat("0", 64) + `","passed":` + strconv.Itoa(passed) + `,"failed":0,"skipped":0,"inline":[` + inline + `]}}`
		return judge.LogEvent{Type: "verdict.decided", Subject: judge.LogSubject{Future: tree, UnitKey: key, Run: run}, Data: json.RawMessage(`{"verdict":` + record + `}`)}
	}
	decided := func(seq int64, tree, run, status, red string) judge.LogEvent {
		return judge.LogEvent{Seq: seq, Type: "verdict.decided", Subject: judge.LogSubject{Future: tree, Run: run},
			Data: json.RawMessage(`{"decision":{"status":"` + status + `","red":[` + red + `],"excused":[],"problems":[]},"rule":"judge-v1"}`)}
	}
	failing := `{"package":"github.com/system-inc/adamic/stage3/census/latent/statecopy","test":"TestPublishedCopyModeIsPrivate","outcome":"fail"}`
	events := []judge.LogEvent{
		unit(plainSha, "p-1", "k1", "failed", "change", "judge-v1", failing, 3), decided(1, plainSha, "p-1", "red", `"k1"`),
		unit(censusSha, "c-1", "k2", "void", "infra", "judge-v1", "", 0), decided(2, censusSha, "c-1", "void", ""),
		unit(censusSha, "c-2", "k2", "failed", "change", "judge-v1 census", "", 40), decided(3, censusSha, "c-2", "red", `"k2"`),
		unit(canary, "m-1", "k3", "passed", "", "judge-v1", "", 612), decided(4, canary, "m-1", "green", ""),
	}
	lines, promote, err := gateReport(suite, events, canary)
	if err != nil || !promote {
		t.Fatalf("promote %v (%v):\n%s", promote, err, strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[1], "deferred-red-reaches-census cccccccc ok (run c-2, red at \"census\"") {
		t.Fatalf("census mutant line %q: want its newest decided run, c-2", lines[1])
	}
	// The plain mutant reading green instead: a wrong reading holds the tools and names it.
	events[1] = decided(1, plainSha, "p-1", "green", "")
	events[0] = unit(plainSha, "p-1", "k1", "passed", "", "judge-v1", "", 3)
	lines, promote, _ = gateReport(suite, events, canary)
	if promote || !strings.Contains(strings.Join(lines, "\n"), "plain-wrong-answer-leaf aaaaaaaa wrong green") || !strings.Contains(strings.Join(lines, "\n"), "hold: mutant plain-wrong-answer-leaf: wrong green") {
		t.Fatalf("a wrong reading promoted or wasn't named:\n%s", strings.Join(lines, "\n"))
	}
	// No canary read: held.
	if _, promote, _ := gateReport(suite, events[:6], canary); promote {
		t.Fatal("promoted without a canary reading")
	}
}

// A rerun reads the pool table as it is now: a pool Fabric resizes after the judge starts holds the next rerun's need
// (typeaware's 16 cpus on box-strict-8a70, Release Oct 10 02:21Z), never only after a restart.
func TestTheJudgeReadsThePoolTableFreshForEachRerun(t *testing.T) {
	path := t.TempDir() + "/pools.json"
	write := func(cpus int) {
		table := `{"pools":[{"name":"box-strict-8a70","tier":"box-strict","runner":"8a70","memoryMegabytes":65536,"cpus":` + strconv.Itoa(cpus) + `}]}`
		if err := os.WriteFile(path, []byte(table), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(8)
	if table, err := readPools(path); err != nil || len(table) != 1 || table[0].Cpus != 8 {
		t.Fatalf("table %+v (%v), want the box at 8 cpus", table, err)
	}
	write(16)
	table, err := readPools(path)
	if err != nil || len(table) != 1 || table[0].Cpus != 16 {
		t.Fatalf("table %+v (%v), want the resized box at 16 cpus", table, err)
	}
	if fit := judge.FitPools(table, "test", "8a70", protocol.Resources{MemoryMegabytes: 9710, Cpus: 16}); len(fit) != 1 {
		t.Fatalf("fit %v, want the resized box for a 16-cpu need", fit)
	}
	write(0)
	if _, err := readPools(path); err == nil {
		t.Fatal("read a table whose pool has no cpus")
	}
}
