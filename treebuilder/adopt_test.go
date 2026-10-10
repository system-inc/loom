package treebuilder

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An adopted build is waited for under the same bound as one this builder started, and decided by the store: past the
// bound its group is killed and it's failed, backed off; gone with no index this release reads it's stopped and built
// again at once; and a builder told to stop while it waits leaves it running for the next. Mutants: no bound on an
// adopted build; an adopted build gone with no index taken for built; the wait kept through a stop.
func TestAnAdoptedBuildIsBoundedAndDecidedByTheStore(t *testing.T) {
	running := func(t *testing.T, h *harness, ledger *Ledger, at time.Time) {
		t.Helper()
		if err := ledger.Append(Record{Tree: keyA, Future: strings.Repeat("1", 40), At: at.UTC().Format(time.RFC3339), Event: Started}); err != nil {
			t.Fatal(err)
		}
		if err := ledger.Append(Record{Tree: keyA, Future: strings.Repeat("1", 40), At: at.UTC().Format(time.RFC3339), Event: Running, Pid: 777}); err != nil {
			t.Fatal(err)
		}
	}
	setup := func(t *testing.T) (*harness, *Ledger, *[]int) {
		ledger := openLedger(t, filepath.Join(t.TempDir(), "trees.jsonl"), time.Now())
		h := newHarness(t, listedFutures{}, ledger)
		killed := &[]int{}
		h.builder.Bound, h.builder.Kill = 2*time.Hour, func(pid int) { *killed = append(*killed, pid) }
		waited := 0
		h.builder.Sleep = func(duration time.Duration) {
			if waited++; waited > 10000 {
				t.Fatal("waited on an adopted build past every bound")
			}
			h.now = h.now.Add(duration)
		}
		return h, ledger, killed
	}

	t.Run("past its bound", func(t *testing.T) {
		h, ledger, killed := setup(t)
		running(t, h, ledger, h.now.Add(-time.Hour))
		h.builder.Alive = func(pid int, tree string) bool { return len(*killed) == 0 }
		h.buildOnce(t, true)
		newest, _ := ledger.Newest(keyA)
		if len(*killed) != 1 || (*killed)[0] != 777 || newest.Event != Failed || !newest.Standing(h.now) || !strings.Contains(newest.Cause, "past its 2h0m0s bound") {
			t.Fatalf("killed %v, newest %+v", *killed, newest)
		}
		if len(h.builds) != 0 {
			t.Fatalf("built %v while adopting", h.builds)
		}
	})
	t.Run("gone with no index", func(t *testing.T) {
		h, ledger, killed := setup(t)
		running(t, h, ledger, h.now.Add(-10*time.Minute))
		h.builder.Alive = func(int, string) bool { return false }
		h.builder.Source = listedFutures{future("1", unit(t, "test", "run", keyA))}
		h.buildOnce(t, true)
		if newest, _ := ledger.Newest(keyA); newest.Event != Stopped || newest.Standing(h.now) || len(*killed) != 0 {
			t.Fatalf("newest %+v, killed %v", newest, *killed)
		}
		h.buildOnce(t, true)
		if len(h.builds) != 1 || h.builds[0].Tree != keyA {
			t.Fatalf("built %v, want tree a again", h.builds)
		}
	})
	t.Run("told to stop while waiting", func(t *testing.T) {
		h, ledger, killed := setup(t)
		running(t, h, ledger, h.now.Add(-10*time.Minute))
		waits := 0
		h.builder.Alive = func(int, string) bool { return true }
		h.builder.Stopping = func() bool { waits++; return waits > 3 }
		h.buildOnce(t, false)
		if newest, _ := ledger.Newest(keyA); newest.Event != Running || newest.Pid != 777 || len(*killed) != 0 || len(h.builds) != 0 {
			t.Fatalf("newest %+v, killed %v, builds %v: the build must be left running", newest, *killed, h.builds)
		}
	})
}
