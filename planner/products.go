package planner

import (
	"fmt"
	"strings"
)

// A ProductTest is one product test on a tree, TestProduct_X in a package: Builder's action, which it builds once on
// Workshop and stores under the test's product key.
type ProductTest struct {
	Package   string // import path
	Directory string // the package's directory in the tree, repo-relative
	Test      string // TestProduct_X
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
	parts := map[string]KeyParts{}
	for _, test := range tests {
		packageParts, known := parts[test.Package]
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
			if packageParts, err = KeyFor(tree, gateTools, unit, tools, compilerPackages); err != nil {
				return nil, fmt.Errorf("%s: %w", test.Package, err)
			}
			parts[test.Package] = packageParts
		}
		packageParts.Select = Select{Run: "^" + test.Test + "$"}
		key, err := UnitKey(packageParts)
		if err != nil {
			return nil, err
		}
		keys[test] = key
	}
	return keys, nil
}
