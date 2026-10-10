package judge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/r2"
)

// Every unit attempt a decided run holds leaves one row (#g1jvdbq, Kirk: "are we doing good tracking of how long each
// unit took?"): what its run's events say of it, as fields, and when its run was placed, so a unit's time is read
// across runs without parsing streams. The judge writes a run's rows once it has posted the run, as one object,
// units/<YYYY>/<MM>/<DD>/<run>.jsonl in the action store's bucket under no lifecycle rule, the day the run was decided
// (R2 appends nothing, and a run decided again writes the same object again). `loom units` reads them.

// RowsPrefix is where a day's rows are kept.
const RowsPrefix = "units/"

// A UnitRow is one unit attempt of a run.
type UnitRow struct {
	Run     string `json:"run"`
	Future  string `json:"future"`
	Attempt int    `json:"attempt"`
	// Unit is its key, Name what the plan names it (its package, or its phase), Kind its key's kind.
	Unit string `json:"unit"`
	Name string `json:"name,omitempty"`
	Kind string `json:"kind,omitempty"`
	// Machine is the worker its started event names.
	Machine string `json:"machine,omitempty"`
	// Placed is when its run was placed (the placer's ledger), Started and Finished its events' times, and
	// QueueSeconds the time between placed and started: its wait for a worker.
	Placed       string  `json:"placed,omitempty"`
	Started      string  `json:"started"`
	Finished     string  `json:"finished,omitempty"`
	QueueSeconds float64 `json:"queueSeconds,omitempty"`
	// WallSeconds, UserSeconds and SystemSeconds are its exit event's: its command's, or its tests' together.
	WallSeconds   float64 `json:"wallSeconds,omitempty"`
	UserSeconds   float64 `json:"userSeconds,omitempty"`
	SystemSeconds float64 `json:"systemSeconds,omitempty"`
	// Status is its finished event's (passed, failed or broken), empty when it never finished.
	Status string `json:"status,omitempty"`
	// OverBudget marks a test unit that passed past the run budget (RunBudgetSeconds, #ccewvra): green with a warning,
	// so the rollups can count and name the units to split.
	OverBudget bool `json:"overBudget,omitempty"`
	// The runner's timing event, its fields at the row's top level; all zero from a runner that sends none.
	protocol.Timing
}

// PlacedOf is when a run was placed, from the placer's ledger; found is false when the ledger doesn't hold it.
type PlacedOf func(run string) (placed string, found bool)

// Rows keeps a run's rows.
type Rows interface {
	Put(key string, content []byte) error
}

// BucketRows puts a run's rows straight into the action store's bucket.
type BucketRows struct {
	Bucket *r2.Bucket
}

func (rows BucketRows) Put(key string, content []byte) error {
	return rows.Bucket.Put(key, content, r2.PutOptions{ContentType: "application/x-ndjson"})
}

// RowsKey is where the rows of a run decided at decided are kept.
func RowsKey(run string, decided time.Time) string {
	return RowsPrefix + decided.UTC().Format("2006/01/02") + "/" + run + ".jsonl"
}

// GatherRows makes one row of each of the future's units that reported in the run (its last attempt there), in plan
// order. A unit with no started event in this run (reused, carried from an earlier attempt, never placed) has none.
func GatherRows(future PlannedFuture, attempt int, run string, events []protocol.Event, placed PlacedOf) []UnitRow {
	placedAt, placedFound := "", false
	if placed != nil {
		placedAt, placedFound = placed(run)
	}
	rows := []UnitRow{}
	for _, unit := range future.Units {
		mine := eventsOf(events, unit.UnitKey)
		start := -1
		for index, event := range mine {
			if event.Type == "started" {
				start = index
			}
		}
		if start < 0 {
			continue
		}
		row := UnitRow{Run: run, Future: future.Future, Attempt: attempt, Unit: unit.UnitKey, Name: unit.Name, Kind: planUnitOf(unit).Kind,
			Machine: mine[start].Machine, Started: mine[start].Time}
		for _, event := range mine[start:] {
			switch event.Type {
			case "exit":
				row.WallSeconds, row.UserSeconds, row.SystemSeconds = event.WallSeconds, event.UserSeconds, event.SystemSeconds
			case "timing":
				if event.Timing != nil {
					row.Timing = *event.Timing
				}
			case "finished":
				row.Finished, row.Status = event.Time, event.Status
			}
		}
		row.OverBudget = row.Kind == KindTest && row.Status == protocol.StatusPassed && row.WallSeconds > RunBudgetSeconds
		if placedFound {
			row.Placed = placedAt
			placedTime, placedError := time.Parse(time.RFC3339Nano, placedAt)
			startedTime, startedError := time.Parse(time.RFC3339Nano, row.Started)
			if placedError == nil && startedError == nil && startedTime.After(placedTime) {
				row.QueueSeconds = startedTime.Sub(placedTime).Round(time.Millisecond).Seconds()
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// EncodeRows is rows as JSON lines.
func EncodeRows(rows []UnitRow) ([]byte, error) {
	var content bytes.Buffer
	encoder := json.NewEncoder(&content)
	encoder.SetEscapeHTML(false)
	for _, row := range rows {
		if err := encoder.Encode(row); err != nil {
			return nil, err
		}
	}
	return content.Bytes(), nil
}

// keepRows writes a decided run's rows, when the puller keeps rows. It never fails the pass: the run's verdict is
// posted, and a row that couldn't be written is said on Report and written again only if the run is decided again.
func (puller Puller) keepRows(future PlannedFuture, attempt int, run string, events []protocol.Event) {
	if puller.Rows == nil {
		return
	}
	rows := GatherRows(future, attempt, run, events, puller.Placed)
	if len(rows) == 0 {
		return
	}
	content, err := EncodeRows(rows)
	if err == nil {
		err = puller.Rows.Put(RowsKey(run, puller.Loop.Now()), content)
	}
	if err != nil && puller.Report != nil {
		puller.Report(fmt.Sprintf("judge: run %s's %d unit rows weren't kept: %v", run, len(rows), err))
	}
}
