package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/planner"
)

// The check samples: a new signature is checked, then the keyCheckEvery-th tree after it; always and never decide alone.
func TestTheKeyCheckRunsOnANewSignatureAndThenOneTreeInTen(t *testing.T) {
	base := t.TempDir()
	if due, why, err := keyCheckDue(base, "sampled", "one"); err != nil || !due {
		t.Fatalf("the first tree: due %v (%s) %v", due, why, err)
	}
	for tree := 1; tree < keyCheckEvery; tree++ {
		if due, why, err := keyCheckDue(base, "sampled", "one"); err != nil || due {
			t.Fatalf("tree %d after a check: due %v (%s) %v", tree, due, why, err)
		}
	}
	if due, why, err := keyCheckDue(base, "sampled", "one"); err != nil || !due {
		t.Fatalf("the %dth tree: due %v (%s) %v", keyCheckEvery, due, why, err)
	}
	if due, _, err := keyCheckDue(base, "sampled", "one"); err != nil || due {
		t.Fatalf("the tree after a check: due %v %v", due, err)
	}
	if due, why, err := keyCheckDue(base, "sampled", "two"); err != nil || !due {
		t.Fatalf("a new loom release, unit environment or Go release: due %v (%s) %v", due, why, err)
	}
	if due, _, _ := keyCheckDue(base, "always", "two"); !due {
		t.Error("always didn't check")
	}
	if due, _, _ := keyCheckDue(base, "never", "three"); due {
		t.Error("never checked")
	}
	if _, _, err := keyCheckDue(base, "often", "two"); err == nil {
		t.Error("a mode that isn't one ran")
	}
}

// keyedProductTest is a product test as adamic's are keyed: its product's key reads GOFLAGS, it says hit or miss on
// ADAMIC_BUILD_LOG as buildcache's census does, and on a miss it builds with go, as a GoBuild product would.
const keyedProductTest = `package p

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestProduct_Thing(t *testing.T) {
	sum := sha256.Sum256([]byte("GOFLAGS=" + os.Getenv("GOFLAGS")))
	key := hex.EncodeToString(sum[:])
	outcome := "hit"
	if _, err := os.Stat(filepath.Join(os.Getenv("ADAMIC_BUILD_CACHE_DIR"), key)); err != nil {
		outcome = "miss"
		exec.Command("go", "build", "-o", os.DevNull, ".").Run()
	}
	log, err := os.OpenFile(os.Getenv("ADAMIC_BUILD_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(log, "build thing %s %s 0.01\n", key[:12], outcome)
	log.Close()
}
`

// productKeyUnder is the fixture product's key when its test runs under goflags.
func productKeyUnder(goflags string) string {
	sum := sha256.Sum256([]byte("GOFLAGS=" + goflags))
	return hex.EncodeToString(sum[:])
}

// keyCheckFixture is a one-package tree as build-tree has it before its index goes up: its source in chunks, its
// test binary in Out, and in Cache the product its product test made on Workshop, keyed under workshopFlags.
func keyCheckFixture(t *testing.T, workshopFlags string) (builder.TreeBuild, builder.Source, []builder.TreePackage, map[string][]string, string) {
	t.Helper()
	tree, directory := t.TempDir(), t.TempDir()
	files := map[string]string{"go.mod": "module example.com/keys\n\ngo 1.22\n", "p/p.go": "package p\n", "p/p_test.go": keyedProductTest}
	for name, content := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(tree, name)), 0o755)
		if err := os.WriteFile(filepath.Join(tree, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, arguments := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "fixture"}} {
		if output, err := exec.Command("git", append([]string{"-C", tree}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", arguments, err, output)
		}
	}
	build := builder.TreeBuild{Tree: tree, Cache: filepath.Join(directory, "cache"), Out: filepath.Join(directory, "out")}
	binary := filepath.Join(build.Out, "example.com_keys_p.test")
	command := exec.Command("go", "test", "-c", "-o", binary, "./p")
	command.Dir = tree
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go test -c: %v %s", err, output)
	}
	key := productKeyUnder(workshopFlags)
	if err := os.MkdirAll(filepath.Join(build.Cache, key), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(build.Cache, key, "thing"), []byte("the product\n"), 0o644)
	os.WriteFile(filepath.Join(build.Cache, key+".inputs"), []byte("name thing\nflag GOFLAGS="+workshopFlags+"\n"), 0o644)
	source, err := builder.SourceChunks(tree, nil)
	if err != nil {
		t.Fatal(err)
	}
	release, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		t.Fatal(err)
	}
	built := []builder.TreePackage{{Package: "example.com/keys/p", Directory: "p"}}
	return build, source, built, map[string][]string{"example.com/keys/p": {key}}, strings.TrimSpace(string(release))
}

