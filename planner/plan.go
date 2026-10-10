package planner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/system-inc/loom/protocol"
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
	// Resources is what the unit needs from its machine (unit-needs.json), for placement only, never keyed.
	Resources *protocol.Resources `json:"resources,omitempty"`
	// Tree is the tree key of the future's tree (ReadTreeIdentity's Key), on every unit that reads its build
	// (ReadsTreeBuild): the build Workshop makes for it (`loom build-trees`), which the placer names on the unit's job
	// (#w7agfa9). Placement only, never keyed: a unit's verdict doesn't depend on which build of its tree ran it.
	Tree string `json:"tree,omitempty"`
}

// ReadsTreeBuild says whether a unit of the kind reads its tree's build: a test or product unit's go test job runs a
// package's test binary from it, and a phase job, run on a checkout, takes only the tree's npm packages from it, since
// no runner installs them (#v03v751).
func ReadsTreeBuild(kind string) bool {
	return kind == "test" || kind == "build" || kind == "product" || kind == "phase"
}

// PlanTree plans every package with tests on a checked-out tree: one test unit per package, keyed by KeyFor and
// decided by Choose against the verdict index. A package the change didn't reach keeps its key and so its verdict;
// selection is the key, so no unit is left out to make the plan smaller.
func PlanTree(tree, gateTools string, tools Tools, index VerdictIndex, uncached bool) ([]PlannedResult, error) {
	return planTree(tree, gateTools, tools, index, uncached, KeyFor, nil, nil)
}

// PlanSelected plans a parity run: exactly the selection's packages, every unit run and none reused, and for a package
// whose tests the selection names, a unit that runs exactly those tests (its run pattern ^(A|B)$).
//
// Its units carry what the box record ran with, as keyed parts (Loom, Oct 10 00:27Z): the gate inputs' manifest, the
// change's paths as ADAMIC_GATE_CHANGED (the file the strict runner writes, sorted and newline-joined), and the sample.
func PlanSelected(tree, gateTools string, tools Tools, selection ParitySelect, inputs ParityInputs) ([]PlannedResult, error) {
	selection.inputs = &inputs
	return planTree(tree, gateTools, tools, MemoryIndex{}, true, KeyFor, &selection, nil)
}

// PlanByKey is the planner's selection by key for a parity future, as proof 3 compares it with the future's uncached
// run (Loom, Oct 10 01:29Z): the parent tree is keyed with the future's own parity parts and every key counted passed,
// then the tree is planned against them, so a unit the change didn't reach reuses on exactly the key its uncached run
// used. One checkout serves both trees in turn.
func PlanByKey(checkout Checkout, parent, sha, gateTools string, tools Tools, selection ParitySelect, inputs ParityInputs) ([]PlannedResult, error) {
	selection.inputs = &inputs
	plan := func(at string, index VerdictIndex, uncached bool) ([]PlannedResult, error) {
		tree, cleanup, err := checkout(at)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		return planTree(tree, gateTools, tools, index, uncached, KeyFor, &selection, nil)
	}
	parentPlan, err := plan(parent, MemoryIndex{}, true)
	if err != nil {
		return nil, fmt.Errorf("parent %s: %w", parent, err)
	}
	index := MemoryIndex{}
	for _, unit := range parentPlan {
		index[unit.UnitKey] = Verdict{UnitKey: unit.UnitKey, Status: "passed", Run: "parent-" + short(parent)}
	}
	return plan(sha, index, false)
}

// ParityInputs are what a parity run's box record ran with: the gate inputs' manifest sha256, the change's paths and
// the sample commit (empty: unset).
type ParityInputs struct {
	GateInputs   string
	ChangedPaths []string
	Sample       string
}

// ChangedPathsFile is the changed-paths file a test job's changedPaths become on the strict runner: sorted here, then
// newline-joined with a trailing newline, as runner/strict.go writes it, so the key hashes what the tests read.
func ChangedPathsFile(directory string, paths []string) (string, error) {
	file, err := os.CreateTemp(directory, "changed-paths-*.txt")
	if err != nil {
		return "", err
	}
	defer file.Close()
	if _, err := file.WriteString(changedPathsContent(paths)); err != nil {
		return "", err
	}
	return file.Name(), nil
}

