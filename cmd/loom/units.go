package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/system-inc/loom/judge"
	"github.com/system-inc/loom/r2"
)

// units is `loom units` (#g1jvdbq): the unit rows the judge keeps (judge/rows.go), read for a span of days through
// R2's S3 interface with the store's key, filtered, and grouped by name, box or day, each group with its count, its
// statuses, and the p50 and p90 of its wall, queue wait, phases and peak memory. --json prints every row as kept.
func units(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("units", flag.ContinueOnError)
	flags.SetOutput(stderr)
	days := flags.Int("days", 1, "the days to read, today (UTC) and the ones before it")
	day := flags.String("day", "", "one day to read, YYYY-MM-DD (UTC), in place of --days")
	name := flags.String("name", "", "only units whose name (a package, or a phase) holds this")
	box := flags.String("box", "", "only units whose worker's name holds this")
	kind := flags.String("kind", "", "only units of this kind: test, product or phase")
	status := flags.String("status", "", "only units that finished so: passed, failed or broken")
	by := flags.String("by", "name", "group by name, box or day")
	asJSON := flags.Bool("json", false, "print every row as kept, one JSON line each, in place of the groups")
	storeFlags := addStoreFlags(flags)
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 || *days < 1 || !slices.Contains([]string{"name", "box", "day"}, *by) {
		fmt.Fprintln(stderr, "usage: loom units [--days N | --day YYYY-MM-DD] [--name <text>] [--box <text>] [--kind <kind>] [--status <status>] [--by name|box|day] [--json] [--r2 <key file>]")
		return 2
	}
	spanned, err := unitDays(*day, *days, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "units:", err)
		return 2
	}
	store, err := storeFlags.open(nil)
	if err != nil {
		fmt.Fprintln(stderr, "units:", err)
		return 1
	}
	rows, err := readUnitRows(store.Bucket, spanned)
	if err != nil {
		fmt.Fprintln(stderr, "units:", err)
		return 1
	}
	rows = slices.DeleteFunc(rows, func(row judge.UnitRow) bool {
		return !strings.Contains(row.Name, *name) || !strings.Contains(row.Machine, *box) || (*kind != "" && row.Kind != *kind) ||
			(*status != "" && row.Status != *status)
	})
	if *asJSON {
		content, err := judge.EncodeRows(rows)
		if err != nil {
			fmt.Fprintln(stderr, "units:", err)
			return 1
		}
		stdout.Write(content)
		return 0
	}
	writeUnitGroups(stdout, groupUnitRows(rows, *by))
	return 0
}

// unitDays are the days to read, newest first: day alone, or today and the count-1 before it, in UTC.
func unitDays(day string, count int, now time.Time) ([]string, error) {
	if day != "" {
		parsed, err := time.Parse("2006-01-02", day)
		if err != nil {
			return nil, fmt.Errorf("--day %q isn't YYYY-MM-DD", day)
		}
		return []string{parsed.Format("2006/01/02")}, nil
	}
	spanned := []string{}
	for back := range count {
		spanned = append(spanned, now.UTC().AddDate(0, 0, -back).Format("2006/01/02"))
	}
	return spanned, nil
}

// rowStore is what reads the rows: r2.Bucket's List and Get.
type rowStore interface {
	List(prefix string) ([]r2.Object, error)
	Get(key string) ([]byte, error)
}

// readUnitRows reads every row kept on the days (YYYY/MM/DD), one object a run.
func readUnitRows(store rowStore, days []string) ([]judge.UnitRow, error) {
	rows := []judge.UnitRow{}
	for _, day := range days {
		objects, err := store.List(judge.RowsPrefix + day + "/")
		if err != nil {
			return nil, fmt.Errorf("listing %s's rows: %w", day, err)
		}
		for _, object := range objects {
			content, err := store.Get(object.Key)
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", object.Key, err)
			}
			scanner := bufio.NewScanner(bytes.NewReader(content))
			scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
			for scanner.Scan() {
				var row judge.UnitRow
				if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
					return nil, fmt.Errorf("%s holds a line that isn't a row: %w", object.Key, err)
				}
				rows = append(rows, row)
			}
			if err := scanner.Err(); err != nil {
				return nil, fmt.Errorf("reading %s: %w", object.Key, err)
			}
		}
	}
	return rows, nil
}

