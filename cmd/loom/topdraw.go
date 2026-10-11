package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/system-inc/loom/livestatus"
	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/treebuilder"
)

// A topStyle is a span's color, btop's way: 256-color foregrounds on the terminal's own background.
type topStyle int

const (
	styleNone topStyle = iota
	styleTitle
	styleRule
	styleHeading
	styleLabel
	styleDim
	stylePassed
	styleFailed
	styleBroken
	styleBusy
	styleBarLow
	styleBarMiddle
	styleBarHigh
	styleBarEmpty
)

var topPalette = map[topStyle]string{
	styleTitle: "1;38;5;45", styleRule: "38;5;239", styleHeading: "1;38;5;213", styleLabel: "38;5;110", styleDim: "38;5;245",
	stylePassed: "38;5;114", styleFailed: "1;38;5;203", styleBroken: "38;5;215", styleBusy: "38;5;81",
	styleBarLow: "38;5;114", styleBarMiddle: "38;5;221", styleBarHigh: "38;5;203", styleBarEmpty: "38;5;237",
}

type topSpan struct {
	style topStyle
	text  string
}

// A topLine is one row of the screen, spans of text each in its style.
type topLine []topSpan

func (line topLine) add(style topStyle, text string) topLine {
	return append(line, topSpan{style: style, text: text})
}

func (line topLine) width() int {
	total := 0
	for _, span := range line {
		total += len([]rune(span.text))
	}
	return total
}

// render is the line cut to width columns (every rune here is one column), its last kept rune an ellipsis when cut,
// in color or plain.
func (line topLine) render(width int, color bool) string {
	var text strings.Builder
	left := width
	cut := line.width() > width
	for _, span := range line {
		runes := []rune(span.text)
		if left <= 0 {
			break
		}
		if len(runes) > left {
			runes = runes[:left]
		}
		left -= len(runes)
		if cut && left == 0 && len(runes) > 0 {
			runes[len(runes)-1] = '…'
		}
		if color && span.style != styleNone {
			text.WriteString("\033[" + topPalette[span.style] + "m" + string(runes) + "\033[0m")
		} else {
			text.WriteString(string(runes))
		}
	}
	return text.String()
}

// A topSection is one panel: its heading, drawn as a rule, and its rows in order, some always shown and the optional
// ones as the terminal's height allows, the first of them first.
type topSection struct {
	heading  topLine
	rows     []topLine
	optional []bool
	// optionals counts the optional rows.
	optionals int
}

func (section *topSection) always(lines ...topLine) {
	for _, line := range lines {
		section.rows, section.optional = append(section.rows, line), append(section.optional, false)
	}
}

func (section *topSection) optionalRows(lines ...topLine) {
	for _, line := range lines {
		section.rows, section.optional = append(section.rows, line), append(section.optional, true)
		section.optionals++
	}
}

// drawTop is the frame for state at width by height: a title row, then each panel, the rows every panel always shows
// first and then, one row at a time across the panels, their optional rows, until the height is full. Nothing is wider
// than width; a terminal too small for the panels loses their last rows.
func drawTop(state topState, width, height int, color bool) string {
	width, height = max(width, 40), max(height, 10)
	sections := []topSection{machineSection(state, width), serveSection(state, width)}
	if state.Tree != nil || len(state.Builds) > 0 {
		sections = append(sections, treeSection(state, width))
	}
	sections = append(sections, poolsSection(state), queueSection(state))
	shown := make([]int, len(sections))
	used := 1
	for _, section := range sections {
		used += 1 + len(section.rows) - section.optionals
	}
	for added := true; added && used < height; {
		added = false
		for index := range sections {
			if used < height && shown[index] < sections[index].optionals {
				shown[index]++
				used++
				added = true
			}
		}
	}
	rows := []string{titleLine(state, width).render(width, color)}
	for index, section := range sections {
		rows = append(rows, ruleLine(section.heading, width).render(width, color))
		optionals := 0
		for position, line := range section.rows {
			if section.optional[position] {
				if optionals++; optionals > shown[index] {
					continue
				}
			}
			rows = append(rows, line.render(width, color))
		}
	}
	if len(rows) > height {
		rows = rows[:height]
	}
	for len(rows) < height {
		rows = append(rows, "")
	}
	return strings.Join(rows, "\n")
}

