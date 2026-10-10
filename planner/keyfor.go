package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
)

// gateSwitches is the contract's closed set of ADAMIC_* switches a unit's key holds by value (proposed to Loom,
// Oct 9 23:4xZ). ADAMIC_GATE_CHANGED names a per-run file, so the key holds its contents' sha256, never the path.
// Any other ADAMIC_* switch is refused until it joins the set: a switch the key can't see could change a verdict.
var gateSwitches = map[string]bool{
	"ADAMIC_GATE_UNCACHED": true, "ADAMIC_TEST_WASI": true, "ADAMIC_ORACLE_WASI": true, "ADAMIC_GATE_COHERE": true,
	"ADAMIC_GATE_SAMPLE": true, "ADAMIC_GATE_CHANGED": true,
}

// KeyEnv is the env part: the unit's ADAMIC_* switches, ADAMIC_GATE_CHANGED as its file's sha256.
func KeyEnv(environment map[string]string) (map[string]string, error) {
	env := map[string]string{}
	for name, value := range environment {
		if !strings.HasPrefix(name, "ADAMIC_") {
			continue
		}
		if !gateSwitches[name] {
			return nil, fmt.Errorf("switch %s isn't in the unit key's closed env set; add it to gateSwitches before a unit sets it", name)
		}
		if name == "ADAMIC_GATE_CHANGED" {
			content, err := os.ReadFile(value)
			if err != nil {
				return nil, fmt.Errorf("ADAMIC_GATE_CHANGED %s: %w", value, err)
			}
			sum := sha256.Sum256(content)
			value = hex.EncodeToString(sum[:])
		}
		env[name] = value
	}
	return env, nil
}

// A Unit is what the planner keys: one package's selected tests (or a product, or a phase) on one tree.
type Unit struct {
	Kind        string
	Package     string // import path
	Directory   string // the package's directory in the tree, repo-relative
	Run, Skip   string
	Environment map[string]string
	GateInputs  string
	// Products are a test unit's product keys (UnitProducts over TestProductKeys), computed once per tree. Nil for a
	// test unit is refused, so a caller can't key one without its products by forgetting them.
	Products []string
}

// ProductStub stands in for Builder's productKeys while the planner stubs the builder as "build locally" (contract,
// Stubs): a unit that builds its products itself depends on what builds them, so each product here is the closure
// of a compiler package the unit's compiler-dependencies.json declaration names. Builder's real keys replace it.
func ProductStub(tree string, compilerPackages []string) ([]string, error) {
	products := []string{}
	for _, importPath := range compilerPackages {
		closure, err := Closure(tree, importPath)
		if err != nil {
			return nil, fmt.Errorf("product stub %s: %w", importPath, err)
		}
		products = append(products, closure)
	}
	sort.Strings(products)
	return products, nil
}

// KeyFor assembles a unit's key parts on a tree: its closure from go list, its reads (the declared stub), its
// products, the box's tools and its closed env. The key is UnitKey(parts).
//
// A test unit's products are Builder's product keys (unit.Products), what a runner fetches before the unit starts.
// The compiler packages its tests build at run time are reads outside its closure, so their closures' files join its
// reads: adamic's declared compilers (internal/native, lower, ir and the rest) have no product tests of their own, so
// product keys alone would leave them out. A product's products part stays its compilers' closures (ProductStub), so a
// product key never keys itself.
func KeyFor(tree, gateTools string, unit Unit, tools Tools, compilerPackages []string) (KeyParts, error) {
	closure, err := Closure(tree, unit.Package)
	if err != nil {
		return KeyParts{}, err
	}
	reads, err := DeclaredReads(tree, gateTools, unit.Directory)
	if err != nil {
		return KeyParts{}, err
	}
	var products []string
	if unit.Kind == "product" {
		if products, err = ProductStub(tree, compilerPackages); err != nil {
			return KeyParts{}, err
		}
	} else {
		if unit.Products == nil {
			return KeyParts{}, fmt.Errorf("unit %s: a %s unit's products weren't computed (UnitProducts)", unit.Package, unit.Kind)
		}
		products = append([]string{}, unit.Products...)
		sort.Strings(products)
		read := map[string]bool{}
		for _, file := range reads {
			read[file] = true
		}
		for _, importPath := range compilerPackages {
			files, err := ClosureFiles(tree, importPath)
			if err != nil {
				return KeyParts{}, fmt.Errorf("compiler %s: %w", importPath, err)
			}
			for _, file := range files {
				read[file] = true
			}
		}
		reads = reads[:0]
		for file := range read {
			reads = append(reads, file)
		}
		sort.Strings(reads)
	}
	readsHash, err := ReadsHash(tree, reads)
	if err != nil {
		return KeyParts{}, err
	}
	env, err := KeyEnv(unit.Environment)
	if err != nil {
		return KeyParts{}, err
	}
	return KeyParts{
		Kind: unit.Kind, Package: unit.Package, Select: Select{Run: unit.Run, Skip: unit.Skip},
		Closure: closure, Reads: readsHash, Products: products, Tools: tools, Env: env, GateInputs: unit.GateInputs,
	}, nil
}
