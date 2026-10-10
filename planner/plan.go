package planner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// GateEnvironment is the env every test unit runs with today (run.py's gateEnvironment): the four gate switches.
var GateEnvironment = map[string]string{"ADAMIC_GATE_UNCACHED": "1", "ADAMIC_TEST_WASI": "1", "ADAMIC_ORACLE_WASI": "1", "ADAMIC_GATE_COHERE": "1"}

// A PlannedResult is one unit of a future's plan as the planner posts it to Queue (contract v1.1, the planning seam):
// its key, the parts the key was made from, and the decision.
type PlannedResult struct {
	Name     string   `json:"name"`
	UnitKey  string   `json:"unitKey"`
	KeyParts KeyParts `json:"keyParts"`
	Decision string   `json:"decision"` // reuse | run
	Reason   string   `json:"reason"`
	Reused   string   `json:"reused,omitempty"`
}

// PlanTree plans every package with tests on a checked-out tree: one test unit per package, keyed by KeyFor and
// decided by Choose against the verdict index. A package the change didn't reach keeps its key and so its verdict;
// selection is the key, so no unit is left out to make the plan smaller.
func PlanTree(tree, gateTools string, tools Tools, index VerdictIndex, uncached bool) ([]PlannedResult, error) {
	return planTree(tree, gateTools, tools, index, uncached, KeyFor, nil)
}

// PlanSelected plans a parity run: exactly the selection's packages, every unit run and none reused, and for a package
// whose tests the selection names, a unit that runs exactly those tests (its run pattern ^(A|B)$).
func PlanSelected(tree, gateTools string, tools Tools, selection ParitySelect) ([]PlannedResult, error) {
	return planTree(tree, gateTools, tools, MemoryIndex{}, true, KeyFor, &selection)
}

// exactRun is the run pattern that selects exactly the named top-level tests. A subtest's name can't be one, since go
// test splits a -run pattern at its slashes.
func exactRun(tests []string) (string, error) {
	quoted := []string{}
	for _, test := range tests {
		if strings.Contains(test, "/") {
			return "", fmt.Errorf("test %q is a subtest; a parity selection names top-level tests", test)
		}
		quoted = append(quoted, regexp.QuoteMeta(test))
	}
	sort.Strings(quoted)
	return "^(" + strings.Join(quoted, "|") + ")$", nil
}

// A keyFunction makes a unit's key parts. PlanTree's is KeyFor; the selector's mutants are weaker ones.
type keyFunction func(tree, gateTools string, unit Unit, tools Tools, compilerPackages []string) (KeyParts, error)

func planTree(tree, gateTools string, tools Tools, index VerdictIndex, uncached bool, keyFor keyFunction, selection *ParitySelect) ([]PlannedResult, error) {
	module, packages, err := testedPackages(tree)
	if err != nil {
		return nil, err
	}
	if selection != nil {
		tested := map[string]listedPackage{}
		for _, listed := range packages {
			tested[listed.ImportPath] = listed
		}
		packages = packages[:0]
		for _, importPath := range selection.Packages {
			listed, found := tested[importPath]
			if !found {
				return nil, fmt.Errorf("selected package %s has no tests on this tree", importPath)
			}
			packages = append(packages, listed)
		}
		for importPath := range selection.Tests {
			if _, found := tested[importPath]; !found {
				return nil, fmt.Errorf("the selection names tests of %s, which isn't a tested package", importPath)
			}
		}
	}
	declared, err := compilerDeclarations(tree)
	if err != nil {
		return nil, err
	}
	productKeys, err := TestProductKeys(tree, gateTools, tools)
	if err != nil {
		return nil, err
	}
	results := []PlannedResult{}
	planned := []PlannedUnit{}
	parts := map[string]KeyParts{}
	for _, listed := range packages {
		directory := strings.TrimPrefix(strings.TrimPrefix(listed.ImportPath, module), "/")
		compilers := []string{}
		for _, input := range declared[directory] {
			compilers = append(compilers, module+"/"+input)
		}
		unit := Unit{Kind: "test", Package: listed.ImportPath, Directory: directory, Environment: GateEnvironment,
			Products: UnitProducts(productKeys, listed.ImportPath, compilers)}
		if selection != nil && len(selection.Tests[listed.ImportPath]) > 0 {
			if unit.Run, err = exactRun(selection.Tests[listed.ImportPath]); err != nil {
				return nil, fmt.Errorf("unit %s: %w", listed.ImportPath, err)
			}
		}
		keyParts, err := keyFor(tree, gateTools, unit, tools, compilers)
		if err != nil {
			return nil, fmt.Errorf("unit %s: %w", listed.ImportPath, err)
		}
		key, err := UnitKey(keyParts)
		if err != nil {
			return nil, err
		}
		planned = append(planned, PlannedUnit{Name: listed.ImportPath, UnitKey: key})
		parts[listed.ImportPath] = keyParts
	}
	choices, err := Choose(planned, index, uncached)
	if err != nil {
		return nil, err
	}
	for _, choice := range choices {
		results = append(results, PlannedResult{Name: choice.Name, UnitKey: choice.UnitKey, KeyParts: parts[choice.Name],
			Decision: choice.Action, Reason: choice.Reason, Reused: choice.Reused})
	}
	return results, nil
}

// testedPackages lists the tree's module path and its packages that have tests.
func testedPackages(tree string) (string, []listedPackage, error) {
	module, err := modulePath(tree)
	if err != nil {
		return "", nil, err
	}
	command := exec.Command("go", "list", "-json", "./...")
	command.Dir = tree
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return "", nil, fmt.Errorf("go list ./...: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	packages := []listedPackage{}
	decoder := json.NewDecoder(bytes.NewReader(output))
	for decoder.More() {
		var listed listedPackage
		if err := decoder.Decode(&listed); err != nil {
			return "", nil, err
		}
		if len(listed.TestGoFiles)+len(listed.XTestGoFiles) > 0 {
			packages = append(packages, listed)
		}
	}
	sort.Slice(packages, func(left, right int) bool { return packages[left].ImportPath < packages[right].ImportPath })
	return module, packages, nil
}

// modulePath is the tree's main module path.
func modulePath(tree string) (string, error) {
	command := exec.Command("go", "list", "-m")
	command.Dir = tree
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("go list -m: %w", err)
	}
	return strings.TrimSpace(strings.SplitN(string(output), "\n", 2)[0]), nil
}

// compilerDeclarations reads the tree's cloud/fast-gate/compiler-dependencies.json: package directory to the compiler
// package directories its tests run. A tree without one declares none.
func compilerDeclarations(tree string) (map[string][]string, error) {
	content, err := os.ReadFile(filepath.Join(tree, "cloud/fast-gate/compiler-dependencies.json"))
	if os.IsNotExist(err) {
		return map[string][]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var declarations struct {
		Packages map[string][]string `json:"packages"`
	}
	if err := json.Unmarshal(content, &declarations); err != nil {
		return nil, fmt.Errorf("compiler-dependencies.json: %w", err)
	}
	return declarations.Packages, nil
}
