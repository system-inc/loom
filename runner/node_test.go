package runner

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/protocol"
)

// withNodePackages gives the fixture's source stage3/api's package.json and lockfile, and its node_modules as a chunk of
// its own, as Workshop's build-tree makes them, and returns the lockfile's sha256 and that chunk's blob. The index is
// published naming nothing of them; the test names what it means to.
func (fixture *prebuiltFixture) withNodePackages(t *testing.T) (string, string) {
	t.Helper()
	lockfile := `{"name":"api","lockfileVersion":3,"requires":true,"packages":{}}` + "\n"
	files := append(fixtureFiles(),
		tarEntry{name: "stage3/api/package.json", kind: tar.TypeReg, content: `{"name":"api"}` + "\n"},
		tarEntry{name: "stage3/api/package-lock.json", kind: tar.TypeReg, content: lockfile},
		tarEntry{name: "stage3/api/node_modules/@types/node/package.json", kind: tar.TypeReg, content: `{"name":"@types/node","version":"25.3.3"}` + "\n"},
		tarEntry{name: "stage3/api/node_modules/.bin/tsc", kind: tar.TypeSymlink, linkname: "../typescript/bin/tsc"},
		tarEntry{name: "stage3/api/node_modules/typescript/bin/tsc", kind: tar.TypeReg, content: "#!/usr/bin/env node\n", mode: 0o755},
	)
	chunks := fixture.store.putChunks(t, files, append(fixtureCuts, "stage3/api/node_modules/", "stage3/api/package-lock.json")...)
	chunk := ""
	for _, candidate := range chunks {
		if strings.HasPrefix(candidate.First, "stage3/api/node_modules/") && strings.HasPrefix(candidate.Last, "stage3/api/node_modules/") {
			chunk = candidate.Blob
		}
	}
	if chunk == "" {
		t.Fatalf("no chunk holds node_modules alone: %+v", chunks)
	}
	fixture.tree.setSource(t, chunks)
	sum := sha256.Sum256([]byte(lockfile))
	return hex.EncodeToString(sum[:]), chunk
}

// A tree's npm packages arrive in its source, where a checkout's install put them, and nothing on the runner installs
// them (#v03v751): prepare.sh is asked only for the environment, and no npm is anywhere on the runner's PATH.
func TestATreesNpmPackagesArriveInItsSource(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	lockfile, chunk := fixture.withNodePackages(t)
	fixture.tree.index.Node = []builder.NodeProject{{Directory: "stage3/api", Lockfile: lockfile, Chunk: chunk}}
	fixture.tree.publish(t)
	result, events, _ := runUnit(t, fixture.unit("^TestA$"), fixture.options(t))
	if result.Status != protocol.StatusPassed {
		t.Fatalf("%s; errors %q\n%s", result.Status, errorPhases(events), strings.Join(outputLines(events, "runner"), "\n"))
	}
	tree := filepath.Join(fixture.directory, "root", sourceDirectoryName, fixture.tree.source, sourceTreeName)
	content, err := os.ReadFile(filepath.Join(tree, "stage3/api/node_modules/@types/node/package.json"))
	if err != nil || !strings.Contains(string(content), `"25.3.3"`) {
		t.Fatalf("@types/node isn't in the tree: %q %v", content, err)
	}
	if target, err := os.Readlink(filepath.Join(tree, "stage3/api/node_modules/.bin/tsc")); err != nil || target != "../typescript/bin/tsc" {
		t.Errorf("node_modules/.bin/tsc is %q, %v", target, err)
	}
	if info, err := os.Stat(filepath.Join(tree, "stage3/api/node_modules/typescript/bin/tsc")); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Errorf("typescript/bin/tsc isn't executable: %v %v", info, err)
	}
	if prepared, err := os.ReadFile(filepath.Join(fixture.directory, "environment-tree")); err != nil || strings.TrimSpace(string(prepared)) != tree {
		t.Errorf("prepare.sh's environment was asked over %q, %v, not the tree", prepared, err)
	}
}

// phaseNodeJob is a gofmt witness of the fixture's tree, a phase job that names the tree for its npm packages, with
// prepare.sh a stub that records what the root holds of them when it runs, and fails: the placement is what's tested.
func (fixture *prebuiltFixture) phaseNodeJob(t *testing.T, lockfile string) (protocol.Unit, Options, string) {
	t.Helper()
	seen := filepath.Join(fixture.directory, "prepare-saw")
	placed := filepath.Join(fixture.directory, "root", nodeDirectory, lockfile, "node_modules", "@types", "node", "package.json")
	prepareScript = []byte("#!/bin/bash\n[ \"$1\" = trim-only ] && exit 0\nif [ -f '" + placed + "' ]; then echo placed > '" + seen + "'; else echo missing > '" + seen + "'; fi\nexit 2\n")
	job := protocol.TestJob{Repository: protocol.AdamicRepository, Sha: testSha, Base: testSha, Phase: protocol.GofmtPhase, Tools: strings.Repeat("c", 40), Go: "go1.27.2", Tree: fixture.tree.key}
	options := fixture.options(t)
	options.PhaseJobs = true
	return testJobUnit(job), options, seen
}

