// Package lower is the prebuilt runner's fixture (runner/prebuilt_test.go): a test package Workshop would build,
// compiled by the test into a binary the runner runs with no Go.
package lower

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
