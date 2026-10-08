package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// cacheKeyVersion changes whenever the key's encoding does, so an old entry can never match a new key.
const cacheKeyVersion = "loom-cache-v1"

// A cacheKeyPart is one thing a unit's result depends on, named so a test can drop it and watch the key go blind.
type cacheKeyPart struct {
	name  string
	value any
}

// cacheKeyParts is everything a unit's result depends on. The run id, unit id, resources, store, wire and
// token are not here: they say where a unit ran, not what it computed.
func cacheKeyParts(unit Unit, runnerVersion string, platform string) []cacheKeyPart {
	environment := make([][2]string, 0, len(unit.Environment))
	for name, value := range unit.Environment {
		environment = append(environment, [2]string{name, value})
	}
	sort.Slice(environment, func(left, right int) bool { return environment[left][0] < environment[right][0] })
	inputs := make([][4]string, len(unit.Inputs))
	for index, input := range unit.Inputs {
		inputs[index] = [4]string{input.Path, input.Sha256, input.Mode, input.Archive}
	}
	outputs := make([]string, len(unit.Outputs))
	for index, output := range unit.Outputs {
		outputs[index] = output.Glob
	}
	return []cacheKeyPart{
		{"argv", unit.Argv},
		{"environment", environment},
		{"directory", unit.Directory},
		{"inputs", inputs},
		{"outputs", outputs},
		{"timeoutSeconds", unit.TimeoutSeconds},
		{"runnerVersion", runnerVersion},
		{"platform", platform},
	}
}

// cacheKeyOf hashes the parts in order, leaving out the one named omit (a test's mutant; "" omits none).
func cacheKeyOf(parts []cacheKeyPart, omit string) string {
	hasher := sha256.New()
	hasher.Write([]byte(cacheKeyVersion + "\n"))
	for _, part := range parts {
		if part.name == omit {
			continue
		}
		encoded, err := json.Marshal(part.value)
		if err != nil {
			// Every part is strings, ints or slices of them, so this can't happen.
			panic(err)
		}
		hasher.Write([]byte(part.name + "="))
		hasher.Write(encoded)
		hasher.Write([]byte("\n"))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// CacheKey is the key a unit's result is cached under on a runner version and platform ("linux/amd64").
// Two units with the same key compute the same thing, whichever run or machine they came from.
func CacheKey(unit Unit, runnerVersion string, platform string) string {
	return cacheKeyOf(cacheKeyParts(unit, runnerVersion, platform), "")
}

// A CacheEntry is what the coordinator writes after a unit passes: where it was proved, what it produced,
// and its event log as a blob, so a hit can show the original output.
type CacheEntry struct {
	Key           string        `json:"key"`
	Run           string        `json:"run"`
	Unit          string        `json:"unit"`
	Machine       string        `json:"machine"`
	RunnerVersion string        `json:"runnerVersion"`
	WallSeconds   float64       `json:"wallSeconds"`
	Outputs       []CacheOutput `json:"outputs"`
	Events        string        `json:"events"`
}

type CacheOutput struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// MarshalJSON writes no outputs as [], never null.
func (entry CacheEntry) MarshalJSON() ([]byte, error) {
	type plain CacheEntry
	if entry.Outputs == nil {
		entry.Outputs = []CacheOutput{}
	}
	return json.Marshal(plain(entry))
}

// CheckCacheEntry checks an entry's shape; whether its blobs are in the store is the Worker's check.
func CheckCacheEntry(entry CacheEntry) error {
	if !Sha256Pattern.MatchString(entry.Key) {
		return fmt.Errorf("cache key %q isn't 64 lowercase hex digits", entry.Key)
	}
	if !RunIdPattern.MatchString(entry.Run) || entry.Unit == "" || len(entry.Unit) > MaximumUnitIdLength {
		return fmt.Errorf("a cache entry names the run and unit that proved it")
	}
	if !Sha256Pattern.MatchString(entry.Events) {
		return fmt.Errorf("a cache entry's events is the sha256 of the unit's event log")
	}
	for _, output := range entry.Outputs {
		if output.Path == "" || !Sha256Pattern.MatchString(output.Sha256) || output.Bytes < 0 {
			return fmt.Errorf("cache output %q needs a path, a sha256 and a size", output.Path)
		}
	}
	return nil
}
