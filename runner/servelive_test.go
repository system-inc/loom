package runner

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/protocol"
)

// A box serve's live status reaches its pool (#yk0q0kj). Mutants: no post at all; a post every loop pass instead of
// every liveEvery; a field the wire doesn't take; the units in hand or the totals not kept current; a refused post
// stopping serve; a post before serve's first ask, which the pool refuses.

// The wire's fields exactly (wire/source/Pool.ts, checkPoolLive): a post that decodes strictly into these is one the wire
// takes.
type wireLive struct {
	Worker    string `json:"worker"`
	Release   string `json:"release"`
	Runner    string `json:"runner"`
	StartedAt string `json:"startedAt"`
	Unfit     string `json:"unfit"`
	Units     []struct {
		Run             string `json:"run"`
		Unit            string `json:"unit"`
		Package         string `json:"package"`
		Phase           string `json:"phase"`
		StartedAt       string `json:"startedAt"`
		Deadline        string `json:"deadline"`
		Cpus            int    `json:"cpus"`
		MemoryMegabytes int    `json:"memoryMegabytes"`
	} `json:"units"`
	Slots *struct {
		Units               int `json:"units"`
		Cpus                int `json:"cpus"`
		MemoryMegabytes     int `json:"memoryMegabytes"`
		HeldCpus            int `json:"heldCpus"`
		HeldMemoryMegabytes int `json:"heldMemoryMegabytes"`
	} `json:"slots"`
	Disk *struct {
		FreeMegabytes  int64 `json:"freeMegabytes"`
		FloorMegabytes int64 `json:"floorMegabytes"`
	} `json:"disk"`
	Cache *struct {
		BlobBytes      int64 `json:"blobBytes"`
		BlobLimitBytes int64 `json:"blobLimitBytes"`
	} `json:"cache"`
	Totals *struct {
		Units  int `json:"units"`
		Passed int `json:"passed"`
		Failed int `json:"failed"`
		Broken int `json:"broken"`
	} `json:"totals"`
}

func TestServePostsItsLiveStatusToItsPool(t *testing.T) {
	pool := newTestPool(t)
	pool.queue = []protocol.Unit{pool.unit("a", "sleep 1"), pool.unit("b", "sleep 1")}
	options := pool.slotsOptions(t, 16, 1<<20, 2, 4, 1024)
	options.liveEvery = 100 * time.Millisecond
	// The pool answers each ask after 300 ms, so several of serve's ticks come before its first ask lands.
	pool.askDelay = 300 * time.Millisecond
	var report lockedBuffer
	options.Report = &report
	started := time.Now()
	if summary, err := Serve(context.Background(), options); err != nil || summary.Passed != 2 {
		t.Fatalf("summary %+v, %v", summary, err)
	}
	// Nothing is posted before serve's first ask, which the pool would refuse.
	if strings.Contains(string(report.Bytes()), "posting the live status failed") {
		t.Fatalf("a post the pool refused:\n%s", report.Bytes())
	}
	served := time.Since(started)
	pool.mutex.Lock()
	lives := append([]postedLive{}, pool.lives...)
	pool.mutex.Unlock()
	// One post every 100 ms and one at the end, never one each pass of serve's loop.
	if len(lives) < 5 || len(lives) > int(served/options.liveEvery)+2 {
		t.Fatalf("%d live posts in %v", len(lives), served)
	}
	decoded := []wireLive{}
	for _, posted := range lives {
		var live wireLive
		if err := protocol.Decode(bytes.NewReader(posted.body), &live); err != nil {
			t.Fatalf("a post the wire wouldn't take: %v: %s", err, posted.body)
		}
		if live.Worker != "codex-1" || live.StartedAt == "" || live.Release != Version || live.Units == nil || live.Disk == nil || live.Disk.FloorMegabytes != 1500 || live.Slots == nil {
			t.Fatalf("a post: %s", posted.body)
		}
		decoded = append(decoded, live)
	}
	both := false
	for _, live := range decoded {
		if len(live.Units) == 2 {
			both = live.Units[0].Cpus == 4 && live.Units[0].Deadline != "" && live.Slots.HeldCpus == 8 && live.Slots.Cpus == 16
		}
	}
	if !both {
		t.Fatal("no post said both units in hand, their shares and the slots they hold")
	}
	last := decoded[len(decoded)-1]
	if len(last.Units) != 0 || last.Totals == nil || last.Totals.Units != 2 || last.Totals.Passed != 2 || last.Slots.HeldCpus != 0 {
		t.Fatalf("the last post: %s", lives[len(lives)-1].body)
	}
}

func TestALiveStatusTheWireRefusesNeverStopsServing(t *testing.T) {
	pool := newTestPool(t)
	pool.refuseLive = true
	pool.queue = []protocol.Unit{pool.unit("a", "sleep 0.5"), pool.unit("b", "true")}
	options := pool.slotsOptions(t, 16, 1<<20, 1, 4, 1024)
	options.liveEvery = 50 * time.Millisecond
	var report lockedBuffer
	options.Report = &report
	if summary, err := Serve(context.Background(), options); err != nil || summary.Passed != 2 {
		t.Fatalf("summary %+v, %v", summary, err)
	}
	if said := strings.Count(string(report.Bytes()), "posting the live status failed"); said != 1 {
		t.Fatalf("a refused live status said %d times:\n%s", said, report.Bytes())
	}
}