func titleLine(state topState, width int) topLine {
	left := topLine{}.add(styleTitle, " loom top").add(styleDim, " · ").add(styleNone, state.Host).add(styleDim, " · "+state.Now.UTC().Format("2006-01-02 15:04:05Z"))
	right := topLine{}.add(styleDim, "q quits ")
	return append(left.add(styleNone, strings.Repeat(" ", max(1, width-left.width()-right.width()))), right...)
}

func ruleLine(heading topLine, width int) topLine {
	line := topLine{}.add(styleRule, "── ")
	line = append(line, heading...)
	return line.add(styleRule, " "+strings.Repeat("─", max(0, width-line.width()-1)))
}

// bar is a meter of filled over cells, colored low to high along its length as btop's are.
func bar(filled float64, cells int) topLine {
	filled = min(max(filled, 0), 1)
	full := int(filled*float64(cells) + 0.5)
	line := topLine{}
	for cell := range cells {
		switch {
		case cell >= full:
			line = line.add(styleBarEmpty, "░")
		case float64(cell) < float64(cells)*0.5:
			line = line.add(styleBarLow, "█")
		case float64(cell) < float64(cells)*0.8:
			line = line.add(styleBarMiddle, "█")
		default:
			line = line.add(styleBarHigh, "█")
		}
	}
	return merge(line)
}

// merge joins neighbouring spans of one style, so a bar is a few escapes, not one a cell.
func merge(line topLine) topLine {
	merged := topLine{}
	for _, span := range line {
		if count := len(merged); count > 0 && merged[count-1].style == span.style {
			merged[count-1].text += span.text
			continue
		}
		merged = append(merged, span)
	}
	return merged
}

func barCells(width int) int {
	return min(max(width/4, 10), 40)
}

func machineSection(state topState, width int) topSection {
	machine, cells := state.Machine, barCells(width)
	section := topSection{heading: topLine{}.add(styleHeading, "machine").add(styleDim, fmt.Sprintf(" · %d threads", machine.Cpus))}
	cpu := topLine{}.add(styleLabel, " cpu  ")
	if machine.Busy >= 0 {
		cpu = append(append(cpu, bar(machine.Busy, cells)...), topSpan{styleNone, fmt.Sprintf(" %3.0f%%", machine.Busy*100)})
	} else {
		cpu = append(append(cpu, bar(0, cells)...), topSpan{styleDim, "  --%"})
	}
	if machine.HasLoad {
		cpu = cpu.add(styleDim, "  load ").add(styleNone, fmt.Sprintf("%.2f %.2f %.2f", machine.Load[0], machine.Load[1], machine.Load[2]))
	}
	section.always(cpu)
	memory := topLine{}.add(styleLabel, " mem  ")
	if machine.MemoryTotal > 0 {
		used := machine.MemoryTotal - min(machine.MemoryAvailable, machine.MemoryTotal)
		fraction := float64(used) / float64(machine.MemoryTotal)
		memory = append(append(memory, bar(fraction, cells)...), topSpan{styleNone, fmt.Sprintf(" %3.0f%%", fraction*100)})
		memory = memory.add(styleDim, "  "+formatBytes(int64(used))+" of "+formatBytes(int64(machine.MemoryTotal)))
	} else {
		memory = memory.add(styleDim, "not read here (no /proc)")
	}
	section.always(memory)
	for _, disk := range state.Disks {
		line := topLine{}.add(styleLabel, " disk ")
		used := 0.0
		if disk.Total > 0 {
			used = float64(disk.Total-min(disk.Free, disk.Total)) / float64(disk.Total)
		}
		line = append(line, bar(used, cells)...)
		line = line.add(styleNone, " "+formatBytes(int64(disk.Free))+" free").add(styleDim, " of "+formatBytes(int64(disk.Total)))
		if disk.Floor > 0 {
			style := styleDim
			if disk.Free < disk.Floor {
				style = styleFailed
			}
			line = line.add(style, ", floor "+formatBytes(int64(disk.Floor)))
		}
		section.always(line.add(styleDim, " · "+disk.Names))
	}
	if caches := state.Caches; caches.Read {
		line := topLine{}.add(styleLabel, " kept ").add(styleNone, "blobs "+formatBytes(caches.BlobBytes)).add(styleDim, " of "+formatBytes(caches.BlobBound)+fmt.Sprintf(" in %d", caches.Blobs))
		line = line.add(styleDim, " · ").add(styleNone, fmt.Sprintf("sources %d", caches.Sources)).add(styleDim, ", "+formatBytes(caches.SourceBytes))
		section.always(line.add(styleDim, " · ").add(styleNone, fmt.Sprintf("runners %d", caches.Runners)))
	}
	// The units fill rows of their own, the first always shown and the rest as the height allows.
	rows := []topLine{}
	row := topLine{}.add(styleLabel, " units")
	for _, unit := range state.Units {
		style, mark := stylePassed, "●"
		switch unit.Active {
		case "active":
		case "activating", "reloading":
			style = styleBusy
		case "failed":
			style, mark = styleFailed, "✕"
		default:
			style, mark = styleDim, "○"
		}
		entry := topLine{}.add(style, " "+mark+" ").add(styleNone, unit.Name).add(styleDim, " "+unit.Sub)
		if !unit.Since.IsZero() && unit.Active == "active" {
			entry = entry.add(styleDim, " "+formatDuration(state.Now.Sub(unit.Since)))
		}
		if row.width()+entry.width() > width && row.width() > 6 {
			rows, row = append(rows, row), topLine{}.add(styleLabel, "      ")
		}
		row = append(row, entry...)
	}
	if len(state.Units) > 0 {
		rows = append(rows, row)
		section.always(rows[0])
		section.optionalRows(rows[1:]...)
	}
	return section
}

