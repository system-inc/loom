package gatelines

import (
	"context"
	"os"
	"path/filepath"
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
	os.WriteFile(filepath.Join(state, "slots"), []byte("server B\nserver S\n"), 0o644)
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
		t.Fatal("no server slot 2")
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
	reader.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if line := lineOf(reader.Read(context.Background())); line.State != "idle" {
		t.Fatalf("after the linger: %+v", line)
	}
}
