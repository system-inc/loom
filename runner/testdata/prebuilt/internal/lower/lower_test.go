// Package lower is the prebuilt runner's fixture (runner/prebuilt_test.go): a test package Workshop would build,
// compiled by the test into a binary the runner runs with no Go.
package lower

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// productKey is the buildcache key of the product TestProduct reads, as the fixture's tree index names it.
const productKey = "abababababababababababababababababababababababababababababababab"

// TestA reads a file of the package's own testdata, which is there only when it runs in its directory of the source.
func TestA(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("testdata", "fixture.txt"))
	if err != nil || string(content) != "read from the unpacked source\n" {
		t.Fatalf("testdata/fixture.txt: %q, %v", content, err)
	}
}

// TestGate sees the gate's switches.
func TestGate(t *testing.T) {
	if os.Getenv("ADAMIC_GATE_UNCACHED") != "1" || os.Getenv("ADAMIC_GATE_COHERE") != "1" {
		t.Fatal("the gate's environment is missing")
	}
	t.Run("Nested", func(t *testing.T) { t.Log("a subtest's line") })
}

// TestProduct reads its product from ADAMIC_BUILD_CACHE_DIR, and builds it with go, as buildcache would, when it
// isn't there.
func TestProduct(t *testing.T) {
	tool := filepath.Join(os.Getenv("ADAMIC_BUILD_CACHE_DIR"), productKey, "tool")
	if content, err := os.ReadFile(tool); err == nil && string(content) == "the product\n" {
		return
	}
	if output, err := exec.Command("go", "build", "-o", tool, "./cmd/tool").CombinedOutput(); err != nil {
		t.Fatalf("building the product: %v: %s", err, output)
	}
}

// TestFail is the change's red.
func TestFail(t *testing.T) {
	t.Fatal("the change broke this")
}

// TestToolKey asks go for its version and environment, as buildcache.Tool and GoInputs do to key a product, and
// passes whatever go says.
func TestToolKey(t *testing.T) {
	output, err := exec.Command("go", "version").CombinedOutput()
	t.Logf("go version: %q %v", output, err)
	output, err = exec.Command("go", "env", "GOVERSION").CombinedOutput()
	t.Logf("go env GOVERSION: %q %v", output, err)
}

// TestSleep runs past any unit's time.
func TestSleep(t *testing.T) {
	time.Sleep(time.Hour)
}

// TestPanic panics.
func TestPanic(t *testing.T) {
	panic("the change panics")
}

// TestExit exits 0 in the middle of the tests, which -test.paniconexit0 turns into a failure.
func TestExit(t *testing.T) {
	os.Exit(0)
}

// TestListExport asks for an allowed go list with GOFLAGS asking for export data, which would compile the package.
func TestListExport(t *testing.T) {
	command := exec.Command("go", "list", "-f", "{{.Export}}", "errors")
	command.Env = append(os.Environ(), "GOFLAGS=-export=true", "GOCACHE="+t.TempDir())
	output, err := command.CombinedOutput()
	if err == nil && len(strings.TrimSpace(string(output))) > 0 {
		t.Fatalf("the allowed go list compiled the package: %s", output)
	}
	t.Logf("%q %v", output, err)
}

// TestModules lists a package that imports a third-party module, as buildcache's GoInputs does.
func TestModules(t *testing.T) {
	output, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "github.com/system-inc/adamic/internal/uses").CombinedOutput()
	if err != nil || !strings.Contains(string(output), "example.com/dep") {
		t.Fatalf("go list -deps: %v: %s", err, output)
	}
}

// TestListBuildMode lists a package's build in another mode, as buildcache's GoInputs keys the checker archive.
func TestListBuildMode(t *testing.T) {
	output, err := exec.Command("go", "list", "-deps", "-json", "-buildmode=c-archive", "github.com/system-inc/adamic/internal/uses").CombinedOutput()
	if err != nil || !strings.Contains(string(output), `"ImportPath": "example.com/dep"`) {
		t.Fatalf("go list -buildmode=c-archive: %v: %s", err, output)
	}
}