// liveUnit is the unit in hand at its fullest: serve's, with its runner's phase, fetches and tests when the runner
// keeps a status of its own.
func liveUnit(state topState) *livestatus.Unit {
	if state.Serve == nil || state.Serve.Unit == nil || !state.ServeAlive {
		return nil
	}
	unit := *state.Serve.Unit
	if state.Running != nil && state.Running.Unit != nil {
		running := state.Running.Unit
		unit.Fetches, unit.Tests = running.Fetches, running.Tests
		if running.Phase != livestatus.PhaseStarting {
			unit.Phase = running.Phase
		}
		if !running.Deadline.IsZero() {
			unit.StartedAt, unit.Deadline = running.StartedAt, running.Deadline
		}
	}
	return &unit
}

func serveSection(state topState, width int) topSection {
	section := topSection{heading: topLine{}.add(styleHeading, "serve")}
	serve := state.Serve
	if serve == nil {
		section.always(topLine{}.add(styleDim, " "+state.ServeNote))
		return section
	}
	section.heading = section.heading.add(styleNone, " "+serve.Pool).add(styleDim, " · "+serve.Worker)
	switch {
	case !state.ServeAlive && serve.Stopped != "":
		section.heading = section.heading.add(styleBroken, " · stopped "+serve.Stopped+", "+formatDuration(state.Now.Sub(serve.UpdatedAt))+" ago")
	case !state.ServeAlive:
		section.heading = section.heading.add(styleBroken, " · not running, last seen "+formatDuration(state.Now.Sub(serve.UpdatedAt))+" ago")
	default:
		section.heading = section.heading.add(styleDim, " · up "+formatDuration(state.Now.Sub(serve.StartedAt)))
		if !serve.AskedAt.IsZero() {
			section.heading = section.heading.add(styleDim, " · asked "+formatDuration(state.Now.Sub(serve.AskedAt))+" ago")
		}
	}
	unit := liveUnit(state)
	switch {
	case unit != nil:
		section.always(unitLines(*unit, state.Now, width)...)
	case state.ServeAlive && serve.Unfit != "":
		section.always(topLine{}.add(styleFailed, " unfit: "+serve.Unfit))
	case state.ServeAlive:
		section.always(topLine{}.add(styleDim, " idle, asking the pool"))
	}
	totals := serve.Totals
	section.always(topLine{}.add(styleLabel, " ran  ").add(styleNone, fmt.Sprintf("%d units", totals.Units)).
		add(styleDim, ": ").add(stylePassed, fmt.Sprintf("%d passed", totals.Passed)).add(styleDim, ", ").add(styleFailed, fmt.Sprintf("%d failed", totals.Failed)).
		add(styleDim, ", ").add(styleBroken, fmt.Sprintf("%d broken", totals.Broken)).add(styleDim, " · "+formatBytes(totals.Fetched)+" fetched since "+serve.StartedAt.UTC().Format("15:04Z")))
	for _, recent := range serve.Recent {
		glyph, style := verdictMark(recent.Verdict)
		section.optionalRows(topLine{}.add(style, " "+glyph+" ").add(styleNone, fmt.Sprintf("%-28s", truncate(recent.Unit, 28))).
			add(styleDim, fmt.Sprintf(" %7s %7s  %s  ", formatDuration(time.Duration(recent.Seconds*float64(time.Second))), formatBytes(recent.Fetched), recent.FinishedAt.UTC().Format("15:04:05"))).
			add(styleDim, recent.Run))
	}
	return section
}

