// Package planner decides what a change runs: each unit's key, which keys already have a verdict, and the locks
// that keep a key honest (read tracing, selector mutants, an uncached witness). See docs/contracts.md, section 2.
package planner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// unitKeyVersion is the contract's prefix: a change to the key's shape is a new version, so no old key matches.
const unitKeyVersion = "loom-unit-v1"

// KeyParts is everything a unit's verdict depends on (contract section 2). Where a unit ran, which run and which
// change asked for it are not here: they don't change what it computes.
type KeyParts struct {
	Kind       string            `json:"kind"`
	Package    string            `json:"package"`
	Select     Select            `json:"select"`
	Closure    string            `json:"closure"`
	Reads      string            `json:"reads"`
	Products   []string          `json:"products"`
	Tools      Tools             `json:"tools"`
	Env        map[string]string `json:"env"`
	GateInputs string            `json:"gateInputs"`
	// GateTools is a phase unit's gate tools commit (40 hex), which the runner checks out: a phase runs run.py from the
	// tools, so the key commits to the commit itself (Loom, Oct 10 02:09Z). Empty, and so absent, on every other kind,
	// whose keys stay where they were.
	GateTools string `json:"gateTools,omitempty"`
	// ReadSet is the id of the read set a test or product unit is keyed on (readset.go): its reads part then holds the
	// submodule paths that set names, each by its content, in place of the submodules' commits. Empty, and so absent,
	// until one is recorded for the unit, whose key is then what it was.
	ReadSet string `json:"readSet,omitempty"`
}

// Select is which of the package's tests the unit runs.
type Select struct {
	Run  string `json:"run"`
	Skip string `json:"skip"`
}

// Tools is what runs the unit: the runner binary's sha256 and the toolchains' versions.
type Tools struct {
	Runner  string `json:"runner"`
	Go      string `json:"go"`
	Clang   string `json:"clang"`
	Node    string `json:"node"`
	WasiSdk string `json:"wasiSdk"`
}

// Canonical is the contract's canonical JSON: sorted keys at every depth, no insignificant whitespace, UTF-8 as
// is (no HTML escaping, which another language's canonical form wouldn't do). Empty lists and maps are [] and {},
// never null, so a unit with no products and one with an absent list key the same.
func Canonical(value any) ([]byte, error) {
	plain, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	// Through a generic value, so every object's keys come out sorted whatever the Go field order.
	var generic any
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.UseNumber()
	if err := decoder.Decode(&generic); err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(nullsAsEmpty(generic)); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// nullsAsEmpty turns the nil slices and maps Go marshals as null into [] and {} wherever the contract has a list or
// an object; KeyParts has no field whose absence means anything else.
func nullsAsEmpty(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for name, field := range typed {
			if field == nil {
				switch name {
				case "products":
					typed[name] = []any{}
				case "env":
					typed[name] = map[string]any{}
				}
				continue
			}
			typed[name] = nullsAsEmpty(field)
		}
	case []any:
		for index, item := range typed {
			typed[index] = nullsAsEmpty(item)
		}
	}
	return value
}

// UnitKey is sha256("loom-unit-v1\n" + canonical(keyParts)), 64 lowercase hex: the key Judge keeps a verdict under.
func UnitKey(parts KeyParts) (string, error) {
	canonical, err := Canonical(parts)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(unitKeyVersion+"\n"), canonical...))
	return hex.EncodeToString(sum[:]), nil
}
