// Package gatelines reads what Adamic's fast gate and whole gate are doing, machine by machine and slot by
// slot, from their own files only, and turns it into the board's gate lines (docs/protocol.md, "The gate's
// lines"). It is read-only on the gate's state: the watcher's directory on this Mac (slots, running/,
// running-started/, front) and, over ssh, each running gate's status.txt and box.txt on its box.
package gatelines

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Lines is the document the board keeps: every machine, each with one line per slot.
type Lines struct {
	At       string    `json:"at"`
	Machines []Machine `json:"machines"`
}

type Machine struct {
	Name    string   `json:"name"`
	Aliases []string `json:"aliases"`
	Cores   int      `json:"cores"`
	Lines   []Line   `json:"lines"`
}

// A Line is one slot: what runs there (branch, short sha, class, the step with the latest activity), since
// when, and its state: gating, green, red, crash (a box or tool failure, no real verdict) or idle.
type Line struct {
	Slot   int     `json:"slot"`
	Class  string  `json:"class"`
	State  string  `json:"state"`
	Kind   string  `json:"kind"` // "fast", "full" or "" when idle
	Branch string  `json:"branch"`
	Sha    string  `json:"sha"`
	Step   string  `json:"step"`
	Since  *string `json:"since"`
	Star   bool    `json:"star"`
	Detail string  `json:"detail"`
}

// Box is one machine the board shows, by the ssh name the gate uses, with its other names.
type Box struct {
	Name    string
	Aliases []string
}

// Boxes are every machine Kirk's board names, in his order. Cloud is Threadripper under a second ssh name.
var Boxes = []Box{
	{Name: "home"}, {Name: "server"}, {Name: "threadripper", Aliases: []string{"cloud"}}, {Name: "workshop"},
	{Name: "sun1"}, {Name: "sun2"}, {Name: "chonchon"},
}

// A Reader polls the gate's files. Its memory holds each slot's last verdict for a while after its gate ends,
// so a red stays on the line long enough to be seen.
type Reader struct {
	State   string        // the watcher's directory, ~/.adamic-fast-gate-watch
	Full    string        // the whole gate's directory, ~/.adamic-full-gate
	Linger  time.Duration // how long a finished gate's verdict stays on its slot
	Run     func(ctx context.Context, box string, script string, arguments ...string) (string, error)
	mutex   sync.Mutex
	cores   map[string]int
	ended   map[string]endedLine // "box slot" to the verdict its last gate left
	running map[string]Line      // "box slot" to the line last seen running there
	now     func() time.Time
}

type endedLine struct {
	line Line
	at   time.Time
}

func NewReader(state string, full string) *Reader {
	return &Reader{State: state, Full: full, Linger: 90 * time.Second, Run: runSSH, cores: map[string]int{},
		ended: map[string]endedLine{}, running: map[string]Line{}, now: time.Now}
}

// runSSH runs a script on a box through a shared connection, so polling every few seconds costs one handshake
// a minute per box.
func runSSH(runContext context.Context, box string, script string, arguments ...string) (string, error) {
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		quoted[index] = "'" + strings.ReplaceAll(argument, "'", `'\''`) + "'"
	}
	command := exec.CommandContext(runContext, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=4",
		"-o", "ControlMaster=auto", "-o", "ControlPath=/tmp/loom-gate-%C", "-o", "ControlPersist=120",
		box, "bash -c "+"'"+strings.ReplaceAll(script, "'", `'\''`)+"'"+" gate-lines "+strings.Join(quoted, " "))
	output, err := command.Output()
	return string(output), err
}

// wholeGateBoxes names each box a whole gate holds now, with when it took the box. Every whole gate, a loop's or one
// started by hand on any box, claims its box as <Full>/boxes/<box>/holder ("<pid> <sha>"); a claim whose process is
// gone holds nothing (Oct 9: the Threadripper read idle on the board through a hand-started whole gate).
func (reader *Reader) wholeGateBoxes() map[string]time.Time {
	held := map[string]time.Time{}
	entries, _ := os.ReadDir(filepath.Join(reader.Full, "boxes"))
	for _, entry := range entries {
		path := filepath.Join(reader.Full, "boxes", entry.Name(), "holder")
		content, err := os.ReadFile(path)
		info, statError := os.Stat(path)
		if err != nil || statError != nil {
			continue
		}
		pid, _ := strconv.Atoi(strings.Fields(string(content) + " 0")[0])
		if pid > 0 && syscall.Kill(pid, 0) == nil {
			held[entry.Name()] = info.ModTime()
		}
	}
	return held
}

