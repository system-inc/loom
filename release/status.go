package release

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

// Silence is how long a box may go unheard before status calls it silent: its updater reports every 5 minutes when
// nothing changes, so twice that and a little is a box whose updater isn't running or can't reach the receiver.
const Silence = 12 * time.Minute

// A BoxStatus is one box as status reads it: its latest report, the version the published manifest gives it, when its
// serve last asked a pool (zero when unknown), and every way it falls short, worded for a person.
type BoxStatus struct {
	Box      string
	Report   *Report
	Wants    string
	Asked    time.Time
	Problems []string
	Notes    []string
}

// Boxes reads every configured box's report against the published manifest. asked, when the pools were read, is
// when each host's serve last asked (by lowercase host); a box whose report carries loom-serve.service and that no pool
// saw ask in 3 minutes is named.
func Boxes(config Config, published Manifest, asked map[string]time.Time, now time.Time) ([]BoxStatus, error) {
	var boxes []BoxStatus
	for _, box := range config.Boxes {
		report, err := ReadReport(filepath.Join(config.State, "reports"), box)
		if err != nil {
			return nil, err
		}
		status := BoxStatus{Box: box, Report: report, Wants: published.For(box), Asked: asked[strings.ToLower(box)]}
		if report == nil {
			status.Problems = append(status.Problems, "NEVER REPORTED: its update.conf needs report = http://<Workshop>"+config.Listen+"/report")
			boxes = append(boxes, status)
			continue
		}
		switch {
		case report.Held != "":
			status.Notes = append(status.Notes, fmt.Sprintf("HELD at %s by its update.conf (the release for it is %s)", short(report.Held), short(status.Wants)))
		case report.Version != status.Wants:
			status.Problems = append(status.Problems, fmt.Sprintf("LAGS: runs %s, the release for it is %s", orNothing(report.Version), short(status.Wants)))
		}
		if report.Version != "" && report.Hooked != report.Version {
			status.Problems = append(status.Problems, fmt.Sprintf("HOOKS FAILING: they last passed for %s (see its update.log)", orNothing(report.Hooked)))
		}
		if report.Refused != "" {
			status.Problems = append(status.Problems, "REFUSED: "+report.Refused)
		}
		if silent := now.Sub(report.Received); silent > Silence {
			status.Problems = append(status.Problems, fmt.Sprintf("SILENT for %s: its updater reports every 5m", silent.Round(time.Minute)))
		}
		if unhealthy := report.Unhealthy(nil); len(unhealthy) > 0 {
			status.Problems = append(status.Problems, "UNHEALTHY: "+strings.Join(unhealthy, ", "))
		}
		if _, serves := report.Service("loom-serve.service"); serves && asked != nil && now.Sub(status.Asked) > 3*time.Minute {
			when := "never, in " + strings.Join(config.Pools, " or ")
			if !status.Asked.IsZero() {
				when = status.Asked.Format(time.RFC3339)
			}
			status.Problems = append(status.Problems, "NOT ASKING: its serve last asked "+when)
		}
		boxes = append(boxes, status)
	}
	return boxes, nil
}

func orNothing(version string) string {
	if version == "" {
		return "nothing"
	}
	return short(version)
}

func ago(now, then time.Time) string {
	if then.IsZero() {
		return "-"
	}
	return now.Sub(then).Round(time.Second).String() + " ago"
}

// WriteStatus prints the release, the watcher, and a line per box; it says whether anything falls short.
func WriteStatus(writer io.Writer, published Manifest, state State, boxes []BoxStatus, now time.Time) bool {
	short := func(version string) string { return orNothing(version) }
	fmt.Fprintf(writer, "release  %s for every box", short(published.Top()))
	if canary := published.CanaryVersion(); canary != "" {
		fmt.Fprintf(writer, "; canary %s for %s", short(canary), strings.Join(published.Canary, ", "))
	}
	fmt.Fprintln(writer)
	problem := false
	switch state.Phase {
	case PhaseIdle, "":
		fmt.Fprintln(writer, "watcher  idle")
	case PhaseDone:
		fmt.Fprintf(writer, "watcher  released %s at %s\n", short(state.Commit), state.Since.Format(time.RFC3339))
	case PhaseStopped:
		problem = true
		fmt.Fprintf(writer, "watcher  STOPPED at %s on %s: %s\n         `loom release rollback` ends a canary; `loom release resume` releases again\n", state.Since.Format(time.RFC3339), short(state.Commit), state.Why)
	case PhaseBefore, PhaseAfter:
		steps, where := state.Order.Before, "before"
		if state.Phase == PhaseAfter {
			steps, where = state.Order.After, "after"
		}
		fmt.Fprintf(writer, "watcher  WAITING since %s: %s's order puts %s %s the fleet; once each is done, `loom release mark %s <step>`\n",
			state.Since.Format(time.RFC3339), short(state.Commit), strings.Join(steps, ", "), where, state.Commit)
	default:
		fmt.Fprintf(writer, "watcher  %s of %s since %s (%s)\n", state.Phase, short(state.Commit), state.Since.Format(time.RFC3339), now.Sub(state.Since).Round(time.Second))
	}
	if len(state.Lagging) > 0 {
		fmt.Fprintf(writer, "         %s's release left %s lagging\n", short(state.Commit), strings.Join(state.Lagging, ", "))
	}
	fmt.Fprintln(writer)
	table := tabwriter.NewWriter(writer, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "box\tversion\tupdated\theld\theard\tasked\tservices\tstate")
	for _, box := range boxes {
		report := box.Report
		if report == nil {
			report = &Report{}
		}
		var services []string
		for _, line := range report.Services {
			service := ParseService(line)
			text := strings.TrimSuffix(service.Name, ".service") + " " + service.State
			if service.Restarts >= 0 {
				text += fmt.Sprintf(" r%d", service.Restarts)
			}
			services = append(services, text)
		}
		held := "-"
		if report.Held != "" {
			held = short(report.Held)
		}
		verdict := "ok"
		if len(box.Problems)+len(box.Notes) > 0 {
			verdict = strings.Join(append(append([]string{}, box.Problems...), box.Notes...), "; ")
		}
		problem = problem || len(box.Problems) > 0
		heard := "-"
		if box.Report != nil {
			heard = ago(now, report.Received)
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", box.Box, short(report.Version), orDash(report.Updated), held, heard, ago(now, box.Asked), orDash(strings.Join(services, ", ")), verdict)
	}
	table.Flush()
	return problem
}

func orDash(text string) string {
	if text == "" {
		return "-"
	}
	return text
}
