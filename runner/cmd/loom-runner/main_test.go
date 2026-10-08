package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func writeUnit(t testing.TB, argv string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "unit.json")
	unit := `{"run":"r-cli","unit":"cli","argv":` + argv + `,"timeoutSeconds":30}`
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExitCodesFollowTheStatus(t *testing.T) {
	cases := []struct {
		argv string
		code int
		last string
	}{
		{`["true"]`, 0, `"status":"passed"`},
		{`["false"]`, 1, `"status":"failed"`},
		{`["no-such-command-anywhere"]`, 2, `"status":"broken"`},
	}
	for _, test := range cases {
		var stdout, stderr bytes.Buffer
		code := run([]string{"run", "--workspace", t.TempDir(), writeUnit(t, test.argv)}, &stdout, &stderr)
		lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
		if code != test.code || !strings.Contains(lines[len(lines)-1], test.last) {
			t.Errorf("%s: exit %d, last event %s, stderr %s", test.argv, code, lines[len(lines)-1], stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run", filepath.Join(t.TempDir(), "missing.json")}, &stdout, &stderr); code != 2 || stdout.Len() != 0 {
		t.Errorf("an unreadable unit: exit %d, stdout %q", code, stdout.String())
	}
	if code := run(nil, &stdout, &stderr); code != 2 {
		t.Errorf("no arguments: exit %d", code)
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	if builtRunner != "" {
		os.Remove(builtRunner)
		os.Remove(filepath.Dir(builtRunner))
	}
	os.Exit(code)
}

var buildOnce sync.Once
var builtRunner string
var buildError error

// buildRunner compiles the real binary once, so the benchmark measures what a machine would run.
func buildRunner(b *testing.B) string {
	buildOnce.Do(func() {
		directory, err := os.MkdirTemp("", "loom-runner-bench-")
		if err != nil {
			buildError = err
			return
		}
		builtRunner = filepath.Join(directory, "loom-runner")
		output, err := exec.Command("go", "build", "-o", builtRunner, ".").CombinedOutput()
		if err != nil {
			buildError = err
			b.Logf("%s", output)
		}
	})
	if buildError != nil {
		b.Fatal(buildError)
	}
	return builtRunner
}

// BenchmarkTrueBare and BenchmarkTrueThroughRunner together measure the runner's overhead per unit: the
// same argv, run bare and run as a unit (no inputs, outputs or wire), each a fresh process.
func BenchmarkTrueBare(b *testing.B) {
	path, err := exec.LookPath("true")
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if err := exec.Command(path).Run(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTrueThroughRunner(b *testing.B) {
	binary := buildRunner(b)
	unit := writeUnit(b, `["true"]`)
	workspace := b.TempDir()
	for b.Loop() {
		if err := exec.Command(binary, "run", "--workspace", workspace, unit).Run(); err != nil {
			b.Fatal(err)
		}
	}
}
