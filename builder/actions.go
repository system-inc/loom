package builder

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

	"github.com/system-inc/loom/planner"
)

// productTestPattern finds a product test's declaration in a test file: adamic names every product test
// TestProduct_<name>, and run.py's first wave selects them the same way.
var productTestPattern = regexp.MustCompile(`(?m)^func (TestProduct_[A-Za-z0-9_]+)\(`)

type listedPackage struct {
	ImportPath   string
	Dir          string
	TestGoFiles  []string
	XTestGoFiles []string
}

// ListActions lists every product test on a tree, from go list's test files for this platform (so a file another
// platform's build tags leave out is left out here too), without compiling anything. Packages, when given, keeps only
// those import paths.
func ListActions(tree string, packages []string) ([]Action, error) {
	// The tree's own module, from its go.mod: in a workspace, go list -m names every module the workspace uses.
	goMod, err := os.ReadFile(filepath.Join(tree, "go.mod"))
	if err != nil {
		return nil, err
	}
	module := ""
	for _, line := range strings.Split(string(goMod), "\n") {
		if name, found := strings.CutPrefix(strings.TrimSpace(line), "module "); found {
			module = strings.Trim(strings.TrimSpace(name), `"`)
			break
		}
	}
	if module == "" {
		return nil, fmt.Errorf("%s/go.mod names no module", tree)
	}
	command := exec.Command("go", "list", "-json", "./...")
	command.Dir = tree
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("go list ./...: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	wanted := map[string]bool{}
	for _, importPath := range packages {
		wanted[importPath] = true
	}
	actions := []Action{}
	decoder := json.NewDecoder(bytes.NewReader(output))
	for decoder.More() {
		var listed listedPackage
		if err := decoder.Decode(&listed); err != nil {
			return nil, err
		}
		if len(wanted) > 0 && !wanted[listed.ImportPath] {
			continue
		}
		directory := strings.TrimPrefix(strings.TrimPrefix(listed.ImportPath, module), "/")
		seen := map[string]bool{}
		for _, file := range append(append([]string{}, listed.TestGoFiles...), listed.XTestGoFiles...) {
			content, err := os.ReadFile(filepath.Join(listed.Dir, file))
			if err != nil {
				return nil, err
			}
			for _, match := range productTestPattern.FindAllStringSubmatch(string(content), -1) {
				if !seen[match[1]] {
					seen[match[1]] = true
					actions = append(actions, Action{Package: listed.ImportPath, Directory: directory, Test: match[1]})
				}
			}
		}
	}
	sort.Slice(actions, func(left, right int) bool {
		if actions[left].Package != actions[right].Package {
			return actions[left].Package < actions[right].Package
		}
		return actions[left].Test < actions[right].Test
	})
	return actions, nil
}

// CompilerDeclarations reads the tree's cloud/fast-gate/compiler-dependencies.json, package directory to the
// compiler package directories its tests run, as Planner's PlanTree does. A tree without one declares none.
func CompilerDeclarations(tree string) (map[string][]string, error) {
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

// ProductKey is an action's productKey: Planner's KeyFor for the unit that runs exactly this product test, with kind
// product, under the gate's environment (Loom: one key function, never a fork). The runner binary isn't part of it,
// since a product is the same whichever runner asked for it; everything else is the unit key's. Planner's products
// part for a unit is these keys.
func ProductKey(tree, gateTools string, tools planner.Tools, action Action, compilers []string) (string, error) {
	tools.Runner = ""
	module := strings.TrimSuffix(action.Package, "/"+action.Directory)
	if action.Directory == "" || action.Directory == "." {
		module = action.Package
	}
	compilerPackages := []string{}
	for _, directory := range compilers {
		compilerPackages = append(compilerPackages, module+"/"+directory)
	}
	unit := planner.Unit{
		Kind: "product", Package: action.Package, Directory: action.Directory,
		Run: "^" + action.Test + "$", Environment: planner.GateEnvironment,
	}
	parts, err := planner.KeyFor(tree, gateTools, unit, tools, compilerPackages)
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", action.Package, action.Test, err)
	}
	return planner.UnitKey(parts)
}

// ProductKeys is ProductKey for many actions, with KeyFor called once per package: a package's product tests share
// every key part but their selection, so each key is that package's parts with its own run pattern, exactly what
// ProductKey gives (a test holds them equal). On adamic's 649 product tests this is the difference between one go
// list closure per package and one per test.
func ProductKeys(tree, gateTools string, tools planner.Tools, actions []Action, declared map[string][]string) (map[Action]string, error) {
	tools.Runner = ""
	keys := map[Action]string{}
	parts := map[string]planner.KeyParts{}
	for _, action := range actions {
		packageParts, known := parts[action.Package]
		if !known {
			module := strings.TrimSuffix(action.Package, "/"+action.Directory)
			if action.Directory == "" || action.Directory == "." {
				module = action.Package
			}
			compilerPackages := []string{}
			for _, directory := range declared[action.Directory] {
				compilerPackages = append(compilerPackages, module+"/"+directory)
			}
			unit := planner.Unit{Kind: "product", Package: action.Package, Directory: action.Directory, Environment: planner.GateEnvironment}
			var err error
			if packageParts, err = planner.KeyFor(tree, gateTools, unit, tools, compilerPackages); err != nil {
				return nil, fmt.Errorf("%s: %w", action.Package, err)
			}
			parts[action.Package] = packageParts
		}
		packageParts.Select = planner.Select{Run: "^" + action.Test + "$"}
		key, err := planner.UnitKey(packageParts)
		if err != nil {
			return nil, err
		}
		keys[action] = key
	}
	return keys, nil
}

// GateEnvironment is the environment a product test runs with on Workshop: the gate's switches, as a unit's, so
// what buildcache builds here under each key is what a gate unit would ask for.
func GateEnvironment() []string {
	environment := []string{}
	for name, value := range planner.GateEnvironment {
		environment = append(environment, name+"="+value)
	}
	sort.Strings(environment)
	return environment
}
