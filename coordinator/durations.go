package coordinator

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Durations is each unit's last wall time, by job and planned unit id, kept in a tab-separated file
// (~/.loom/durations.tsv) so the next run places its longest units first.
type Durations struct {
	mutex   sync.Mutex
	path    string
	seconds map[string]float64
}

// LoadDurations reads the file; a missing file is an empty table.
func LoadDurations(path string) (*Durations, error) {
	durations := &Durations{path: path, seconds: map[string]float64{}}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return durations, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) != 3 {
			continue
		}
		if seconds, err := strconv.ParseFloat(fields[2], 64); err == nil {
			durations.seconds[fields[0]+"\t"+fields[1]] = seconds
		}
	}
	return durations, scanner.Err()
}

func (durations *Durations) Get(job string, unit string) (float64, bool) {
	durations.mutex.Lock()
	defer durations.mutex.Unlock()
	seconds, ok := durations.seconds[job+"\t"+unit]
	return seconds, ok
}

// Set records a wall time and rewrites the file, through a temporary file so a crash never leaves half of it.
func (durations *Durations) Set(job string, unit string, seconds float64) {
	durations.mutex.Lock()
	defer durations.mutex.Unlock()
	durations.seconds[job+"\t"+unit] = seconds
	if durations.path == "" {
		return
	}
	var text strings.Builder
	for key, value := range durations.seconds {
		fmt.Fprintf(&text, "%s\t%.3f\n", key, value)
	}
	temporary := durations.path + ".tmp"
	if os.WriteFile(temporary, []byte(text.String()), 0o644) == nil {
		os.Rename(temporary, durations.path)
	}
}