// A phase job's checkout takes its npm packages from its tree's build (#v03v751): before prepare.sh runs, the runner
// places the chunk Workshop installed them into at <root>/adamic-npm/<lockfile>/node_modules, where prepare.sh links
// them from, once per lockfile; a second unit fetches and unpacks nothing.
func TestAPhaseJobsCheckoutTakesItsNpmPackagesFromItsTree(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	lockfile, chunk := fixture.withNodePackages(t)
	fixture.tree.index.Node = []builder.NodeProject{{Directory: "stage3/api", Lockfile: lockfile, Chunk: chunk}}
	fixture.tree.publish(t)
	unit, options, seen := fixture.phaseNodeJob(t, lockfile)
	_, events, _ := runUnit(t, unit, options)
	runner := strings.Join(outputLines(events, "runner"), "\n")
	if said, _ := os.ReadFile(seen); strings.TrimSpace(string(said)) != "placed" {
		t.Fatalf("prepare.sh saw the packages %q; errors %q\n%s", said, errorPhases(events), runner)
	}
	if !strings.Contains(runner, "placed stage3/api's npm packages, lockfile "+lockfile+", chunk "+chunk) {
		t.Errorf("the placement isn't on the record:\n%s", runner)
	}
	npm := filepath.Join(fixture.directory, "root", nodeDirectory)
	if target, err := os.Readlink(filepath.Join(npm, lockfile, "node_modules", ".bin", "tsc")); err != nil || target != "../typescript/bin/tsc" {
		t.Errorf("node_modules/.bin/tsc is %q, %v", target, err)
	}
	if entries, _ := os.ReadDir(npm); len(entries) != 1 || entries[0].Name() != lockfile {
		t.Errorf("adamic-npm holds %v, and only %s belongs there", entries, lockfile)
	}
	if entries, _ := os.ReadDir(filepath.Join(npm, lockfile)); len(entries) != 1 || entries[0].Name() != "node_modules" {
		t.Errorf("%s holds %v beside node_modules", lockfile, entries)
	}
	before := fixture.store.blobGets()
	os.Remove(seen)
	_, events, _ = runUnit(t, unit, options)
	if said, _ := os.ReadFile(seen); strings.TrimSpace(string(said)) != "placed" || fixture.store.blobGets() != before {
		t.Fatalf("the second unit: prepare.sh saw %q, %d blob GETs after %d", said, fixture.store.blobGets(), before)
	}
	if runner := strings.Join(outputLines(events, "runner"), "\n"); !strings.Contains(runner, "are already here") {
		t.Errorf("the second unit placed them again:\n%s", runner)
	}
}

// A phase job whose tree's packages the store can't give is Loom's, and its checkout is never readied without them.
func TestAPhaseJobWhoseNpmPackagesTheStoreLacksIsLooms(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	lockfile, chunk := fixture.withNodePackages(t)
	fixture.tree.index.Node = []builder.NodeProject{{Directory: "stage3/api", Lockfile: lockfile, Chunk: chunk}}
	fixture.tree.publish(t)
	fixture.store.remove("blobs/" + chunk)
	unit, options, seen := fixture.phaseNodeJob(t, lockfile)
	result, events, _ := runUnit(t, unit, options)
	if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), "the tree's npm packages: stage3/api's npm packages: blob "+chunk+" isn't in the store") {
		t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
	}
	if _, err := os.Stat(seen); err == nil {
		t.Error("prepare.sh ran without the packages")
	}
}

// An index that names no npm packages for a lockfile its source holds, or names them from another lockfile, or names
// some where its source holds no lockfile, is Loom's, and its tests never run: without the packages they read, they
// would fail red, charged to the change.
func TestAnIndexLackingATreesNpmPackagesIsLooms(t *testing.T) {
	for name, test := range map[string]struct {
		node  func(lockfile, chunk string) []builder.NodeProject
		named string
	}{
		"none named": {func(string, string) []builder.NodeProject { return nil }, "names no npm packages installed from it"},
		"another lockfile's": {func(_, chunk string) []builder.NodeProject {
			return []builder.NodeProject{{Directory: "stage3/api", Lockfile: strings.Repeat("e", 64), Chunk: chunk}}
		}, "installed from lockfile " + strings.Repeat("e", 64)},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newPrebuiltFixture(t)
			lockfile, chunk := fixture.withNodePackages(t)
			fixture.tree.index.Node = test.node(lockfile, chunk)
			fixture.tree.publish(t)
			result, events, _ := runUnit(t, fixture.unit("^TestA$"), fixture.options(t))
			if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), test.named) || !strings.Contains(errorPhases(events), "Loom's, never the change's") {
				t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
			}
			if _, err := os.Stat(filepath.Join(fixture.directory, "environment-tree")); err == nil {
				t.Error("prepare.sh ran for a tree the runner had refused")
			}
		})
	}
	t.Run("named with no lockfile", func(t *testing.T) {
		fixture := newPrebuiltFixture(t)
		_, chunk := fixture.withNodePackages(t)
		// The same chunks, but the lockfile's chunk left out of the source.
		source := []builder.SourceChunk{}
		for _, candidate := range fixture.tree.index.Source {
			if !candidate.Holds("stage3/api/package-lock.json") {
				source = append(source, candidate)
			}
		}
		fixture.tree.index.Node = []builder.NodeProject{{Directory: "stage3/api", Lockfile: strings.Repeat("e", 64), Chunk: chunk}}
		fixture.tree.setSource(t, source)
		result, events, _ := runUnit(t, fixture.unit("^TestA$"), fixture.options(t))
		if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), "the source has no lockfile there") {
			t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
		}
	})
}
