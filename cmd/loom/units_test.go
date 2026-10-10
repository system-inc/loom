package main

import (
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/judge"
	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/r2"
	"github.com/system-inc/loom/r2/r2test"
)

// `loom units` reads the rows the judge keeps for the days asked, every run's object of each, and groups them with
// nearest-rank p50 and p90 over the rows that say a value. Mutants: one object a day read (the first run's only); a
// row that says nothing of a phase counted as zero in its percentile; another day's rows read.
func TestUnitsReadsTheDaysRowsAndGroupsThemWithTheirPercentiles(t *testing.T) {
	fake := r2test.New(t)
	bucket := fake.Bucket()
	row := func(run, name, machine, status string, wall, test float64, peak int64) judge.UnitRow {
		return judge.UnitRow{Run: run, Unit: run + name, Name: "github.com/system-inc/adamic/" + name, Machine: machine, Started: "2026-10-10T18:30:00Z",
			Status: status, WallSeconds: wall, Timing: protocol.Timing{TestSeconds: test, PeakMegabytes: peak}}
	}
	put := func(day time.Time, run string, rows ...judge.UnitRow) {
		content, err := judge.EncodeRows(rows)
		if err != nil {
			t.Fatal(err)
		}
		if err := bucket.Put(judge.RowsKey(run, day), content, r2Options()); err != nil {
			t.Fatal(err)
		}
	}
	today := time.Date(2026, 10, 10, 20, 0, 0, 0, time.UTC)
	put(today, "future-a-1", row("a", "bridge/tsgo", "Cloud-7b29b4", "passed", 40, 38, 2048), row("a", "yaml", "Home-f3279c", "failed", 12, 10, 0))
	put(today, "future-b-1", row("b", "bridge/tsgo", "Server-77b373", "passed", 50, 0, 3072), row("b", "bridge/tsgo", "Cloud-7b29b4", "broken", 90, 85, 4096))
	put(today.AddDate(0, 0, -2), "future-c-1", row("c", "bridge/tsgo", "Cloud-7b29b4", "passed", 999, 999, 9999))

	days, err := unitDays("", 2, today)
	if err != nil || strings.Join(days, ",") != "2026/10/10,2026/10/09" {
		t.Fatalf("days %v %v", days, err)
	}
	rows, err := readUnitRows(bucket, days)
	if err != nil || len(rows) != 4 {
		t.Fatalf("read %d rows %v, want the four of today's two runs", len(rows), err)
	}
	groups := groupUnitRows(rows, "name")
	if len(groups) != 2 || groups[0].key != "github.com/system-inc/adamic/bridge/tsgo" || len(groups[0].rows) != 3 || groups[0].passed != 2 || groups[0].broken != 1 {
		t.Fatalf("groups %+v", groups)
	}
	wall := func(row judge.UnitRow) float64 { return row.WallSeconds }
	test := func(row judge.UnitRow) float64 { return row.TestSeconds }
	if p50, _ := percentile(groups[0].rows, wall, 50); p50 != 50 {
		t.Fatalf("wall p50 %v, want 50 of 40, 50, 90", p50)
	}
	if p90, _ := percentile(groups[0].rows, wall, 90); p90 != 90 {
		t.Fatalf("wall p90 %v", p90)
	}
	// The row with no test seconds says nothing of them: p50 of 38 and 85 is 38.
	if p50, _ := percentile(groups[0].rows, test, 50); p50 != 38 {
		t.Fatalf("test p50 %v, want 38 with the silent row left out", p50)
	}
	if boxes := groupUnitRows(rows, "box"); len(boxes) != 3 || boxes[0].key != "Cloud-7b29b4" || len(boxes[0].rows) != 2 {
		t.Fatalf("by box %+v", boxes)
	}
	var out strings.Builder
	writeUnitGroups(&out, groups)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "bridge/tsgo") || !strings.Contains(lines[1], "50/90") || !strings.Contains(lines[2], "yaml") ||
		!strings.Contains(lines[2], " - ") {
		t.Fatalf("the table:\n%s", out.String())
	}
	if one, _ := unitDays("2026-10-08", 1, today); strings.Join(one, ",") != "2026/10/08" {
		t.Fatalf("--day read %v", one)
	}
	if _, err := unitDays("10/08/2026", 1, today); err == nil {
		t.Fatal("a day not in YYYY-MM-DD was taken")
	}
}

func r2Options() r2.PutOptions { return r2.PutOptions{ContentType: "application/x-ndjson"} }
