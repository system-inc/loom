package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/system-inc/loom/protocol"
)

// Serve's live status reaches the wire (#yk0q0kj): every liveEvery it posts what it is doing to its pool,
// <pool>/live, with its pool token, and the pool keeps the newest per worker for the board's house panel to read
// (docs/protocol.md, The pool). It is what `loom top` reads from the box's own files, made compact: the units in hand,
// the slots, the disk against its floor, the blob cache against its bound. A post that fails is said once and never
// touches serving.

// liveEvery is how often serve posts its live status.
const liveEvery = 10 * time.Second

// A liveStatus is the body of a live post, exactly the fields the wire takes (wire/source/Pool.ts, checkPoolLive).
type liveStatus struct {
	Worker    string           `json:"worker"`
	Release   string           `json:"release,omitempty"`
	Runner    string           `json:"runner,omitempty"`
	StartedAt string           `json:"startedAt"`
	Unfit     string           `json:"unfit,omitempty"`
	Units     []liveStatusUnit `json:"units"`
	Slots     *liveSlots       `json:"slots,omitempty"`
	Disk      *liveDisk        `json:"disk,omitempty"`
	Cache     *liveCache       `json:"cache,omitempty"`
	Totals    liveTotals       `json:"totals"`
}

type liveStatusUnit struct {
	Run             string `json:"run"`
	Unit            string `json:"unit"`
	Package         string `json:"package,omitempty"`
	StartedAt       string `json:"startedAt"`
	Deadline        string `json:"deadline,omitempty"`
	Cpus            int    `json:"cpus,omitempty"`
	MemoryMegabytes int    `json:"memoryMegabytes,omitempty"`
}

type liveSlots struct {
	Units               int `json:"units"`
	Cpus                int `json:"cpus"`
	MemoryMegabytes     int `json:"memoryMegabytes"`
	HeldCpus            int `json:"heldCpus"`
	HeldMemoryMegabytes int `json:"heldMemoryMegabytes"`
}

type liveDisk struct {
	FreeMegabytes  int64 `json:"freeMegabytes"`
	FloorMegabytes int64 `json:"floorMegabytes"`
}

type liveCache struct {
	BlobBytes      int64 `json:"blobBytes"`
	BlobLimitBytes int64 `json:"blobLimitBytes"`
}

type liveTotals struct {
	Units  int `json:"units"`
	Passed int `json:"passed"`
	Failed int `json:"failed"`
	Broken int `json:"broken"`
}

// A livePoster holds serve's live status, which serve's loop changes and its own goroutine posts.
type livePoster struct {
	mutex   sync.Mutex
	status  liveStatus
	options ServeOptions
	client  *http.Client
	// disks are the disks a unit writes, the lowest free one posted; blobs is the blob cache's directory.
	disks []string
	blobs string
	limit int64
	// failing is whether the last post failed, so a wire that refuses is said once, not every 10 s.
	failing bool
	// asked is set once serve's first ask has reached the pool: the pool keeps a status only for a worker it has seen
	// ask (wire/source/Pool.ts), so nothing is posted before.
	asked atomic.Bool
}

// update changes the status under the poster's lock.
func (poster *livePoster) update(change func(status *liveStatus)) {
	poster.mutex.Lock()
	defer poster.mutex.Unlock()
	change(&poster.status)
}

// loop posts the status every every until loopContext ends, then once more, so the pool's last word for the worker is
// its serve having stopped.
func (poster *livePoster) loop(loopContext context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-loopContext.Done():
			poster.post()
			return
		case <-ticker.C:
			poster.post()
		}
	}
}

// post sends the status as it stands, with the disk and the blob cache read now; nothing before serve's first ask.
func (poster *livePoster) post() {
	if !poster.asked.Load() {
		return
	}
	poster.mutex.Lock()
	status := poster.status
	status.Units = append([]liveStatusUnit{}, poster.status.Units...)
	poster.mutex.Unlock()
	free := int64(-1)
	for _, disk := range poster.disks {
		if megabytes, err := poster.options.freeMegabytes(disk); err == nil && (free < 0 || megabytes < free) {
			free = megabytes
		}
	}
	if free >= 0 {
		status.Disk = &liveDisk{FreeMegabytes: free, FloorMegabytes: poster.options.MinimumFreeMegabytes}
	}
	if poster.blobs != "" {
		status.Cache = &liveCache{BlobBytes: directoryBytes(poster.blobs), BlobLimitBytes: poster.limit}
	}
	body, err := json.Marshal(status)
	if err == nil {
		err = poster.send(body)
	}
	if err != nil && !poster.failing {
		fmt.Fprintf(poster.options.Report, "loom-runner serve: posting the live status failed (said once; serving goes on): %v\n", err)
	}
	poster.failing = err != nil
}

func (poster *livePoster) send(body []byte) error {
	postContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := strings.TrimSuffix(poster.options.Pool, "/") + "/live"
	request, err := http.NewRequestWithContext(postContext, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+poster.options.Token)
	request.Header.Set("Content-Type", "application/json")
	response, err := poster.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, 512))
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("POST %s: %s %s", url, response.Status, bytes.TrimSpace(answer))
	}
	return nil
}

// liveUnitOf is a unit in hand as the live status says it, with its share when it holds one.
func liveUnitOf(unit protocol.Unit, started time.Time, unitShare *share) liveStatusUnit {
	live := liveUnit(unit, "", started)
	posted := liveStatusUnit{Run: unit.Run, Unit: unit.Unit, Package: live.Package, StartedAt: started.UTC().Format(time.RFC3339Nano)}
	if !live.Deadline.IsZero() {
		posted.Deadline = live.Deadline.UTC().Format(time.RFC3339Nano)
	}
	if unitShare != nil {
		posted.Cpus, posted.MemoryMegabytes = unitShare.cpus, unitShare.memoryMegabytes
	}
	return posted
}

// directoryBytes is the bytes of the regular files directly in a directory, 0 when it can't be read.
func directoryBytes(directory string) int64 {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return 0
	}
	total := int64(0)
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total
}

// blobDirectory is where a root's blob cache keeps its blobs.
func blobDirectory(root string) string {
	return filepath.Join(root, blobDirectoryName)
}
