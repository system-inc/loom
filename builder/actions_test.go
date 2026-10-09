package builder

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/system-inc/loom/planner"
)

func actionsFixture(t *testing.T) (tree, gateTools string) {
	t.Helper()
	tree, gateTools = t.TempDir(), t.TempDir()
	files := map[string]string{
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
	}
	for path, content := range files {
		file := filepath.Join(tree, filepath.FromSlash(path))
		os.MkdirAll(filepath.Dir(file), 0o755)
		os.WriteFile(file, []byte(content), 0o644)
	}
	os.MkdirAll(filepath.Join(gateTools, "cloud/fast-gate"), 0o755)
	os.WriteFile(filepath.Join(gateTools, "cloud/fast-gate/executors.txt"), nil, 0o644)
	for _, arguments := range [][]string{{"init", "-q"}, {"add", "."}} {
		if output, err := exec.Command("git", append([]string{"-C", tree}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", arguments, err, output)
		}
	}
	return tree, gateTools
}

func TestListActionsFindsEveryProductTestForThisPlatformWithoutCompiling(t *testing.T) {
	tree, _ := actionsFixture(t)
	actions, err := ListActions(tree, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []Action{
		{Package: "example.com/products/oracle", Directory: "oracle", Test: "TestProduct_External"},
		{Package: "example.com/products/oracle", Directory: "oracle", Test: "TestProduct_Oracle"},
		{Package: "example.com/products/oracle", Directory: "oracle", Test: "TestProduct_Stage0"},
	}
	if !reflect.DeepEqual(actions, want) {
		t.Fatalf("actions %+v", actions)
	}
	if only, err := ListActions(tree, []string{"example.com/products/plain"}); err != nil || len(only) != 0 {
		t.Fatalf("a package with no product tests: %+v %v", only, err)
	}
}

func TestWorkshopRunsProductTestsUnderTheGatesEnvironment(t *testing.T) {
	store := newFakeStore()
	var seen []string
	builder := Builder{
		Store: serve(t, store, "workshop"), Scratch: t.TempDir(), Cache: t.TempDir(), Key: func(Action) (string, error) { return keyOf("k"), nil },
		Run: func(_ Action, environment []string) ([]byte, error) {
			seen = environment
			return nil, nil
		},
	}
	builder.Build([]Action{{Directory: "x", Test: "TestProduct_X"}})
	for name, value := range planner.GateEnvironment {
		found := false
		for _, entry := range seen {
			found = found || entry == name+"="+value
		}
		if !found {
			t.Errorf("a product test ran without %s=%s: %v", name, value, seen)
		}
	}
}
