package planner

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/system-inc/loom/protocol"
)

// A unit whose tests read a WASI gate switch is keyed on the WASI SDK the tree pins and its job requires wasiSdk;
// every other unit keys and requires none, whatever the planner's host says (#vv28ewd: internal/native's key named no
// SDK, so its 36 WASI skips on a box that couldn't link read red).
func TestAUnitThatRunsWasiIsKeyedOnTheTreesSdkAndRequiresIt(t *testing.T) {
	t.Setenv("WASI_SDK_VERSION", "99")
	tree, gateTools := planFixture(t)
	writeFiles(t, tree, map[string]string{
		"cloud/setup.sh": "#!/bin/bash\nif \"$wasiSDK\"; then\n wasiVersion=27\nfi\n",
		"a/wasi_test.go": "package a\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestWASI(t *testing.T) {\n\tif os.Getenv(\"ADAMIC_TEST_WASI\") != \"1\" {\n\t\tt.Skip()\n\t}\n}\n",
	})
	plan := planOf(t, tree, gateTools, MemoryIndex{})
	wasi, plain := plan["example.com/plan/a"].KeyParts, plan["example.com/plan/b"].KeyParts
	if wasi.Tools.WasiSdk != "27" || plain.Tools.WasiSdk != "" {
		t.Fatalf("a's wasiSdk %q (want the tree's 27), b's %q (want none)", wasi.Tools.WasiSdk, plain.Tools.WasiSdk)
	}
	for name, parts := range map[string]KeyParts{"a": wasi, "b": plain} {
		// A job names an adamic package; the fixture's module stands in for it.
		parts.Package = protocol.AdamicModule + "/" + name
		job, err := FutureJobUnit(strings.Repeat("e", 64), parts, strings.Repeat("1", 40), strings.Repeat("2", 40), nil)
		if err != nil {
			t.Fatal(err)
		}
		if requires := slices.Contains(job.Requires, "wasiSdk"); requires != (name == "a") {
			t.Errorf("%s's job requires %v", name, job.Requires)
		}
	}
	// The pin moves a's key and leaves b's.
	os.WriteFile(filepath.Join(tree, "cloud/setup.sh"), []byte(" wasiVersion=28\n"), 0o644)
	moved := planOf(t, tree, gateTools, MemoryIndex{})
	if moved["example.com/plan/a"].UnitKey == plan["example.com/plan/a"].UnitKey || moved["example.com/plan/b"].UnitKey != plan["example.com/plan/b"].UnitKey {
		t.Errorf("a new WASI SDK pin: a's key moved %v, b's held %v", moved["example.com/plan/a"].UnitKey != plan["example.com/plan/a"].UnitKey,
			moved["example.com/plan/b"].UnitKey == plan["example.com/plan/b"].UnitKey)
	}
}

func TestTreeWasiSdkReadsThePinOrNone(t *testing.T) {
	tree := t.TempDir()
	if sdk, err := TreeWasiSdk(tree); err != nil || sdk != "" {
		t.Fatalf("no setup.sh: %q %v", sdk, err)
	}
	for content, want := range map[string]string{" wasiVersion=27\n": "27", "wasiVersion=\"27.0\"\n": "27.0", "# wasiVersion=27\n": "", "wasiVersion=$x\n": ""} {
		writeFiles(t, tree, map[string]string{"cloud/setup.sh": content})
		if sdk, err := TreeWasiSdk(tree); err != nil || sdk != want {
			t.Errorf("%q: %q %v, want %q", content, sdk, err, want)
		}
	}
}
