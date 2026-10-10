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

// A ProductTest is one product test on a tree, TestProduct_X in a package: Builder's action, which it builds once on
// Workshop and stores under the test's product key.
type ProductTest struct {
	Package   string // import path
	Directory string // the package's directory in the tree, repo-relative
	Test      string // TestProduct_X
}

// productTestPattern finds a product test's declaration in a test file: adamic names every product test
// TestProduct_<name>, and run.py's first wave selects them the same way.
var productTestPattern = regexp.MustCompile(`(?m)^func (TestProduct_[A-Za-z0-9_]+)\(`)

// ListProductTests lists every product test on a tree, from go list's test files for this platform (so a file another
// platform's build tags leave out is left out here too), without compiling anything. Packages, when given, keeps only
// those import paths.
func ListProductTests(tree string, packages []string) ([]ProductTest, error) {
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
	tests := []ProductTest{}
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
					tests = append(tests, ProductTest{Package: listed.ImportPath, Directory: directory, Test: match[1]})
				}
			}
		}
	}
	sort.Slice(tests, func(left, right int) bool {
		if tests[left].Package != tests[right].Package {
			return tests[left].Package < tests[right].Package
		}
		return tests[left].Test < tests[right].Test
	})
	return tests, nil
}

// CompilerDeclarations is the tree's cloud/fast-gate/compiler-dependencies.json, package directory to the compiler
// package directories its tests run; a tree without one declares none.
func CompilerDeclarations(tree string) (map[string][]string, error) {
	return compilerDeclarations(tree)
}

// ProductKey is a product test's productKey: KeyFor for the unit that runs exactly that test, with kind product,
// under the gate's environment (one key function for Planner and Builder, Loom's rule). The runner binary isn't part
// of it, since a product is the same whichever runner asked for it. A product's own products part stays its declared
// compilers' closures, so a product key never keys itself; a test unit's products part is the ProductKeys of its
// package's product tests and its declared compilers'.
func ProductKey(tree, gateTools string, tools Tools, test ProductTest, compilers []string) (string, error) {
	keys, err := ProductKeys(tree, gateTools, tools, []ProductTest{test}, map[string][]string{test.Directory: compilers})
	if err != nil {
		return "", err
	}
	return keys[test], nil
}

// ProductKeys is ProductKey for many product tests, with KeyFor called once per package: a package's product tests
// share every key part but their selection, so each key is the package's parts with its own run pattern. On adamic's
// 649 product tests that is one go list closure per package instead of one per test (8.7 s against 4 m 28 s on home).
func ProductKeys(tree, gateTools string, tools Tools, tests []ProductTest, declared map[string][]string) (map[ProductTest]string, error) {
	tools.Runner = ""
	keys := map[ProductTest]string{}
	type keyed struct {
		parts KeyParts
		pairs readPairs
	}
	parts := map[string]keyed{}
	for _, test := range tests {
		packageKeyed, known := parts[test.Package]
		if !known {
			module := strings.TrimSuffix(test.Package, "/"+test.Directory)
			if test.Directory == "" || test.Directory == "." {
				module = test.Package
			}
			compilerPackages := []string{}
			for _, directory := range declared[test.Directory] {
				compilerPackages = append(compilerPackages, module+"/"+directory)
			}
			unit := Unit{Kind: "product", Package: test.Package, Directory: test.Directory, Environment: GateEnvironment}
			var err error
			if packageKeyed.parts, packageKeyed.pairs, err = baseKey(tree, gateTools, unit, tools, compilerPackages); err != nil {
				return nil, fmt.Errorf("%s: %w", test.Package, err)
			}
			parts[test.Package] = packageKeyed
		}
		// Each product test is its own unit, so each is keyed on its own read set.
		testParts := packageKeyed.parts
		testParts.Select = Select{Run: "^" + test.Test + "$"}
		if _, err := withReadSet(tree, &testParts, packageKeyed.pairs); err != nil {
			return nil, fmt.Errorf("%s %s: %w", test.Package, test.Test, err)
		}
		key, err := UnitKey(testParts)
		if err != nil {
			return nil, err
		}
		keys[test] = key
	}
	return keys, nil
}

// TestProductKeys is every package's product keys on a tree, once per tree: each package's import path to the sorted
// keys of its product tests, what UnitProducts reads for each test unit.
func TestProductKeys(tree, gateTools string, tools Tools) (map[string][]string, error) {
	tests, err := ListProductTests(tree, nil)
	if err != nil {
		return nil, err
	}
	declared, err := compilerDeclarations(tree)
	if err != nil {
		return nil, err
	}
	keys, err := ProductKeys(tree, gateTools, tools, tests, declared)
	if err != nil {
		return nil, err
	}
	byPackage := map[string][]string{}
	for test, key := range keys {
		byPackage[test.Package] = append(byPackage[test.Package], key)
	}
	for _, packageKeys := range byPackage {
		sort.Strings(packageKeys)
	}
	return byPackage, nil
}

// UnitProducts is a test unit's products part: the product keys of its own package's product tests and of each
// compiler package its declaration names, sorted, never nil.
func UnitProducts(productKeys map[string][]string, importPath string, compilerPackages []string) []string {
	products := []string{}
	for _, owner := range append([]string{importPath}, compilerPackages...) {
		products = append(products, productKeys[owner]...)
	}
	sort.Strings(products)
	return products
}