// ChangedPathsSum is the sha256 a unit's key holds for ADAMIC_GATE_CHANGED when the change touched paths (KeyEnv over
// ChangedPathsFile's file), so the placer can check that the paths it carries are the ones the unit was keyed on.
func ChangedPathsSum(paths []string) string {
	sum := sha256.Sum256([]byte(changedPathsContent(paths)))
	return hex.EncodeToString(sum[:])
}

func changedPathsContent(paths []string) string {
	return strings.Join(sortedCopy(paths), "\n") + "\n"
}

// RunNames is the inverse of a parity unit's run pattern: the exact top-level test names ^(A|B)$ names, and true, or
// nil and false for any other pattern (empty runs every test, so it names none). Judge reads it to check every named
// test ran (Loom's zerorun rule, Oct 10 01:12Z).
func RunNames(run string) ([]string, bool) {
	inner, found := strings.CutPrefix(run, "^(")
	if inner, found = strings.CutSuffix(inner, ")$"); !found || inner == "" {
		return nil, false
	}
	names := []string{}
	for _, quoted := range strings.Split(inner, "|") {
		name := unquoteMeta(quoted)
		if name == "" || regexp.QuoteMeta(name) != quoted {
			return nil, false
		}
		names = append(names, name)
	}
	return names, true
}

// unquoteMeta undoes regexp.QuoteMeta: a backslash escapes the byte after it.
func unquoteMeta(quoted string) string {
	var name strings.Builder
	for index := 0; index < len(quoted); index++ {
		if quoted[index] == '\\' && index+1 < len(quoted) {
			index++
		}
		name.WriteByte(quoted[index])
	}
	return name.String()
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

// PlanChange plans a change's future (Loom, Oct 10 01:4xZ): every tested package, decided against the verdict index,
// each keyed with the gate inputs it runs with, so the corpus and pinned-TypeScript tests run rather than skip into a
// pass. The change's paths reach only the units whose tests import internal/gatesample, the one reader of
// ADAMIC_GATE_CHANGED, read from go list: on every unit they would make each key the change's own and end reuse.
func PlanChange(tree, gateTools string, tools Tools, index VerdictIndex, uncached bool, inputs ParityInputs) ([]PlannedResult, error) {
	return planTree(tree, gateTools, tools, index, uncached, KeyFor, nil, &inputs)
}

// GateSample is the package whose tests read ADAMIC_GATE_CHANGED.
const GateSample = "internal/gatesample"

// gateSampleReaders is the tested packages whose test closure holds internal/gatesample.
func gateSampleReaders(tree, module string) (map[string]bool, error) {
	command := exec.Command("go", "list", "-test", "-f", "{{.ImportPath}}|{{join .Deps \",\"}}", "./...")
	command.Dir = tree
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -test ./...: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	target := module + "/" + GateSample
	readers := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		name, deps, _ := strings.Cut(line, "|")
		owner := strings.SplitN(name, " ", 2)[0]
		for _, dep := range strings.Split(deps, ",") {
			if strings.SplitN(dep, " ", 2)[0] == target {
				readers[strings.TrimSuffix(owner, ".test")] = true
			}
		}
	}
	return readers, nil
}

// A keyFunction makes a unit's key parts. PlanTree's is KeyFor; the selector's mutants are weaker ones.
type keyFunction func(tree, gateTools string, unit Unit, tools Tools, compilerPackages []string) (KeyParts, error)

