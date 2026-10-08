package coordinator

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Rollout is the staged-rollout law for runner versions, kept in ~/.loom/rollout.tsv ("version box state",
// state installed or green): a version goes to one box first, and to any other only after a green run on a
// box that has it.
type Rollout struct {
	mutex sync.Mutex
	path  string
	rows  map[string]string // "version\tbox" to "installed" or "green"
}

func LoadRollout(path string) (*Rollout, error) {
	rollout := &Rollout{path: path, rows: map[string]string{}}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return rollout, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) == 3 {
			rollout.rows[fields[0]+"\t"+fields[1]] = fields[2]
		}
	}
	return rollout, scanner.Err()
}

// MayRun says whether a box may run a version, and why not when it may not.
func (rollout *Rollout) MayRun(version string, box string) (bool, string) {
	rollout.mutex.Lock()
	defer rollout.mutex.Unlock()
	if _, has := rollout.rows[version+"\t"+box]; has {
		return true, ""
	}
	var holders []string
	for key, state := range rollout.rows {
		rowVersion, rowBox, _ := strings.Cut(key, "\t")
		if rowVersion != version {
			continue
		}
		if state == "green" {
			return true, ""
		}
		holders = append(holders, rowBox)
	}
	if len(holders) == 0 {
		return true, ""
	}
	return false, fmt.Sprintf("runner %s is on %s and has no green run there yet", version, strings.Join(holders, ", "))
}

// Installed records a box holding a version; Green records a green run on it. Green never goes back.
func (rollout *Rollout) Installed(version string, box string) {
	rollout.set(version, box, "installed")
}

func (rollout *Rollout) Green(version string, box string) {
	rollout.set(version, box, "green")
}

func (rollout *Rollout) set(version string, box string, state string) {
	rollout.mutex.Lock()
	defer rollout.mutex.Unlock()
	key := version + "\t" + box
	if rollout.rows[key] == "green" {
		return
	}
	rollout.rows[key] = state
	if rollout.path == "" {
		return
	}
	var text strings.Builder
	for key, state := range rollout.rows {
		fmt.Fprintf(&text, "%s\t%s\n", key, state)
	}
	temporary := rollout.path + ".tmp"
	if os.WriteFile(temporary, []byte(text.String()), 0o644) == nil {
		os.Rename(temporary, rollout.path)
	}
}
