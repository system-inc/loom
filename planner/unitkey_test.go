package planner

import (
	"reflect"
	"strings"
	"testing"
)

func goldenParts() KeyParts {
	return KeyParts{
		Kind:       "test",
		Package:    "github.com/system-inc/adamic/stage1/cohere/json",
		Select:     Select{Run: "^(TestPortMatchesGoCohere_0001|TestA)$", Skip: ""},
		Closure:    strings.Repeat("a", 64),
		Reads:      strings.Repeat("b", 64),
		Products:   []string{strings.Repeat("c", 64)},
		Tools:      Tools{Runner: strings.Repeat("d", 64), Go: "go1.27.0", Clang: "20.1.0", Node: "v24.1.0", WasiSdk: "27"},
		Env:        map[string]string{"ADAMIC_GATE_UNCACHED": "1", "ADAMIC_LABEL": "<café>"},
		GateInputs: "",
	}
}

// The key is the contract's, not Go's: the expected canonical form and key come from Python's json.dumps with
// sort_keys, compact separators and ensure_ascii off, so a second implementation that follows the contract agrees.
func TestUnitKeyMatchesTheContractGolden(t *testing.T) {
	t.Parallel()
	canonical, err := Canonical(goldenParts())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"closure":"` + strings.Repeat("a", 64) + `","env":{"ADAMIC_GATE_UNCACHED":"1","ADAMIC_LABEL":"<café>"},"gateInputs":"","kind":"test",` +
		`"package":"github.com/system-inc/adamic/stage1/cohere/json","products":["` + strings.Repeat("c", 64) + `"],"reads":"` + strings.Repeat("b", 64) +
		`","select":{"run":"^(TestPortMatchesGoCohere_0001|TestA)$","skip":""},"tools":{"clang":"20.1.0","go":"go1.27.0","node":"v24.1.0","runner":"` +
		strings.Repeat("d", 64) + `","wasiSdk":"27"}}`
	if string(canonical) != want {
		t.Fatalf("canonical form:\n got %s\nwant %s", canonical, want)
	}
	key, err := UnitKey(goldenParts())
	if err != nil {
		t.Fatal(err)
	}
	if key != "91f9c6c7ee812b639e73de8b59758df2830a502039279ba490f6abea9a741b76" {
		t.Fatalf("unit key %s, want the golden 91f9c6c7...", key)
	}
}

// An empty products list and env key the same whether Go holds them as nil or empty: [] and {}, never null.
func TestUnitKeyEmptyListsAreNotNull(t *testing.T) {
	t.Parallel()
	withNil, withEmpty := goldenParts(), goldenParts()
	withNil.Products, withNil.Env = nil, nil
	withEmpty.Products, withEmpty.Env = []string{}, map[string]string{}
	canonical, _ := Canonical(withNil)
	if strings.Contains(string(canonical), "null") {
		t.Fatalf("canonical form holds null: %s", canonical)
	}
	left, _ := UnitKey(withNil)
	right, _ := UnitKey(withEmpty)
	if left != right {
		t.Fatal("nil and empty products or env key differently")
	}
}

// The contract's parts, every leaf of KeyParts. A field added to KeyParts without a mutant here fails below.
var keyPartMutants = map[string]func(*KeyParts){
	"kind":          func(parts *KeyParts) { parts.Kind = "product" },
	"package":       func(parts *KeyParts) { parts.Package += "/other" },
	"select.run":    func(parts *KeyParts) { parts.Select.Run = "^TestA$" },
	"select.skip":   func(parts *KeyParts) { parts.Select.Skip = "^TestB$" },
	"closure":       func(parts *KeyParts) { parts.Closure = strings.Repeat("e", 64) },
	"reads":         func(parts *KeyParts) { parts.Reads = strings.Repeat("e", 64) },
	"products":      func(parts *KeyParts) { parts.Products = append(parts.Products, strings.Repeat("e", 64)) },
	"tools.runner":  func(parts *KeyParts) { parts.Tools.Runner = strings.Repeat("e", 64) },
	"tools.go":      func(parts *KeyParts) { parts.Tools.Go = "go1.27.1" },
	"tools.clang":   func(parts *KeyParts) { parts.Tools.Clang = "20.1.1" },
	"tools.node":    func(parts *KeyParts) { parts.Tools.Node = "v24.2.0" },
	"tools.wasiSdk": func(parts *KeyParts) { parts.Tools.WasiSdk = "28" },
	"env":           func(parts *KeyParts) { parts.Env["ADAMIC_GATE_UNCACHED"] = "0" },
	"gateInputs":    func(parts *KeyParts) { parts.GateInputs = strings.Repeat("e", 64) },
	"gateTools":     func(parts *KeyParts) { parts.GateTools = strings.Repeat("f", 40) },
}

// Contract section 2: every part has a mutant that changes only that part, and it must change the key.
func TestEveryKeyPartChangesTheKey(t *testing.T) {
	t.Parallel()
	base, err := UnitKey(goldenParts())
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range keyPartMutants {
		parts := goldenParts()
		mutate(&parts)
		key, err := UnitKey(parts)
		if err != nil {
			t.Fatal(err)
		}
		if key == base {
			t.Errorf("mutating %s left the unit key unchanged", name)
		}
	}
	// Every leaf of KeyParts has its mutant, so a new part can't join the key untested.
	for _, leaf := range leaves(reflect.TypeOf(KeyParts{}), "") {
		if keyPartMutants[leaf] == nil {
			t.Errorf("key part %s has no mutant in keyPartMutants", leaf)
		}
	}
}

func leaves(kind reflect.Type, prefix string) []string {
	var names []string
	for index := 0; index < kind.NumField(); index++ {
		field := kind.Field(index)
		name := prefix + strings.Split(field.Tag.Get("json"), ",")[0]
		if field.Type.Kind() == reflect.Struct {
			names = append(names, leaves(field.Type, name+".")...)
			continue
		}
		names = append(names, name)
	}
	return names
}