// unitLines are the unit in hand: what it is, where it is against its deadline, what it fetched and its tests.
func unitLines(unit livestatus.Unit, now time.Time, width int) []topLine {
	what := topLine{}.add(styleBusy, " ▶ ").add(styleNone, unit.Unit).add(styleDim, " "+unit.Run)
	if unit.Package != "" {
		what = what.add(styleDim, " · ").add(styleNone, unit.Package)
		if unit.Packages > 1 {
			what = what.add(styleDim, fmt.Sprintf(" +%d", unit.Packages-1))
		}
	}
	if unit.Shard != "" {
		what = what.add(styleDim, " "+unit.Shard)
	}
	elapsed := now.Sub(unit.StartedAt)
	where := topLine{}.add(styleLabel, fmt.Sprintf("   %-15s", unit.Phase)).add(styleNone, fmt.Sprintf("%7s ", formatDuration(elapsed)))
	if !unit.Deadline.IsZero() {
		total := unit.Deadline.Sub(unit.StartedAt)
		where = append(where, bar(float64(elapsed)/float64(max(total, time.Second)), barCells(width))...)
		where = where.add(styleDim, " of "+formatDuration(total))
	}
	if unit.Runner != "" {
		where = where.add(styleDim, " · runner "+unit.Runner)
	}
	lines := []topLine{what, where}
	for _, place := range []string{livestatus.FromStore, livestatus.FromCache} {
		line := topLine{}.add(styleLabel, fmt.Sprintf("   %-15s", "from the "+place))
		found := false
		for _, fetch := range unit.Fetches {
			if fetch.From != place {
				continue
			}
			if found {
				line = line.add(styleDim, " · ")
			}
			line = line.add(styleNone, fetch.What).add(styleDim, fmt.Sprintf(" %d, %s", fetch.Count, formatBytes(fetch.Bytes)))
			found = true
		}
		if found {
			lines = append(lines, line)
		}
	}
	if tests := unit.Tests; tests != (livestatus.Tests{}) {
		lines = append(lines, topLine{}.add(styleLabel, fmt.Sprintf("   %-15s", "tests")).add(stylePassed, fmt.Sprintf("%d passed", tests.Passed)).
			add(styleDim, "  ").add(styleFailed, fmt.Sprintf("%d failed", tests.Failed)).add(styleDim, fmt.Sprintf("  %d skipped", tests.Skipped)))
	}
	return lines
}

func verdictMark(verdict string) (string, topStyle) {
	switch verdict {
	case protocol.StatusPassed, treebuilder.Built:
		return "✓", stylePassed
	case protocol.StatusFailed: // a failed build's event too, treebuilder.Failed
		return "✕", styleFailed
	default:
		return "⚠", styleBroken
	}
}

