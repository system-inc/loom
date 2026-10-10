package planner

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func productsFixture(t *testing.T) (tree, gateTools string) {
	t.Helper()
	tree, gateTools = t.TempDir(), t.TempDir()
	writeFiles(t, tree, map[string]string{
		"go.mod":      "module example.com/products\n\ngo 1.22\n",
		"oracle/o.go": "package oracle\n\nconst Version = 1\n",
		"oracle/o_test.go": "package oracle\n\nimport \"testing\"\n\n" +
			"func TestProduct_Oracle(t *testing.T) {}\n\nfunc TestProduct_Stage0(t *testing.T) {}\n\nfunc TestOther(t *testing.T) {}\n" +
			"\n// func TestProduct_Commented(t *testing.T) {}\n",
		"oracle/x_test.go": "package oracle_test\n\nimport \"testing\"\n\nfunc TestProduct_External(t *testing.T) {}\n",
		"oracle/w_test.go": "//go:build windows\n\npackage oracle\n\nimport \"testing\"\n\nfunc TestProduct_Windows(t *testing.T) {}\n",
		"plain/p.go":       "package plain\n",
		"plain/p_test.go":  "package plain\n\nimport \"testing\"\n\nfunc TestPlain(t *testing.T) {}\n",
		"compiler/c.go":    "package compiler\n\nconst Version = 1\n",
		"unrelated/u.go":   "package unrelated\n",
		"cloud/fast-gate/compiler-dependencies.json": `{"packages": {"oracle": ["compiler"]}}`,
	})
	writeFiles(t, gateTools, map[string]string{"cloud/fast-gate/executors.txt": ""})
	for _, arguments := range [][]string{{"init", "-q"}, {"add", "."}} {
		if output, err := exec.Command("git", append([]string{"-C", tree}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", arguments, err, output)
		}
	}
	return tree, gateTools
}

func TestAProductKeyMovesWithItsTestFileItsCompilerAndGoButNotTheRunnerOrAnotherPackage(t *testing.T) {
	tree, gateTools := productsFixture(t)
	declared, err := CompilerDeclarations(tree)
	if err != nil {
		t.Fatal(err)
	}
	tools := Tools{Runner: strings.Repeat("a", 64), Go: "go1.27.2", Clang: "20.1.8", Node: "v24.1.0", WasiSdk: "27"}
	oracle := ProductTest{Package: "example.com/products/oracle", Directory: "oracle", Test: "TestProduct_Oracle"}
	key := func(test ProductTest, tools Tools) string {
		value, err := ProductKey(tree, gateTools, tools, test, declared[test.Directory])
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	base := key(oracle, tools)
	if !Sha256Hex(base) {
		t.Fatalf("a productKey is 64 hex: %q", base)
	}
	stage0 := oracle
	stage0.Test = "TestProduct_Stage0"
	if key(stage0, tools) == base {
		t.Fatal("two product tests of one package share a key")
	}
	otherRunner := tools
	otherRunner.Runner = strings.Repeat("b", 64)
	if key(oracle, otherRunner) != base {
		t.Fatal("the runner binary moved a product's key")
	}
	otherGo := tools
	otherGo.Go = "go1.27.3"
	if key(oracle, otherGo) == base {
		t.Fatal("a different Go left a product's key unchanged")
	}
	for _, edit := range []struct {
		path  string
		moves bool
	}{{"oracle/o_test.go", true}, {"oracle/o.go", true}, {"compiler/c.go", true}, {"unrelated/u.go", false}} {
		path := filepath.Join(tree, filepath.FromSlash(edit.path))
		original, _ := os.ReadFile(path)
		os.WriteFile(path, append(append([]byte{}, original...), []byte("\n// edited\n")...), 0o644)
		moved := key(oracle, tools) != base
		os.WriteFile(path, original, 0o644)
		if moved != edit.moves {
			t.Errorf("editing %s: the product key moved %v, want %v", edit.path, moved, edit.moves)
		}
	}
}

func TestProductKeysGivesEachActionExactlyItsProductKey(t *testing.T) {
	tree, gateTools := productsFixture(t)
	declared, _ := CompilerDeclarations(tree)
	actions := []ProductTest{
		{Package: "example.com/products/oracle", Directory: "oracle", Test: "TestProduct_Oracle"},
		{Package: "example.com/products/oracle", Directory: "oracle", Test: "TestProduct_Stage0"},
	}
	tools := Tools{Runner: strings.Repeat("a", 64), Go: "go1.27.2"}
	keys, err := ProductKeys(tree, gateTools, tools, actions, declared)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, action := range actions {
		one, err := ProductKey(tree, gateTools, tools, action, declared[action.Directory])
		if err != nil {
			t.Fatal(err)
		}
		if keys[action] != one {
			t.Fatalf("%s: ProductKeys %s, ProductKey %s", action.Test, keys[action], one)
		}
		if seen[one] {
			t.Fatalf("two actions share %s", one)
		}
		seen[one] = true
	}
}

func TestListProductTestsFindsEveryProductTestForThisPlatformWithoutCompiling(t *testing.T) {
	tree, _ := productsFixture(t)
	tests, err := ListProductTests(tree, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []ProductTest{
		{Package: "example.com/products/oracle", Directory: "oracle", Test: "TestProduct_External"},
		{Package: "example.com/products/oracle", Directory: "oracle", Test: "TestProduct_Oracle"},
		{Package: "example.com/products/oracle", Directory: "oracle", Test: "TestProduct_Stage0"},
	}
	if !reflect.DeepEqual(tests, want) {
		t.Fatalf("tests %+v", tests)
	}
	if only, err := ListProductTests(tree, []string{"example.com/products/plain"}); err != nil || len(only) != 0 {
		t.Fatalf("a package with no product tests: %+v %v", only, err)
	}
}
