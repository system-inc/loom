package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The command reds a unit whose trace read a tracked file nothing declares, naming it with the unit's key, and
// passes the same unit when its trace read only its declared inputs.
func TestReadsCheckRedsAnUndeclaredReadAndPassesDeclaredOnes(t *testing.T) {
	t.Parallel()
	tree, gateTools := t.TempDir(), t.TempDir()
	for name, text := range map[string]string{
		"go.mod": "module example.com/rc\n\ngo 1.22\n", "p/p.go": "package p\n",
		"p/p_test.go":         "package p\n\nimport \"testing\"\n\nfunc TestP(t *testing.T) {}\n",
		"p/testdata/case.txt": "case\n", "q/secret.txt": "undeclared\n",
		filepath.Join(gateTools, "cloud/fast-gate/executors.txt"): "# no reads\n",
	} {
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(tree, name)
		}
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte(text), 0o644)
	}
	for _, arguments := range [][]string{{"init", "-q"}, {"add", "."}} {
		if output, err := exec.Command("git", append([]string{"-C", tree}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", arguments, err, output)
		}
	}
	directory := filepath.Join(tree, "p")
	declared := `7 openat(AT_FDCWD<` + directory + `>, "testdata/case.txt", O_RDONLY|O_CLOEXEC) = 3` + "\n" +
		`7 openat(AT_FDCWD<` + directory + `>, "p_test.go", O_RDONLY|O_CLOEXEC) = 4` + "\n"
	key := strings.Repeat("a", 64)
	check := func(trace string) (int, string) {
		path := filepath.Join(t.TempDir(), "trace")
		os.WriteFile(path, []byte(trace), 0o644)
		var stdout, stderr bytes.Buffer
		code := run([]string{"reads-check", "--tree", tree, "--gate-tools", gateTools, "--package", "example.com/rc/p",
			"--trace", path, "--unit-key", key}, &stdout, &stderr)
		return code, stdout.String()
	}
	if code, output := check(declared); code != 0 || output != "" {
		t.Fatalf("declared reads only: exit %d, findings %q", code, output)
	}
	code, output := check(declared + `7 openat(AT_FDCWD<` + directory + `>, "../q/secret.txt", O_RDONLY) = 5` + "\n")
	var finding struct{ UnitKey, Package, Path, Sha256 string }
	if err := json.Unmarshal([]byte(output), &finding); err != nil || code != 1 {
		t.Fatalf("an undeclared read: exit %d, output %q (%v)", code, output, err)
	}
	if finding.Path != "q/secret.txt" || finding.UnitKey != key || finding.Package != "example.com/rc/p" || len(finding.Sha256) != 64 {
		t.Fatalf("the finding %+v doesn't name the read, the unit and its key", finding)
	}
}