// unitFlags is GOFLAGS as the unit environment sets it, on Workshop and on a runner alike.
func unitFlags(t *testing.T) string {
	t.Helper()
	for _, variable := range planner.UnitEnvironment(planner.RunnersGoos, planner.RunnersGoarch) {
		if value, found := strings.CutPrefix(variable, "GOFLAGS="); found {
			return value
		}
	}
	t.Fatal("the unit environment sets no GOFLAGS")
	return ""
}

// A product Workshop built under the unit environment is a hit when a runner asks for it: nothing moved.
func TestTheKeyCheckFindsAProductBuiltUnderTheUnitEnvironment(t *testing.T) {
	build, source, built, products, release := keyCheckFixture(t, unitFlags(t))
	checked, err := checkKeys(context.Background(), build, t.TempDir(), built, products, source, release, 2)
	if err != nil {
		t.Fatal(err)
	}
	if checked.Packages != 1 || checked.Hits != 1 || len(checked.Moved) != 0 || len(checked.Failed) != 0 {
		t.Fatalf("checked %d packages, %d hits, moved %v, failed %v", checked.Packages, checked.Hits, checked.Moved, checked.Failed)
	}
}

// The mutant: Workshop builds the product under GOFLAGS=-p=3, as its compile share once rode GOFLAGS (verify run 4),
// so a runner keys it apart. The check names the product and the go build the stand-in refused.
func TestTheKeyCheckNamesAKeyThatMovesBetweenWorkshopAndARunner(t *testing.T) {
	build, source, built, products, release := keyCheckFixture(t, "-p=3")
	checked, err := checkKeys(context.Background(), build, t.TempDir(), built, products, source, release, 2)
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.Join(checked.Moved["example.com/keys/p"], "\n")
	// Workshop's key beside the runner's: the pair says which side drifted.
	want := "thing workshop " + productKeyUnder("-p=3")[:12] + " runner " + productKeyUnder(unitFlags(t))[:12] + " miss"
	if !strings.Contains(moved, want) || !strings.Contains(moved, "a test ran go build") {
		t.Fatalf("moved %q, failed %v", moved, checked.Failed)
	}
}

// The key check copies each product into a unit's cache where the tree's cache holds it, in adamic's local directory
// as a tree's build leaves every product (landable-4), with the tree's pointers beside it, as a runner unpacks them, and
// reads a local product's name from its .inputs there.
//
// Mutants: copyProduct copying from <key> at the top only (the local product isn't found); copyPointers copying none
// (the pointer is missing in the unit's cache).
func TestTheKeyCheckCopiesTheLocalLayoutAndItsPointers(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	key, nameKey := strings.Repeat("a", 64), strings.Repeat("b", 64)
	local := filepath.Join(from, builder.LocalDirectory)
	os.MkdirAll(filepath.Join(local, key), 0o755)
	os.WriteFile(filepath.Join(local, key, "tool"), []byte("the tool"), 0o755)
	os.WriteFile(filepath.Join(local, key+".inputs"), []byte("name the tool\nfile tool.go\n"), 0o644)
	os.WriteFile(filepath.Join(local, key+".lock"), nil, 0o644)
	os.WriteFile(filepath.Join(local, nameKey+".json"), []byte(`{"Name":"the tool"}`), 0o644)
	os.WriteFile(filepath.Join(local, ".pointer-1"), []byte("{}"), 0o644)
	if err := copyProduct(from, to, key); err != nil {
		t.Fatal(err)
	}
	if err := copyPointers(from, to); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{key + "/tool": "the tool", key + ".inputs": "name the tool\nfile tool.go\n", nameKey + ".json": `{"Name":"the tool"}`} {
		if content, err := os.ReadFile(filepath.Join(to, builder.LocalDirectory, name)); err != nil || string(content) != want {
			t.Errorf("the unit's cache holds local/%s as %q: %v", name, content, err)
		}
	}
	for _, name := range []string{key + ".lock", ".pointer-1"} {
		if _, err := os.Stat(filepath.Join(to, builder.LocalDirectory, name)); err == nil {
			t.Errorf("buildcache's own local/%s was copied", name)
		}
	}
	if name := productName(to, key); name != "the_tool" {
		t.Errorf("the local product's name is %q", name)
	}
}
