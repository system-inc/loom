package gatelines

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A fast gate goes red at its first failure and ends within a second: its slot must read red, from the box's own
// status line, until the slot takes another gate or the linger passes.
func TestARedGateThatEndsAtOnceStaysRedOnItsSlot(t *testing.T) {
	state := t.TempDir()
	os.MkdirAll(filepath.Join(state, "running"), 0o755)
	os.MkdirAll(filepath.Join(state, "running-started"), 0o755)
	// Only the area slot is in the table: slot 2 is lent out, as it is while Loom holds the small slots.
	os.WriteFile(filepath.Join(state, "slots"), []byte("server B\n"), 0o644)
	os.WriteFile(filepath.Join(state, "running", "7"), []byte("codex/x 0123456789abcdef0123 S server S tools log\n"), 0o644)
	os.WriteFile(filepath.Join(state, "running-started", "7"), []byte("1791490000\n"), 0o644)
	status := "running: fast gate of 0123456789ab"
	reader := NewReader(state, t.TempDir())
	reader.Run = func(_ context.Context, box string, _ string, arguments ...string) (string, error) {
		output := "cores|64\n"
		for _, argument := range arguments {
			if argument == "0123456789ab" {
				output += "gate|0123456789ab|2|" + status + "|tests-1.stderr\n"
			}
		}
		return output, nil
	}
	lineOf := func(lines Lines) Line {
		for _, machine := range lines.Machines {
			if machine.Name == "server" {
				for _, line := range machine.Lines {
					if line.Slot == 2 {
						return line
					}
				}
			}
		}
		t.Fatalf("no server slot 2 in %+v", lines.Machines)
		return Line{}
	}
	if line := lineOf(reader.Read(context.Background())); line.State != "gating" || line.Branch != "codex/x" {
		t.Fatalf("running: %+v", line)
	}
	// The gate goes red and its watcher entry is gone before the next poll.
	status = "red: 0123456789abcdef0123 fast gate, first failure at tests after 9.1 s"
	os.Remove(filepath.Join(state, "running", "7"))
	first := lineOf(reader.Read(context.Background()))
	second := lineOf(reader.Read(context.Background()))
	if second.State != "red" || !strings.Contains(second.Detail, "first failure") {
		t.Fatalf("after it ended red: first poll %+v, second poll %+v", first, second)
	}
	// After the linger a slot the table doesn't name (this one is lent out) leaves the board.
	reader.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	for _, machine := range reader.Read(context.Background()).Machines {
		for _, line := range machine.Lines {
			if machine.Name == "server" && line.Slot == 2 {
				t.Fatalf("after the linger, the lent slot is still shown: %+v", line)
			}
		}
	}
}

// A whole gate started by hand on a box other than Home holds that box's claim, and the board reads it there.
func TestAWholeGateOnAnyBoxIsReadFromItsClaim(t *testing.T) {
	state, full := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(state, "slots"), []byte("threadripper B\n"), 0o644)
	os.MkdirAll(filepath.Join(full, "boxes", "threadripper"), 0o755)
	os.WriteFile(filepath.Join(full, "boxes", "threadripper", "holder"), []byte(fmt.Sprintf("%d 8161285ad449cc3a8cce6746120de2133c4a738a\n", os.Getpid())), 0o644)
	reader := NewReader(state, full)
	reader.Run = func(_ context.Context, box string, _ string, arguments ...string) (string, error) {
		output := "cores|64\n"
		if box == "threadripper" && len(arguments) == 1 && arguments[0] == "full" {
			output += "full|8161285ad449-20261009T012331Z|yes|running: full gate of 8161285ad449|tests-1.stderr\n"
		}
		return output, nil
	}
	lineOn := func(box string) *Line {
		for _, machine := range reader.Read(context.Background()).Machines {
			for _, line := range machine.Lines {
				if machine.Name == box && line.Kind == "full" {
					return &line
				}
			}
		}
		return nil
	}
	if line := lineOn("threadripper"); line == nil || line.State != "gating" || line.Sha != "8161285ad449" || line.Since == nil {
		t.Fatalf("the Threadripper's whole gate: %+v", line)
	}
	// A claim whose process is gone holds nothing: past the linger a just-ended gate gets, its line is gone.
	os.WriteFile(filepath.Join(full, "boxes", "threadripper", "holder"), []byte("999999 8161285ad449cc3a8cce6746120de2133c4a738a\n"), 0o644)
	lineOn("threadripper")
	reader.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if line := lineOn("threadripper"); line != nil {
		t.Fatalf("a dead claim still shows %+v", line)
	}
}