// A gate is one entry of the watcher's running directory.
type gate struct {
	branch, sha, slotClass, box, class string
	since                              time.Time
}

func (reader *Reader) readRunning() []gate {
	var gates []gate
	entries, _ := os.ReadDir(filepath.Join(reader.State, "running"))
	for _, entry := range entries {
		content, err := os.ReadFile(filepath.Join(reader.State, "running", entry.Name()))
		if err != nil {
			continue
		}
		fields := strings.Fields(string(content))
		if len(fields) < 3 {
			continue
		}
		found := gate{branch: fields[0], sha: fields[1], slotClass: fields[2], box: "threadripper", class: fields[2]}
		// An entry from before the table named no box: it was Cloud's, which is Threadripper.
		if len(fields) >= 4 {
			found.box = fields[3]
		}
		if len(fields) >= 5 {
			found.class = fields[4]
		}
		if started, err := os.ReadFile(filepath.Join(reader.State, "running-started", entry.Name())); err == nil {
			if seconds, err := strconv.ParseInt(strings.TrimSpace(string(started)), 10, 64); err == nil {
				found.since = time.Unix(seconds, 0)
			}
		}
		gates = append(gates, found)
	}
	return gates
}

// slotsTable reads the watcher's slot table into each box's count of B and S slots.
func (reader *Reader) slotsTable() map[string]map[string]int {
	table := map[string]map[string]int{}
	file, err := os.Open(filepath.Join(reader.State, "slots"))
	if err != nil {
		return table
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		if table[fields[0]] == nil {
			table[fields[0]] = map[string]int{}
		}
		table[fields[0]][fields[1]]++
	}
	return table
}

// front reads the globs of branches integration ranks first: the star's.
func (reader *Reader) front() []string {
	content, err := os.ReadFile(filepath.Join(reader.State, "front"))
	if err != nil {
		return nil
	}
	var globs []string
	for _, line := range strings.Split(string(content), "\n") {
		line, _, _ = strings.Cut(line, "#")
		if line = strings.TrimSpace(line); line != "" {
			globs = append(globs, line)
		}
	}
	return globs
}

func starred(branch string, globs []string) bool {
	for _, glob := range globs {
		if matched, _ := path.Match(glob, branch); matched {
			return true
		}
	}
	return false
}

// boxScript reports, for each 12-character sha given, its newest run directory's status line, slot and the
// file most recently written there. With "full" first it reports the whole gate's newest run instead.
const boxScript = `cores=$(nproc --all 2>/dev/null || sysctl -n hw.ncpu)
echo "cores|${cores}"
if [ "${1:-}" = full ]; then
  d=$(ls -td ~/full-gate/out/*/ 2>/dev/null | head -1)
  [ -n "${d}" ] || exit 0
  d=${d%/}
  name=$(basename "${d}")
  running=no; tmux has-session -t "full-${name%%-*}" 2>/dev/null && running=yes
  newest=$(ls -t "${d}" | grep -vE '^(status.txt|full.json|driver.log|run.sh)$' | head -1)
  echo "full|${name}|${running}|$(head -1 "${d}/status.txt" 2>/dev/null)|${newest}"
  exit 0
fi
for s in "$@"; do
  d=$(ls -td ~/fast-gate/out/${s}-*/ 2>/dev/null | head -1)
  [ -n "${d}" ] || { echo "gate|${s}||||"; continue; }
  d=${d%/}
  slot=$(grep -o 'slot=[0-9]*' "${d}/box.txt" 2>/dev/null | cut -d= -f2)
  newest=$(ls -t "${d}" | grep -vE '^(status.txt|box.txt|fast.json|changed-paths.txt)$' | head -1)
  echo "gate|${s}|${slot}|$(head -1 "${d}/status.txt" 2>/dev/null)|${newest}"
done
`

