package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The placer's answer for a declared unit is the planner's stamp, Queue's two keys; an undeclared unit prints nothing;
// a need no tier meets exits 1.
func TestUnitNeedsPrintsThePlannersStamp(t *testing.T) {
	t.Parallel()
	tools := t.TempDir()
	write := func(content string) {
		os.MkdirAll(filepath.Join(tools, "cloud/fast-gate"), 0o755)
		os.WriteFile(filepath.Join(tools, "cloud/fast-gate/unit-needs.json"), []byte(content), 0o644)
	}
	needs := func(arguments ...string) (int, string) {
		var stdout, stderr bytes.Buffer
		code := run(append([]string{"unit-needs", "--gate-tools", tools}, arguments...), &stdout, &stderr)
		return code, stdout.String()
	}
	tiers := `"tiers": [{"name": "codex-strict", "memoryMegabytes": 16384, "cpus": 4}, {"name": "box-strict", "memoryMegabytes": 65536, "cpus": 8}]`
	write(`{"version": 1, ` + tiers + `, "units": [{"package": "stage1/cohere/typeaware", "memoryMegabytes": 24576, "cpus": 4, "record": "fabric ec123b7f"}]}`)
	if code, output := needs("--package", "stage1/cohere/typeaware"); code != 0 || output != "{\"cpus\":4,\"memoryMegabytes\":24576}\n" {
		t.Fatalf("typeaware: exit %d, %q", code, output)
	}
	if code, output := needs("--package", "internal/oracle"); code != 0 || output != "" {
		t.Fatalf("an undeclared unit: exit %d, %q", code, output)
	}
	write(`{"version": 1, ` + tiers + `, "units": [{"package": "stage1/cohere/typeaware", "memoryMegabytes": 131072, "cpus": 4, "record": "fabric ec123b7f"}]}`)
	if code, _ := needs("--package", "stage1/cohere/typeaware"); code != 1 {
		t.Fatalf("a need no tier meets exited %d", code)
	}
}