func treeSection(state topState, width int) topSection {
	section := topSection{heading: topLine{}.add(styleHeading, "tree builder")}
	if tree := state.Tree; tree != nil && tree.Tree != nil {
		build := tree.Tree
		building := state.TreeAlive && build.Phase != "built" && build.Phase != "failed"
		if building {
			section.heading = section.heading.add(styleBusy, " · building "+shortHash(build.Key))
			line := topLine{}.add(styleLabel, " phase       ").add(styleBusy, build.Phase).add(styleNone, " "+formatDuration(state.Now.Sub(build.StartedAt)))
			if build.Future != "" {
				line = line.add(styleDim, " · future "+shortHash(build.Future))
			}
			line = line.add(styleDim, fmt.Sprintf(" · %d packages, %d product tests", build.Packages, build.ProductTests))
			section.always(line)
			products := topLine{}.add(styleLabel, " products    ").add(stylePassed, fmt.Sprintf("%d hit in the store", build.ProductsHit))
			if build.Products > 0 {
				products = products.add(styleDim, ", ").add(styleBusy, fmt.Sprintf("%d built", max(0, build.Products-build.ProductsHit))).add(styleDim, fmt.Sprintf(" of %d", build.Products))
			}
			section.always(products)
			treePhaseRows(&section, build.Phases, width)
		} else {
			section.heading = section.heading.add(styleDim, " · idle")
			glyph, style := verdictMark(build.Phase)
			section.always(topLine{}.add(style, " "+glyph+" ").add(styleNone, "last "+shortHash(build.Key)).
				add(styleDim, fmt.Sprintf(" %s %s ago", build.Phase, formatDuration(state.Now.Sub(tree.UpdatedAt)))))
			treePhaseRows(&section, build.Phases, width)
		}
	}
	for _, record := range state.Builds {
		glyph, style := verdictMark(record.Event)
		at, _ := time.Parse(time.RFC3339, record.At)
		line := topLine{}.add(style, " "+glyph+" ").add(styleNone, fmt.Sprintf("%-11s %s", record.Event, shortHash(record.Tree))).
			add(styleDim, fmt.Sprintf(" future %s %7s  %s", shortHash(record.Future), formatDuration(time.Duration(record.Seconds*float64(time.Second))), at.UTC().Format("15:04:05")))
		if record.Cause != "" {
			line = line.add(styleDim, "  "+record.Cause)
		}
		section.optionalRows(line)
	}
	return section
}

// treePhaseRows are a tree build's ended phases, in order, as many to a row as width holds: the first row always
// shown, the rest as the height allows. A phase that counts things says how many.
func treePhaseRows(section *topSection, phases []livestatus.Phase, width int) {
	if len(phases) == 0 {
		return
	}
	label, indent := " phases     ", "            "
	rows := []topLine{}
	row := topLine{}.add(styleLabel, label)
	for _, phase := range phases {
		entry := topLine{}.add(styleNone, " ")
		if phase.Count > 0 {
			entry = entry.add(styleNone, fmt.Sprintf("%d ", phase.Count))
		}
		entry = entry.add(styleDim, phase.Name+" ").add(styleNone, formatPhaseSeconds(phase.Seconds))
		switch {
		case row.width() == len(label):
		case row.width()+2+entry.width() > width:
			rows, row = append(rows, row), topLine{}.add(styleLabel, indent)
		default:
			row = row.add(styleDim, " ·")
		}
		row = append(row, entry...)
	}
	rows = append(rows, row)
	section.always(rows[0])
	section.optionalRows(rows[1:]...)
}

// formatPhaseSeconds is a phase's seconds: tenths under ten seconds, where a phase's change shows, and formatDuration's
// two units above.
func formatPhaseSeconds(seconds float64) string {
	if seconds < 10 {
		return fmt.Sprintf("%.1fs", seconds)
	}
	return formatDuration(time.Duration(seconds * float64(time.Second)))
}

func poolsSection(state topState) topSection {
	section := topSection{heading: topLine{}.add(styleHeading, "pools")}
	if state.PoolsNote != "" {
		section.always(topLine{}.add(styleDim, " "+state.PoolsNote))
		return section
	}
	if state.Pools == nil {
		section.always(topLine{}.add(styleDim, " reading the pools"))
		return section
	}
	for _, pool := range state.Pools {
		line := topLine{}.add(styleNone, " "+pool.Name)
		if pool.Err != "" {
			section.always(line.add(styleFailed, " "+pool.Err))
			continue
		}
		style := styleDim
		if pool.Status.Queued > 0 {
			style = styleBusy
		}
		section.always(line.add(style, fmt.Sprintf(" %d queued", pool.Status.Queued)).add(styleDim, fmt.Sprintf(" · %d workers", len(pool.Status.Workers))))
		for _, worker := range pool.Status.Workers {
			asked := worker.SeenAt
			if at, err := time.Parse(time.RFC3339, worker.SeenAt); err == nil {
				asked = formatDuration(state.Now.Sub(at)) + " ago"
			}
			took := "nothing yet"
			var unit string
			if json.Unmarshal(worker.Took, &unit) == nil && unit != "" {
				took = unit
			}
			section.optionalRows(topLine{}.add(styleDim, "   ").add(styleNone, fmt.Sprintf("%-22s", truncate(worker.Worker, 22))).
				add(styleDim, fmt.Sprintf(" %3d cpus · asked %s · took ", worker.Cpus, asked)).add(styleNone, took))
		}
	}
	return section
}

// queueOrder puts the changes still on their way first, the oldest in its state first, then the finished, newest first.
var queueOrder = map[string]int{"testing": 0, "building": 1, "queued": 2}