// stepOf names the step behind the file a gate wrote last.
func stepOf(file string) string {
	switch {
	case file == "":
		return "starting"
	case strings.HasPrefix(file, "tests-") || file == "test.jsonl" || strings.HasPrefix(file, "test-") || strings.HasPrefix(file, "long-tests"):
		return "tests"
	case strings.HasPrefix(file, "a-check"):
		return "a-check"
	case strings.HasPrefix(file, "stage3"):
		return "stage3"
	case strings.HasPrefix(file, "wasi"):
		return "wasi"
	}
	stem := strings.TrimSuffix(strings.TrimSuffix(file, ".log"), ".stderr")
	stem, _, _ = strings.Cut(stem, "-")
	return stem
}

// stateOf reads a status line: green, red, crash (void, or a box that lacked a tool), or gating.
func stateOf(status string) string {
	switch {
	case strings.HasPrefix(status, "green:"):
		return "green"
	case strings.HasPrefix(status, "red:"):
		return "red"
	case strings.HasPrefix(status, "void:"):
		return "crash"
	}
	return "gating"
}

// Read polls everything once and returns the lines.
func (reader *Reader) Read(readContext context.Context) Lines {
	reader.mutex.Lock()
	defer reader.mutex.Unlock()
	now := reader.now()
	gates := reader.readRunning()
	table := reader.slotsTable()
	globs := reader.front()
	byBox := map[string][]gate{}
	for _, found := range gates {
		byBox[found.box] = append(byBox[found.box], found)
	}
	boxes := append([]Box(nil), Boxes...)
	known := map[string]bool{}
	for _, box := range boxes {
		known[box.Name] = true
		for _, alias := range box.Aliases {
			known[alias] = true
		}
	}
	for name := range table {
		if !known[name] {
			boxes = append(boxes, Box{Name: name})
			known[name] = true
		}
	}

	type report struct {
		output string
		err    error
	}
	held := reader.wholeGateBoxes()
	reports := make([]report, len(boxes))
	var wait sync.WaitGroup
	for index, box := range boxes {
		wait.Add(1)
		go func() {
			defer wait.Done()
			callContext, cancel := context.WithTimeout(readContext, 8*time.Second)
			defer cancel()
			var arguments []string
			if _, whole := held[box.Name]; whole || box.Name == "home" {
				arguments = append(arguments, "full")
			} else {
				for _, found := range byBox[box.Name] {
					arguments = append(arguments, found.sha[:min(12, len(found.sha))])
				}
				// A gate that ended lately is asked about too, for its final verdict.
				for key, ended := range reader.ended {
					if strings.HasPrefix(key, box.Name+" ") && now.Sub(ended.at) < reader.Linger {
						arguments = append(arguments, ended.line.Sha)
					}
				}
			}
			output, err := reader.Run(callContext, box.Name, boxScript, arguments...)
			reports[index] = report{output, err}
		}()
	}
	wait.Wait()

	lines := Lines{At: now.UTC().Format(time.RFC3339)}
	for index, box := range boxes {
		machine := Machine{Name: box.Name, Aliases: box.Aliases}
		if machine.Aliases == nil {
			machine.Aliases = []string{}
		}
		statuses := map[string][3]string{} // sha12 to slot, status, newest file
		var full []string
		for _, line := range strings.Split(reports[index].output, "\n") {
			fields := strings.Split(line, "|")
			switch {
			case len(fields) == 2 && fields[0] == "cores":
				reader.cores[box.Name], _ = strconv.Atoi(fields[1])
			case len(fields) == 5 && fields[0] == "gate":
				statuses[fields[1]] = [3]string{fields[2], fields[3], fields[4]}
			case len(fields) == 5 && fields[0] == "full":
				full = fields[1:]
			}
		}
		machine.Cores = reader.cores[box.Name]
		for key, ended := range reader.ended {
			if status, ok := statuses[ended.line.Sha]; ok && strings.HasPrefix(key, box.Name+" ") && status[1] != "" {
				ended.line.State = stateOf(status[1])
				if ended.line.State == "gating" {
					// Its process is gone but its status never left running: it died before a verdict.
					ended.line.State = "crash"
				}
				ended.line.Detail = status[1]
				reader.ended[key] = ended
			}
		}

		// Every slot the table gives the box, numbered the gate's way: the area slot is 1, small slots 2 up,
		// and a box under 32 CPUs has one slot only.
		classes := map[int]string{}
		if counts := table[box.Name]; counts != nil {
			if counts["B"] > 0 || (machine.Cores > 0 && machine.Cores < 32) {
				classes[1] = "B"
			}
			for number := 0; number < counts["S"] && machine.Cores >= 32; number++ {
				classes[2+number] = "S"
			}
		}
		occupied := map[int]Line{}
		for _, found := range byBox[box.Name] {
			short := found.sha[:min(12, len(found.sha))]
			status := statuses[short]
			slot, _ := strconv.Atoi(status[0])
			if slot == 0 {
				// Not on its box yet: the first free slot of its class.
				for number := 1; number <= 4; number++ {
					if _, taken := occupied[number]; !taken && (classes[number] == found.slotClass || classes[number] == "") {
						slot = number
						break
					}
				}
			}
			since := found.since.UTC().Format(time.RFC3339)
			line := Line{Slot: slot, Class: found.class, State: stateOf(status[1]), Kind: "fast", Branch: found.branch, Sha: short,
				Step: stepOf(status[2]), Since: &since, Star: starred(found.branch, globs), Detail: status[1]}
			if line.State == "gating" {
				line.Detail = ""
			}
			occupied[slot] = line
			if _, ok := classes[slot]; !ok {
				classes[slot] = found.slotClass
			}
		}
		if len(full) == 4 {
			line := Line{Slot: 1, Class: "whole", Kind: "full", Branch: "main", State: stateOf(full[2]), Step: stepOf(full[3]), Detail: full[2]}
			line.Sha, _, _ = strings.Cut(full[0], "-")
			if claimed, whole := held[box.Name]; whole {
				at := claimed.UTC().Format(time.RFC3339)
				line.Since = &at
			} else if started, err := os.ReadFile(filepath.Join(reader.Full, "last-started")); err == nil {
				if at, _, ok := strings.Cut(strings.TrimSpace(string(started)), " "); ok {
					line.Since = &at
				}
			}
			if line.State == "gating" {
				line.Detail = ""
			}
			if full[1] == "yes" || line.State == "gating" {
				line.Step = "full-main · " + line.Step
				occupied[1] = line
				classes[1] = "whole"
			}
		}

		// A slot whose gate just ended stays on the board through the linger even when the table doesn't name it
		// (a borrowed slot, or one lent to Loom), so its verdict, a red above all, is seen.
		// The same goes for a slot that was running at the last poll: its gate may have just ended there.
		for key, ended := range reader.ended {
			if strings.HasPrefix(key, box.Name+" ") && now.Sub(ended.at) < reader.Linger {
				if _, named := classes[ended.line.Slot]; !named {
					classes[ended.line.Slot] = ended.line.Class
				}
			}
		}
		for key, last := range reader.running {
			if strings.HasPrefix(key, box.Name+" ") {
				if _, named := classes[last.Slot]; !named {
					classes[last.Slot] = last.Class
				}
			}
		}
		numbers := make([]int, 0, len(classes))
		for number := range classes {
			numbers = append(numbers, number)
		}
		sort.Ints(numbers)
		for _, number := range numbers {
			key := box.Name + " " + strconv.Itoa(number)
			line, busy := occupied[number]
			if busy {
				reader.running[key] = line
				delete(reader.ended, key)
			} else {
				if last, was := reader.running[key]; was {
					// Its gate ended since the last poll. Its verdict comes from its own status line on the
					// next poll; until then the slot says it is ending, never a verdict it hasn't seen.
					last.Step = "ending"
					reader.ended[key] = endedLine{line: last, at: now}
					delete(reader.running, key)
				}
				line = Line{Slot: number, Class: classes[number], State: "idle"}
				if ended, ok := reader.ended[key]; ok && now.Sub(ended.at) < reader.Linger {
					// The verdict stays on the slot for a while, then the slot goes idle.
					line = ended.line
					line.Since = nil
					if line.State != "gating" {
						line.Step = "done"
					}
				} else if ok {
					delete(reader.ended, key)
				}
			}
			machine.Lines = append(machine.Lines, line)
		}
		if len(machine.Lines) == 0 {
			// A machine the gate gives no slot still has its line, idle, so the board shows every machine.
			machine.Lines = []Line{{Slot: 0, State: "idle", Detail: "no gate slots"}}
		}
		if reports[index].err != nil && len(reports[index].output) == 0 && len(byBox[box.Name]) == 0 {
			machine.Lines = append(machine.Lines[:0:0], Line{Slot: 0, Class: "", State: "idle", Detail: "unreachable over ssh"})
		}
		lines.Machines = append(lines.Machines, machine)
	}
	return lines
}