// Home runs the whole gate's loop and fast gates beside it: a fast gate there reads its own step and how long ago it
// last wrote (Oct 9: 14383e9d ran 3 hours on Home slot 1 and the board said "starting", since Home was asked about
// its whole gate only).
func TestAFastGateOnHomeReadsItsStepAndItsAge(t *testing.T) {
	state := t.TempDir()
	os.MkdirAll(filepath.Join(state, "running"), 0o755)
	os.WriteFile(filepath.Join(state, "slots"), []byte("home B\n"), 0o644)
	os.WriteFile(filepath.Join(state, "running", "3"), []byte("cloud/land-x 14383e9d94a34563fbca B home B\n"), 0o644)
	reader := NewReader(state, t.TempDir())
	reader.Run = func(_ context.Context, box string, _ string, arguments ...string) (string, error) {
		output := "cores|64\n"
		if box == "home" && len(arguments) > 0 && arguments[0] == "full" {
			output += "full|aaaaaaaaaaaa-20261009T000000Z|no|green: full gate of aaaaaaaaaaaa|tests-1.stderr|900\n"
		}
		for _, argument := range arguments {
			if argument == "14383e9d94a3" {
				output += "gate|14383e9d94a3|1|running: fast gate of 9a338e03|test.jsonl|11040\n"
			}
		}
		return output, nil
	}
	for _, machine := range reader.Read(context.Background()).Machines {
		for _, line := range machine.Lines {
			if machine.Name == "home" && line.Slot == 1 {
				if line.Kind != "fast" || line.Step != "tests 3h04m ago" || line.State != "gating" {
					t.Fatalf("home slot 1: %+v", line)
				}
				return
			}
		}
	}
	t.Fatal("no home slot 1")
}

// The box script itself, on this machine against a ~/fast-gate made here: a fast gate's slot, status, newest file and
// its age, and with "full" first the shas after it still answered.
func TestTheBoxScriptReportsEachGateWithItsAge(t *testing.T) {
	home := t.TempDir()
	run := filepath.Join(home, "fast-gate", "out", "0123456789ab-20261009T000000Z")
	os.MkdirAll(run, 0o755)
	os.WriteFile(filepath.Join(run, "box.txt"), []byte("home slot=2\n"), 0o644)
	os.WriteFile(filepath.Join(run, "status.txt"), []byte("running: fast gate of 0123456789ab\n"), 0o644)
	os.WriteFile(filepath.Join(run, "test.jsonl"), []byte("{}\n"), 0o644)
	old := time.Now().Add(-125 * time.Second)
	os.Chtimes(filepath.Join(run, "test.jsonl"), old, old)
	command := exec.Command("bash", "-c", boxScript, "gate-lines", "full", "0123456789ab", "ffffffffffff")
	command.Env = append(os.Environ(), "HOME="+home)
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var gate, missing string
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "gate|0123456789ab|") {
			gate = line
		}
		if strings.HasPrefix(line, "gate|ffffffffffff|") {
			missing = line
		}
	}
	fields := strings.Split(gate, "|")
	if len(fields) != 6 || fields[2] != "2" || fields[4] != "test.jsonl" || fields[3] != "running: fast gate of 0123456789ab" {
		t.Fatalf("the gate's line: %q in %q", gate, output)
	}
	if age, _ := strconv.Atoi(fields[5]); age < 120 || age > 140 {
		t.Fatalf("its age: %q", fields[5])
	}
	if len(strings.Split(missing, "|")) != 6 {
		t.Fatalf("a sha with no run: %q", missing)
	}
}