func queueSection(state topState) topSection {
	section := topSection{heading: topLine{}.add(styleHeading, "queue")}
	queue := state.Queue
	switch {
	case state.QueueNote != "":
		section.always(topLine{}.add(styleDim, " "+state.QueueNote))
		return section
	case queue == nil:
		section.always(topLine{}.add(styleDim, " reading Queue"))
		return section
	case !queue.Read:
		section.always(topLine{}.add(styleFailed, " "+queue.Err))
		return section
	}
	section.heading = section.heading.add(styleDim, fmt.Sprintf(" · seq %d", queue.Seq))
	head := topLine{}.add(styleLabel, " main ").add(styleNone, shortHash(queue.LandedMain))
	if queue.MainRed {
		head = head.add(styleFailed, " · main is red")
	}
	counts := map[string]int{}
	for _, change := range queue.Changes {
		counts[change.State]++
	}
	states := []string{}
	for _, name := range []string{"testing", "building", "queued", "landed", "red", "parked", "refused"} {
		if counts[name] > 0 {
			states = append(states, fmt.Sprintf("%d %s", counts[name], name))
		}
	}
	head = head.add(styleDim, fmt.Sprintf(" · %d changes", len(queue.Changes)))
	if len(states) > 0 {
		head = head.add(styleDim, ": "+strings.Join(states, ", "))
	}
	section.always(head)
	if queue.Err != "" {
		section.always(topLine{}.add(styleFailed, " "+queue.Err))
	}
	changes := slices.Clone(queue.Changes)
	slices.SortStableFunc(changes, func(left, right topChange) int {
		leftOrder, leftLive := queueOrder[left.State]
		rightOrder, rightLive := queueOrder[right.State]
		switch {
		case leftLive && rightLive && leftOrder != rightOrder:
			return leftOrder - rightOrder
		case leftLive && rightLive:
			return strings.Compare(left.StateSince, right.StateSince)
		case leftLive:
			return -1
		case rightLive:
			return 1
		}
		return strings.Compare(right.StateSince, left.StateSince)
	})
	for _, change := range changes {
		glyph, style := "·", styleDim
		switch change.State {
		case "testing", "building":
			glyph, style = "▶", styleBusy
		case "landed":
			glyph, style = "✓", stylePassed
		case "red":
			glyph, style = "✕", styleFailed
		case "parked", "refused":
			glyph, style = "⚠", styleBroken
		}
		since := ""
		if at, err := time.Parse(time.RFC3339, change.StateSince); err == nil {
			since = formatDuration(state.Now.Sub(at))
		}
		units := change.Units
		section.optionalRows(topLine{}.add(style, fmt.Sprintf(" %s %-8s", glyph, change.State)).add(styleNone, " "+shortHash(change.Sha)).
			add(styleDim, fmt.Sprintf(" %-10s", truncate(change.Owner, 10))).
			add(styleDim, fmt.Sprintf(" %7s  units ", since)).add(styleNone, fmt.Sprintf("%d/%d", units.Passed, units.Planned)).
			add(styleFailed, fmt.Sprintf(" %d✕", units.Failed)).add(styleDim, fmt.Sprintf(" %d void  %s", units.Void, change.Change)))
	}
	return section
}

func shortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	if hash == "" {
		return "-"
	}
	return hash
}

// formatDuration is a duration in its two largest units: 45s, 2m14s, 1h03m, 2d03h.
func formatDuration(duration time.Duration) string {
	seconds := int64(max(duration, 0) / time.Second)
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm%02ds", seconds/60, seconds%60)
	case seconds < 86400:
		return fmt.Sprintf("%dh%02dm", seconds/3600, seconds%3600/60)
	default:
		return fmt.Sprintf("%dd%02dh", seconds/86400, seconds%86400/3600)
	}
}

// formatBytes is a size in binary units, btop's short way: 512B, 3.4K, 410M, 1.2G.
func formatBytes(size int64) string {
	value := float64(max(size, 0))
	for _, unit := range []string{"B", "K", "M", "G", "T"} {
		if value < 1024 || unit == "T" {
			if unit == "B" || value >= 10 {
				return fmt.Sprintf("%.0f%s", value, unit)
			}
			return fmt.Sprintf("%.1f%s", value, unit)
		}
		value /= 1024
	}
	return ""
}
