package planner

import (
	"strings"
	"testing"

	"github.com/system-inc/loom/protocol"
)

// Every job names the runner its key names, which alone may run it (docs/serving.md): a serving box hands it to that
// runner, whatever release the box itself runs. A product's key names no runner, so its job names none.
func TestEveryJobNamesItsKeysRunner(t *testing.T) {
	t.Parallel()
	sha, base, key, runner := strings.Repeat("c", 40), strings.Repeat("b", 40), strings.Repeat("1", 64), strings.Repeat("e", 64)
	test := KeyParts{Kind: "test", Package: protocol.AdamicModule + "/internal/oracle", Tools: Tools{Go: "go1.27.1", Runner: runner}}
	rerun, err := JobUnitFor(test, sha, strings.Repeat("7", 64))
	if err != nil || rerun.Test.Runner != runner {
		t.Fatalf("a rerun's job names runner %q (%v)", rerun.Test.Runner, err)
	}
	placed, err := FutureJobUnit(key, test, sha, base, nil)
	if err != nil || placed.Test.Runner != runner {
		t.Fatalf("a placed test's job names runner %q (%v)", placed.Test.Runner, err)
	}
	phase := KeyParts{Kind: "phase", Package: protocol.AdamicModule, Select: Select{Run: "wasi fixture-07"}, Tools: Tools{Go: "go1.27.1", Runner: runner},
		GateTools: strings.Repeat("d", 40), Env: GateEnvironment}
	if unit, err := FutureJobUnit(key, phase, sha, base, nil); err != nil || unit.Test.Runner != runner {
		t.Fatalf("a phase's job names runner %q (%v)", unit.Test.Runner, err)
	}
	product := KeyParts{Kind: "product", Package: protocol.AdamicModule + "/internal/lower", Select: Select{Run: "^TestProduct_Lower$"}, Tools: Tools{Go: "go1.27.1"}}
	if unit, err := FutureJobUnit(key, product, sha, base, nil); err != nil || unit.Test.Runner != "" {
		t.Fatalf("a product's job names runner %q (%v)", unit.Test.Runner, err)
	}
}

// A build unit (Kirk's build law, #8j1qygw) is its package's go test job made a build job, under the build kind's
// ceiling, placed and rerun alike. Mutant: the kind refused, or its job not marked build.
func TestABuildUnitIsABuildJob(t *testing.T) {
	t.Parallel()
	sha, base, key, runner := strings.Repeat("c", 40), strings.Repeat("b", 40), strings.Repeat("1", 64), strings.Repeat("e", 64)
	build := KeyParts{Kind: "build", Package: protocol.AdamicModule + "/internal/native", Select: Select{Run: "^(TestBuildsTheArchive)$"},
		Tools: Tools{Go: "go1.27.1", Clang: "clang 20", Runner: runner}}
	placed, err := FutureJobUnit(key, build, sha, base, nil)
	if err != nil || placed.Kind != "build" || !placed.Test.Build || placed.TimeoutSeconds != protocol.KindCeilings["build"] || placed.Test.Runner != runner ||
		strings.Join(placed.Requires, ",") != "go,clang" {
		t.Fatalf("a placed build unit is %+v (%v)", placed, err)
	}
	rerun, err := JobUnitFor(build, sha, strings.Repeat("7", 64))
	if err != nil || !rerun.Test.Build || rerun.Test.Tree != strings.Repeat("7", 64) || rerun.Kind != "build" {
		t.Fatalf("a build unit's rerun is %+v (%v)", rerun, err)
	}
	if !ReadsTreeBuild("build") {
		t.Fatal("a build unit doesn't read its tree's build")
	}
}
