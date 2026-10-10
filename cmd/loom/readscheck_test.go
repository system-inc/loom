package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/system-inc/loom/planner"
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

// Workshop's measure and refresh of a unit's read set (#xryaqdv): a traced run of a unit keyed on its submodule's
// commit records what it read there, so the next plan keys the unit on that set; a later run that reads beyond the set
// exits 1 as Loom's void, its finding marked beyondKey, its key listed no-reuse with why and its clean record taken
// back, and its set grown, which moves the unit's next key. A clean run of a key on a set is recorded clean, which is
// what lets the planner reuse a verdict under it. Mutants that each fail it: the set not recorded; the void's key not
// listed no-reuse; the set not grown by the void's reads; a clean run not recorded clean; a void's clean record kept.
func TestReadsCheckRecordsAReadSetAndVoidsAReadBeyondIt(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")
	saved := planner.ReadSetsDirectory
	t.Cleanup(func() { planner.ReadSetsDirectory = saved })
	root := t.TempDir()
	git := func(directory string, arguments ...string) {
		t.Helper()
		if output, err := exec.Command("git", append([]string{"-C", directory, "-c", "user.name=t", "-c", "user.email=t@t"}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", arguments, err, output)
		}
	}
	write := func(base string, files map[string]string) {
		for name, text := range files {
			path := filepath.Join(base, filepath.FromSlash(name))
			os.MkdirAll(filepath.Dir(path), 0o755)
			os.WriteFile(path, []byte(text), 0o644)
		}
	}
	sub, tree, gateTools := filepath.Join(root, "sub"), filepath.Join(root, "tree"), filepath.Join(root, "tools")
	readSets, noReuse := filepath.Join(root, "read-sets"), filepath.Join(root, "no-reuse")
	write(sub, map[string]string{"go.mod": "module example.com/shim\n\ngo 1.22\n", "shim.go": "package shim\n", "data.txt": "data\n", "other.txt": "other\n"})
	git(root, "init", "-q", sub)
	git(sub, "add", ".")
	git(sub, "commit", "-q", "-m", "one")
	write(tree, map[string]string{
		"go.mod":      "module example.com/rs\n\ngo 1.22\n\nrequire example.com/shim v0.0.0\n\nreplace example.com/shim => ./sub\n",
		"p/p.go":      "package p\n\nimport _ \"example.com/shim\"\n",
		"p/p_test.go": "package p\n\nimport \"testing\"\n\nfunc TestP(t *testing.T) {}\n",
	})
	git(root, "init", "-q", tree)
	git(tree, "submodule", "add", "-q", sub, "sub")
	git(tree, "add", ".")
	write(gateTools, map[string]string{"cloud/fast-gate/executors.txt": "reads p sub\n"})
	planned := func() (planner.PlannedResult, string) {
		t.Helper()
		planner.ReadSetsDirectory = readSets
		unit := planner.Unit{Kind: "test", Package: "example.com/rs/p", Directory: "p", Environment: planner.GateEnvironment, Products: []string{}}
		parts, err := planner.KeyFor(tree, gateTools, unit, planner.Tools{Go: "go1.27.0"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		key, _ := planner.UnitKey(parts)
		content, _ := json.Marshal(planner.PlannedResult{Name: unit.Package, UnitKey: key, KeyParts: parts, Decision: "run"})
		path := filepath.Join(t.TempDir(), "planned.json")
		os.WriteFile(path, content, 0o644)
		return planner.PlannedResult{UnitKey: key, KeyParts: parts}, path
	}
	check := func(partsPath string, reads ...string) (int, string, string) {
		t.Helper()
		lines := ""
		for _, name := range reads {
			lines += `9 openat(AT_FDCWD<` + filepath.Join(tree, "p") + `>, "../sub/` + name + `", O_RDONLY|O_CLOEXEC) = 3` + "\n"
		}
		trace := filepath.Join(t.TempDir(), "trace")
		os.WriteFile(trace, []byte(lines), 0o644)
		var stdout, stderr bytes.Buffer
		code := run([]string{"reads-check", "--tree", tree, "--gate-tools", gateTools, "--package", "example.com/rs/p", "--trace", trace,
			"--key-parts", partsPath, "--read-sets", readSets, "--no-reuse", noReuse}, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	coarse, coarsePath := planned()
	if coarse.KeyParts.ReadSet != "" {
		t.Fatalf("a unit with no read set was keyed on %q", coarse.KeyParts.ReadSet)
	}
	if code, output, errors := check(coarsePath, "data.txt"); code != 0 || output != "" {
		t.Fatalf("the first traced run: exit %d, findings %q, %s", code, output, errors)
	}
	keyed, keyedPath := planned()
	if keyed.KeyParts.ReadSet == "" || keyed.UnitKey == coarse.UnitKey {
		t.Fatal("the recorded read set didn't key the unit's next plan")
	}
	if code, output, errors := check(keyedPath, "data.txt"); code != 0 || output != "" {
		t.Fatalf("a run that read only its set: exit %d, findings %q, %s", code, output, errors)
	}
	if _, err := os.Stat(noReuse); err == nil {
		t.Fatal("a clean run listed a key no-reuse")
	}
	if clean, err := planner.TraceClean(readSets, keyed.UnitKey); !clean || err != nil {
		t.Fatalf("a clean traced run of a key on a read set wasn't recorded clean (%v)", err)
	}
	code, output, errors := check(keyedPath, "data.txt", "other.txt")
	var finding planner.Finding
	if err := json.Unmarshal([]byte(output), &finding); err != nil || code != 1 {
		t.Fatalf("a run beyond its read set: exit %d, output %q (%v)", code, output, err)
	}
	if !finding.BeyondKey || finding.Path != "sub/other.txt" || finding.UnitKey != keyed.UnitKey || finding.ReadSet != keyed.KeyParts.ReadSet {
		t.Fatalf("the finding %+v doesn't name the read beyond the key, the unit's key and its set", finding)
	}
	if !strings.Contains(errors, "Loom's void") || !strings.Contains(errors, "sub/other.txt") {
		t.Fatalf("the void isn't named on stderr: %s", errors)
	}
	listed, err := planner.LoadNoReuse(noReuse)
	if err != nil || !strings.Contains(listed[keyed.UnitKey], "Loom's void") {
		t.Fatalf("the void's key isn't listed no-reuse: %v (%v)", listed, err)
	}
	if clean, _ := planner.TraceClean(readSets, keyed.UnitKey); clean {
		t.Fatal("the void's key is still recorded clean")
	}
	grown, _ := planned()
	if grown.KeyParts.ReadSet == keyed.KeyParts.ReadSet || grown.UnitKey == keyed.UnitKey {
		t.Fatal("the void didn't grow the unit's read set and move its next key")
	}
}

// The shipped planner keys no unit on a read set until runners trace every set-keyed unit (unit-reads review, finding
// 2): its unit names no --read-sets. Mutant that fails it: the unit naming ~/.loom/read-sets.
func TestTheShippedPlannerNamesNoReadSets(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("../../planner/systemd/loom-plan.service")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, "ExecStart=") && strings.Contains(line, "--read-sets") {
			t.Fatalf("the shipped planner keys units on read sets: %s", line)
		}
	}
}