// A unitGroup is the rows of one name, box or day.
type unitGroup struct {
	key                    string
	rows                   []judge.UnitRow
	passed, failed, broken int
}

// groupUnitRows groups the rows by name, box (the worker) or day (started's date), the groups with the most rows first.
func groupUnitRows(rows []judge.UnitRow, by string) []unitGroup {
	index := map[string]*unitGroup{}
	for _, row := range rows {
		key := row.Name
		switch by {
		case "box":
			key = row.Machine
		case "day":
			key, _, _ = strings.Cut(row.Started, "T")
		}
		group := index[key]
		if group == nil {
			group = &unitGroup{key: key}
			index[key] = group
		}
		group.rows = append(group.rows, row)
		switch row.Status {
		case "passed":
			group.passed++
		case "failed":
			group.failed++
		default:
			group.broken++
		}
	}
	groups := []unitGroup{}
	for _, group := range index {
		groups = append(groups, *group)
	}
	slices.SortFunc(groups, func(left, right unitGroup) int {
		if len(left.rows) != len(right.rows) {
			return len(right.rows) - len(left.rows)
		}
		return strings.Compare(left.key, right.key)
	})
	return groups
}

// percentile is the nearest-rank percentile (0 to 100) of the values a row has, those above zero: a row whose runner
// sent no timing, or a unit without that phase, says nothing of it. ok is false when no row says anything.
func percentile(rows []judge.UnitRow, value func(judge.UnitRow) float64, rank float64) (float64, bool) {
	values := []float64{}
	for _, row := range rows {
		if read := value(row); read > 0 {
			values = append(values, read)
		}
	}
	if len(values) == 0 {
		return 0, false
	}
	slices.Sort(values)
	return values[max(0, int(math.Ceil(rank/100*float64(len(values))))-1)], true
}

// unitColumns are the columns of each group after its count and statuses: each value's p50 and p90.
var unitColumns = []struct {
	name  string
	value func(judge.UnitRow) float64
}{
	{"wall", func(row judge.UnitRow) float64 { return row.WallSeconds }},
	{"queue", func(row judge.UnitRow) float64 { return row.QueueSeconds }},
	{"fetch", func(row judge.UnitRow) float64 { return row.FetchSeconds }},
	{"unpack", func(row judge.UnitRow) float64 { return row.UnpackSeconds }},
	{"prepare", func(row judge.UnitRow) float64 { return row.PrepareSeconds }},
	{"test", func(row judge.UnitRow) float64 { return row.TestSeconds }},
	{"peak MB", func(row judge.UnitRow) float64 { return float64(row.PeakMegabytes) }},
}

// writeUnitGroups prints one line per group: its key, rows, statuses, and each column's p50/p90 ("-" where no row says).
func writeUnitGroups(stdout io.Writer, groups []unitGroup) {
	table := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	header := "group\tunits\tpassed\tfailed\tbroken"
	for _, column := range unitColumns {
		header += "\t" + column.name + " p50/p90"
	}
	fmt.Fprintln(table, header)
	for _, group := range groups {
		line := fmt.Sprintf("%s\t%d\t%d\t%d\t%d", strings.TrimPrefix(group.key, "github.com/system-inc/adamic/"), len(group.rows), group.passed, group.failed, group.broken)
		for _, column := range unitColumns {
			p50, found := percentile(group.rows, column.value, 50)
			p90, _ := percentile(group.rows, column.value, 90)
			if !found {
				line += "\t-"
				continue
			}
			line += "\t" + tenths(p50) + "/" + tenths(p90)
		}
		fmt.Fprintln(table, line)
	}
	table.Flush()
}

// tenths is a value to a tenth, with no trailing zero: 41.5, 3, 2048.
func tenths(value float64) string {
	return strconv.FormatFloat(math.Round(value*10)/10, 'f', -1, 64)
}
