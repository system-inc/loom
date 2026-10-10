package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/system-inc/loom/gatelines"
	"github.com/system-inc/loom/protocol"
)

// topSnapshot is the part of the board's snapshot `loom top` draws.
type topSnapshot struct {
	Gate *gatelines.Lines `json:"gate"`
	Runs []struct {
		Run     string  `json:"run"`
		Job     string  `json:"job"`
		Units   int     `json:"units"`
		Passed  int     `json:"passed"`
		Failed  int     `json:"failed"`
		Broken  int     `json:"broken"`
		Running int     `json:"running"`
		Queued  int     `json:"queued"`
		Verdict *string `json:"verdict"`
		Active  []struct {
			Unit    string `json:"unit"`
			Machine string `json:"machine"`
			Since   string `json:"since"`
		} `json:"active"`
	} `json:"runs"`
}

var topColors = map[string]string{
	"gating": "\033[34m", "green": "\033[32m", "red": "\033[31;1m", "crash": "\033[33m", "idle": "\033[2m", "loom": "\033[35m",
}
var topGlyphs = map[string]string{"gating": "▶", "green": "✓", "red": "✕", "crash": "⚠", "idle": "·", "loom": "◆"}

const topReset = "\033[0m"

// topBoard is `loom top --board`: it draws the board in the terminal, the same lines the page shows, redrawn every second from the board's
// snapshot (read every two). A busy line's marker turns as it runs.
func topBoard(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("top --board", flag.ContinueOnError)
	flags.SetOutput(stderr)
	wire := flags.String("wire", "https://runs.loom.system.inc", "the wire's origin")
	once := flags.Bool("once", false, "draw once, without colors, and exit")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		fmt.Fprint(stderr, usage)
		return 3
	}
	home, _ := os.UserHomeDir()
	secret, err := protocol.ReadTokenSecret(filepath.Join(home, ".loom", "token-secret"))
	if err != nil {
		fmt.Fprintf(stderr, "loom: %v\n", err)
		return 3
	}
	runContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	client := &http.Client{Timeout: 10 * time.Second}
	fetch := func() (*topSnapshot, error) {
		token, _ := protocol.MintToken(secret, protocol.TokenClaims{Run: protocol.BoardRun, Scope: protocol.ScopeBoard, Expires: time.Now().Add(time.Hour).Unix()})
		request, _ := http.NewRequestWithContext(runContext, http.MethodGet, strings.TrimSuffix(*wire, "/")+"/board/snapshot", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(request)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("the board answered %s", response.Status)
		}
		var snapshot topSnapshot
		return &snapshot, json.NewDecoder(response.Body).Decode(&snapshot)
	}
	if *once {
		snapshot, err := fetch()
		if err != nil {
			fmt.Fprintf(stderr, "loom: %v\n", err)
			return 3
		}
		fmt.Fprint(stdout, drawBoard(snapshot, 0, false, err))
		return 0
	}
	var snapshot *topSnapshot
	var lastError error
	fetched := time.Time{}
	fmt.Fprint(stdout, "\033[?25l")
	defer fmt.Fprint(stdout, "\033[?25h")
	for frame := 0; runContext.Err() == nil; frame++ {
		if time.Since(fetched) >= 2*time.Second {
			if next, err := fetch(); err == nil {
				snapshot, lastError = next, nil
			} else {
				lastError = err
			}
			fetched = time.Now()
		}
		if snapshot != nil {
			fmt.Fprint(stdout, "\033[H\033[2J"+drawBoard(snapshot, frame, true, lastError))
		}
		select {
		case <-runContext.Done():
		case <-time.After(time.Second):
		}
	}
	return 0
}

func drawBoard(snapshot *topSnapshot, frame int, color bool, lastError error) string {
	paint := func(state string, text string) string {
		if !color {
			return text
		}
		return topColors[state] + text + topReset
	}
	spinner := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	elapsed := func(since *string) string {
		if since == nil {
			return ""
		}
		at, err := time.Parse(time.RFC3339, *since)
		if err != nil {
			return ""
		}
		seconds := int(time.Since(at).Seconds())
		if seconds >= 3600 {
			return fmt.Sprintf("%dh %02dm", seconds/3600, seconds%3600/60)
		}
		return fmt.Sprintf("%dm %02ds", seconds/60, seconds%60)
	}
	var text strings.Builder
	heading := "Loom"
	if snapshot.Gate != nil {
		heading += "  gate lines read " + elapsed(&snapshot.Gate.At) + " ago"
	}
	if lastError != nil {
		heading += "  (" + lastError.Error() + ")"
	}
	text.WriteString(heading + "\n")
	machines := []gatelines.Machine{}
	if snapshot.Gate != nil {
		machines = append(machines, snapshot.Gate.Machines...)
	}
	// Loom's own units join their machine's lines, as on the page.
	for _, run := range snapshot.Runs {
		if run.Verdict != nil {
			continue
		}
		for _, active := range run.Active {
			index := -1
			for position, machine := range machines {
				if strings.EqualFold(machine.Name, active.Machine) {
					index = position
				}
			}
			if index < 0 {
				machines = append(machines, gatelines.Machine{Name: active.Machine})
				index = len(machines) - 1
			}
			since := active.Since
			machines[index].Lines = append(machines[index].Lines, gatelines.Line{Slot: -1, State: "loom", Kind: "loom", Branch: run.Job, Sha: active.Unit, Step: run.Run, Since: &since})
		}
	}
	for _, machine := range machines {
		busy := 0
		for _, line := range machine.Lines {
			if line.State != "idle" {
				busy++
			}
		}
		name := machine.Name
		if len(machine.Aliases) > 0 {
			name += " (also " + strings.Join(machine.Aliases, ", ") + ")"
		}
		text.WriteString(fmt.Sprintf("\n%s  %d cores  %d busy\n", strings.ToUpper(name[:1])+name[1:], machine.Cores, busy))
		for _, line := range machine.Lines {
			glyph := topGlyphs[line.State]
			if (line.State == "gating" || line.State == "loom") && color {
				glyph = spinner[frame%len(spinner)]
			}
			label := ""
			switch {
			case line.Slot > 0:
				label = fmt.Sprintf("slot %d %s", line.Slot, line.Class)
			case line.Slot < 0:
				label = "loom"
			}
			what := line.Detail
			if line.Branch != "" {
				what = fmt.Sprintf("%s %s · %s · %s", line.Branch, line.Sha, line.Step, line.State)
				if line.Star {
					what = "★ " + what
				}
			}
			if what == "" {
				what = "idle"
			}
			row := fmt.Sprintf("    %s %-12s %-64s %9s", glyph, label, truncate(what, 64), elapsed(line.Since))
			text.WriteString(paint(line.State, row) + "\n")
			if line.State == "red" || line.State == "crash" {
				text.WriteString(paint(line.State, "        "+truncate(line.Detail, 100)) + "\n")
			}
		}
	}
	return text.String()
}

func truncate(text string, width int) string {
	runes := []rune(text)
	if len(runes) <= width {
		return text
	}
	return string(runes[:width-1]) + "…"
}