// TestListUpdates asks a module proxy for newer versions.
func TestListUpdates(t *testing.T) {
	if output, err := exec.Command("go", "list", "-m", "-u", "all").CombinedOutput(); err != nil {
		t.Fatalf("go list -m -u all: %v: %s", err, output)
	}
}

// TestGoEnv asks go env for the flags and toolchain its own environment sets, as adamic's GoInputs does to key a
// product: the answer must be its own.
func TestGoEnv(t *testing.T) {
	command := exec.Command("go", "env", "GOFLAGS", "GOTOOLCHAIN")
	command.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false -trimpath -p=4", "GOTOOLCHAIN=auto")
	if output, err := command.CombinedOutput(); err != nil || string(output) != "-buildvcs=false -trimpath -p=4\nauto\n" {
		t.Fatalf("go env GOFLAGS GOTOOLCHAIN: %v: %q", err, output)
	}
}

// TestToolexec asks for a go list whose GOFLAGS runs a tool of the test's choosing.
func TestToolexec(t *testing.T) {
	command := exec.Command("go", "list", ".")
	command.Env = append(os.Environ(), "GOFLAGS=-toolexec=/bin/echo")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go list: %v: %s", err, output)
	}
}

// TestOtherToolchain asks for another Go release's go.
func TestOtherToolchain(t *testing.T) {
	command := exec.Command("go", "version")
	command.Env = append(os.Environ(), "GOTOOLCHAIN=go1.99.0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go version: %v: %s", err, output)
	}
}

// TestOutsideModule lists a module of the test's own, outside the tree and its workspace.
func TestOutsideModule(t *testing.T) {
	directory := t.TempDir()
	os.WriteFile(filepath.Join(directory, "go.mod"), []byte("module example.com/outside\n\ngo 1.27\n"), 0o644)
	os.WriteFile(filepath.Join(directory, "outside.go"), []byte("package outside\n"), 0o644)
	command := exec.Command("go", "list", ".")
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil || !strings.Contains(string(output), "example.com/outside") {
		t.Fatalf("go list in a module outside the tree: %v: %s", err, output)
	}
}

// TestWorkspace asks go for the workspace, as adamic's product keys do: go env GOWORK names the tree's own go.work, as
// it did on Workshop, never the runner's copy; go work edit -json reads it; and the tree's go.work, handed back, lists.
func TestWorkspace(t *testing.T) {
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(filepath.Dir(filepath.Dir(directory)), "go.work")
	output, err := exec.Command("go", "env", "GOWORK").CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != tree {
		t.Fatalf("go env GOWORK: %v: %q, and the tree's is %q", err, output, tree)
	}
	output, err = exec.Command("go", "env", "-json", "GOWORK").CombinedOutput()
	if err != nil || !strings.Contains(string(output), `"GOWORK": "`+tree+`"`) {
		t.Fatalf("go env -json GOWORK: %v: %q", err, output)
	}
	output, err = exec.Command("go", "work", "edit", "-json").CombinedOutput()
	if err != nil || !strings.Contains(string(output), `"DiskPath"`) {
		t.Fatalf("go work edit -json: %v: %s", err, output)
	}
	output, err = exec.Command("go", "work", "edit", "-json", tree).CombinedOutput()
	if err != nil || !strings.Contains(string(output), `"DiskPath"`) {
		t.Fatalf("go work edit -json %s: %v: %s", tree, err, output)
	}
	command := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "github.com/system-inc/adamic/internal/uses")
	command.Env = append(os.Environ(), "GOWORK="+tree)
	if output, err := command.CombinedOutput(); err != nil || !strings.Contains(string(output), "example.com/dep") {
		t.Fatalf("go list -deps with the tree's go.work: %v: %s", err, output)
	}
}

// TestWorkEdit edits the workspace, which writes it.
func TestWorkEdit(t *testing.T) {
	if output, err := exec.Command("go", "work", "edit", "-json", "-use=./elsewhere").CombinedOutput(); err != nil {
		t.Fatalf("go work edit: %v: %s", err, output)
	}
}