func planTree(tree, gateTools string, tools Tools, index VerdictIndex, uncached bool, keyFor keyFunction, selection *ParitySelect, change *ParityInputs) ([]PlannedResult, error) {
	module, packages, err := listPackages(tree)
	if err != nil {
		return nil, err
	}
	if selection == nil || len(selection.Packages) == 0 {
		// No selection, or a parity run with none (Release's mutants): every tested package.
		tested := packages[:0]
		for _, listed := range packages {
			if len(listed.TestGoFiles)+len(listed.XTestGoFiles) > 0 {
				tested = append(tested, listed)
			}
		}
		packages = tested
	} else {
		// A parity plan holds the box record's packages as it ran them, one without tests included (go test passes it
		// with no test files), since Queue refuses a plan whose packages differ from the selection.
		listedByPath := map[string]listedPackage{}
		for _, listed := range packages {
			listedByPath[listed.ImportPath] = listed
		}
		packages = []listedPackage{}
		for _, importPath := range selection.Packages {
			listed, found := listedByPath[importPath]
			if !found {
				return nil, fmt.Errorf("selected package %s isn't a package on this tree", importPath)
			}
			packages = append(packages, listed)
		}
		for importPath := range selection.Tests {
			if _, found := listedByPath[importPath]; !found {
				return nil, fmt.Errorf("the selection names tests of %s, which isn't a package on this tree", importPath)
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
	needs, err := LoadUnitNeeds(gateTools)
	if err != nil {
		return nil, err
	}
	var pools []Pool
	if len(needs.Units) > 0 {
		if pools, err = LoadPools(PoolsFile); err != nil {
			return nil, err
		}
	}
	resources := map[string]*protocol.Resources{}
	environment := GateEnvironment
	if selection != nil && selection.inputs != nil {
		environment = map[string]string{}
		for name, value := range GateEnvironment {
			environment[name] = value
		}
		if len(selection.inputs.ChangedPaths) > 0 {
			directory, err := os.MkdirTemp("", "loom-plan-changed-")
			if err != nil {
				return nil, err
			}
			defer os.RemoveAll(directory)
			if environment["ADAMIC_GATE_CHANGED"], err = ChangedPathsFile(directory, selection.inputs.ChangedPaths); err != nil {
				return nil, err
			}
		}
		if selection.inputs.Sample != "" {
			environment["ADAMIC_GATE_SAMPLE"] = selection.inputs.Sample
		}
	}
	changeEnvironment, readers := map[string]string{}, map[string]bool{}
	if change != nil {
		for name, value := range GateEnvironment {
			changeEnvironment[name] = value
		}
		if len(change.ChangedPaths) > 0 {
			directory, err := os.MkdirTemp("", "loom-plan-changed-")
			if err != nil {
				return nil, err
			}
			defer os.RemoveAll(directory)
			if changeEnvironment["ADAMIC_GATE_CHANGED"], err = ChangedPathsFile(directory, change.ChangedPaths); err != nil {
				return nil, err
			}
			if readers, err = gateSampleReaders(tree, module); err != nil {
				return nil, err
			}
		}
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
		if selection != nil && selection.inputs != nil {
			unit.GateInputs, unit.Environment = selection.inputs.GateInputs, environment
		}
		if change != nil {
			unit.GateInputs = change.GateInputs
			if readers[listed.ImportPath] {
				unit.Environment = changeEnvironment
			}
		}
		if selection != nil && len(selection.Tests[listed.ImportPath]) > 0 {
			if unit.Run, err = exactRun(selection.Tests[listed.ImportPath]); err != nil {
				return nil, fmt.Errorf("unit %s: %w", listed.ImportPath, err)
			}
		}
		keyParts, err := keyFor(tree, gateTools, unit, tools, compilers)
		if err != nil {
			return nil, fmt.Errorf("unit %s: %w", listed.ImportPath, err)
		}
		// A gatesample reader keyed without the change's paths would sample nothing changed and pass on a key that
		// can't tell one change from another.
		if change != nil && readers[listed.ImportPath] && keyParts.Env["ADAMIC_GATE_CHANGED"] == "" {
			return nil, fmt.Errorf("unit %s reads ADAMIC_GATE_CHANGED (it imports %s) and was keyed without it", listed.ImportPath, GateSample)
		}
		key, err := UnitKey(keyParts)
		if err != nil {
			return nil, err
		}
		if resources[listed.ImportPath], err = needs.For(directory, unit.Run, pools, tools.Runner); err != nil {
			return nil, fmt.Errorf("unit %s: %w", listed.ImportPath, err)
		}
		planned = append(planned, PlannedUnit{Name: listed.ImportPath, UnitKey: key})
		parts[listed.ImportPath] = keyParts
	}
	choices, err := Choose(planned, index, uncached)
	if err != nil {
		return nil, err
	}
	noReuse, err := LoadNoReuse(NoReuseFile)
	if err != nil {
		return nil, err
	}
	if choices, err = UncheckedReadSets(Unreused(choices, noReuse), parts, ReadSetsDirectory); err != nil {
		return nil, err
	}
	for _, choice := range choices {
		results = append(results, PlannedResult{Name: choice.Name, UnitKey: choice.UnitKey, KeyParts: parts[choice.Name],
			Decision: choice.Action, Reason: choice.Reason, Reused: choice.Reused, Resources: resources[choice.Name]})
	}
	return results, nil
}

// listPackages lists the tree's module path and its packages.
func listPackages(tree string) (string, []listedPackage, error) {
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
		packages = append(packages, listed)
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
