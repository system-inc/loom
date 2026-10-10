package judge

import (
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/protocol"
)

// The fixtures in testdata/testlog are two units' uploaded loom-out/test.jsonl.gz from run
// future-f7812fffa6b565c3d31a13d3183e8d074d412e92-1 (proof 1, Oct 10 00:43Z), as the wire served them: unit 5dc8cfa10ca6
// (blob b7907266, 58 passes and 20 skips) and a package with no test files (blob f2c752c1).

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	content, err := os.ReadFile("testdata/testlog/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func gzipped(text string) []byte {
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	writer.Write([]byte(text))
	writer.Close()
	return buffer.Bytes()
}

// uploadedStream is a unit's events as the runner writes them: a prepare line, no test2json, and its test log uploaded.
func uploadedStream(unit, status, sha string) []protocol.Event {
	return []protocol.Event{{Unit: unit, Type: "started", Run: "run-1"}, {Unit: unit, Type: "output", Text: "loom-runner prepare: ready in 1 s"},
		{Unit: unit, Type: "exit", Code: code(0)}, {Unit: unit, Run: "run-1", Type: "uploaded", Path: TestLogPath, Sha256: sha},
		{Unit: unit, Run: "run-1", Type: "uploaded", Path: "loom-out/cpu.tsv", Sha256: strings.Repeat("c", 64)}, {Unit: unit, Type: "finished", Status: status}}
}

func TestARealUploadedLogReadsAsItsTests(t *testing.T) {
	finished, found := FinishedFromEvents(uploadedStream("u", "passed", "b79072660747a454e535864ee40647495f26ca92a96e5d2b082b05c5f4a9d907"))
	if !found || len(finished.Tests) != 0 || finished.TestLog == nil || finished.TestLog.Run != "run-1" {
		t.Fatalf("finished %+v: want no streamed tests and the log named", finished)
	}
	read, err := WithTestLog(finished, fixture(t, "5dc8cfa10ca6.jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, outcome := range read.Tests {
		counts[outcome.Outcome]++
	}
	if counts["pass"] != 58 || counts["skip"] != 20 || len(read.Tests) != 78 || !read.TestLogRead || read.NoTestFiles || len(read.Events) == 0 {
		t.Fatalf("counts %v of %d, read %v, no test files %v", counts, len(read.Tests), read.TestLogRead, read.NoTestFiles)
	}
	empty, err := WithTestLog(finished, fixture(t, "no-test-files.jsonl.gz"))
	if err != nil || len(empty.Tests) != 0 || !empty.NoTestFiles {
		t.Fatalf("no test files: %+v (%v)", empty, err)
	}
}

func TestSubtestsAreTheirOwnOutcomes(t *testing.T) {
	log := `{"Action":"run","Package":"p","Test":"TestA"}
{"Action":"pass","Package":"p","Test":"TestA/one"}
{"Action":"fail","Package":"p","Test":"TestA/two"}
{"Action":"fail","Package":"p","Test":"TestA"}
`
	read, err := WithTestLog(Finished{TestLog: &TestLogRef{Sha256: "x"}}, gzipped(log))
	if err != nil || len(read.Tests) != 3 || read.Tests[1].Test != "TestA/one" || read.Tests[2].Outcome != "fail" {
		t.Fatalf("tests %+v (%v)", read.Tests, err)
	}
}

func TestABadLogIsAnErrorNeverEmptyTests(t *testing.T) {
	for name, content := range map[string][]byte{"not gzip": []byte("plain"), "not json": gzipped("{\"Action\":\"pass\"}\nnot json\n")} {
		if _, err := WithTestLog(Finished{TestLog: &TestLogRef{Sha256: "x"}}, content); err == nil {
			t.Fatalf("%s read without an error", name)
		}
	}
	runs := EventRuns{Read: func(string) ([]protocol.Event, error) { return uploadedStream("u", "passed", "x"), nil },
		Log: func(string, string) ([]byte, error) { return nil, errors.New("wire down") }}
	if _, _, err := runs.Finished("run-1", "u"); err == nil || !strings.Contains(err.Error(), "wire down") {
		t.Fatalf("an unreadable log gave %v, want the error", err)
	}
	fabric := EventFabric{Rerun: func(string, string) ([]protocol.Event, error) { return uploadedStream("job", "passed", "x"), nil },
		Log: func(run, sha string) ([]byte, error) {
			return gzipped(`{"Action":"pass","Package":"p","Test":"TestR"}` + "\n"), nil
		}}
	if rerun, err := fabric.RerunAlone("u", futureTree); err != nil || len(rerun.Tests) != 1 || rerun.Tests[0].Test != "TestR" {
		t.Fatalf("rerun %+v (%v): want its tests from its log", rerun.Tests, err)
	}
}

func TestZeroRun(t *testing.T) {
	withLog := func(text string) Finished {
		read, err := WithTestLog(Finished{Attempt: Attempt{Status: Passed}, TestLog: &TestLogRef{Run: "run-1", Sha256: "x"}}, gzipped(text))
		if err != nil {
			t.Fatal(err)
		}
		return read
	}
	cases := []struct {
		name     string
		finished Finished
		named    []string
		status   string
		rule     string
	}{
		{"a passed unit with no log is void", Finished{Attempt: Attempt{Status: Passed}, Tests: []TestOutcome{outcome("TestA", "pass")}}, nil, Void, Rule},
		{"an empty log never posts green", withLog(""), nil, Failed, RuleZeroRun},
		{"a log with no test result is red", withLog(`{"Action":"start","Package":"p"}` + "\n"), nil, Failed, RuleZeroRun},
		{"no test files passes", withLog(`{"Action":"output","Package":"p","Output":"?   \tp\t[no test files]\n"}` + "\n"), nil, Passed, Rule},
		{"a log with a result passes", withLog(`{"Action":"pass","Package":"p","Test":"TestA"}` + "\n"), nil, Passed, Rule},
		{"a named test with no result is red", withLog(`{"Action":"pass","Package":"p","Test":"TestA"}` + "\n"), []string{"TestA", "TestB"}, Failed, RuleZeroRun},
		{"every named test with a result passes", withLog(`{"Action":"pass","Package":"p","Test":"TestA"}` + "\n" + `{"Action":"skip","Package":"p","Test":"TestB"}` + "\n"), []string{"TestA", "TestB"}, Passed, Rule},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness()
			h.runs["u"] = c.finished
			loop := Loop{Runs: h.runs, Fabric: h.fabric, Main: h.main, Queue: h.queue, Blobs: h.blobs, Reused: stubReused{}, Now: time.Now, RequireTestLog: true}
			post, err := loop.JudgeFuture(censusJob(PlanUnit{UnitKey: "u", Named: c.named}))
			if err != nil {
				t.Fatal(err)
			}
			record := recordOf(t, post, "u")
			if record.Status != c.status || !strings.Contains(string(post.Verdicts[0]), `"rule":"`+c.rule+`"`) || len(h.fabric.Asked) != 0 {
				t.Fatalf("%+v %s, want %s by %s", record, post.Verdicts[0], c.status, c.rule)
			}
		})
	}
}
