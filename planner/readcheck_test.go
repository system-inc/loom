package planner

import (
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Every form a traced read takes, and the ones that aren't reads: a failed open, a write-only open, an O_PATH open.
func TestTracedReadsParsesStraceForms(t *testing.T) {
	t.Parallel()
	trace := strings.Join([]string{
		`101 openat(AT_FDCWD</work/p>, "testdata/case.txt", O_RDONLY|O_CLOEXEC) = 3</work/p/testdata/case.txt>`,
		`[pid   102] openat(AT_FDCWD, "relative.txt", O_RDONLY) = 4`,
		`103 openat(7</work/q>, "data.txt", O_RDWR|O_CREAT, 0644) = 5`,
		`104 openat(AT_FDCWD</work/p>, "missing.txt", O_RDONLY) = -1 ENOENT (No such file or directory)`,
		`105 openat(AT_FDCWD</work/p>, "out.txt", O_WRONLY|O_CREAT|O_TRUNC, 0644) = 6`,
		`106 openat(AT_FDCWD</work/p>, "probe", O_RDONLY|O_PATH) = 7`,
		`107 openat(AT_FDCWD</work/p>, "split.txt", O_RDONLY <unfinished ...>`,
		`108 open("/work/r/old.txt", O_RDONLY) = 8`,
		`107 <... openat resumed>) = 9</work/p/split.txt>`,
		`109 execve("/work/tools/run.sh", ["run.sh"], 0x7ffd /* 12 vars */) = 0`,
		`110 openat2(AT_FDCWD</work>, "s/new.txt", {flags=O_RDONLY|O_CLOEXEC, resolve=0}, 24) = 10`,
		`111 openat(AT_FDCWD</work>, "esc\x61ped\n.txt", O_RDONLY) = 11`,
		`112 +++ exited with 0 +++`,
	}, "\n")
	reads, err := TracedReads(strings.NewReader(trace), "/work/default")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/work/default/relative.txt", "/work/escaped\n.txt", "/work/p/split.txt", "/work/p/testdata/case.txt",
		"/work/q/data.txt", "/work/r/old.txt", "/work/s/new.txt", "/work/tools/run.sh"}
	if !reflect.DeepEqual(reads, want) {
		t.Fatalf("traced reads\n%q\nwant\n%q", reads, want)
	}
}

// A tree where p's run reads through every declared route (its closure, a dependency's source, its testdata, a
// reads line, its declared compiler package) and also reads one tracked file nothing declares.
func readCheckFixture(t *testing.T, executors string) (tree, gateTools string) {
	t.Helper()
	tree, gateTools = t.TempDir(), t.TempDir()
	writeFiles(t, tree, map[string]string{
		"go.mod":              "module example.com/readcheck\n\ngo 1.22\n",
		"p/p.go":              "package p\n\nimport _ \"example.com/readcheck/lib\"\n",
		"p/p_test.go":         "package p\n\nimport \"testing\"\n\nfunc TestP(t *testing.T) {}\n",
		"p/testdata/case.txt": "case\n",
		"lib/lib.go":          "package lib\n",
		"compiler/compile.go": "package compiler\n",
		"q/declared.txt":      "declared\n",
		"q/undeclared.txt":    "nobody declared this\n",
	})
	writeFiles(t, gateTools, map[string]string{"cloud/fast-gate/executors.txt": executors})
	for _, arguments := range [][]string{{"init", "-q"}, {"add", "."}} {
		if output, err := exec.Command("git", append([]string{"-C", tree}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", arguments, err, output)
		}
	}
	return tree, gateTools
}

func findingPaths(t *testing.T, tree, gateTools string, compilers []string, traced []string) []string {
	t.Helper()
	unit := Unit{Kind: "test", Package: packageOf(t, tree), Directory: "p"}
	findings, err := CheckReads(tree, gateTools, unit, compilers, traced)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{}
	for _, finding := range findings {
		// A file's is its sha256; a gitlink's, its recorded commit.
		if !(Sha256Hex(finding.Sha256) || len(finding.Sha256) == 40 && Sha256Hex(finding.Sha256+strings.Repeat("0", 24))) || finding.Package != unit.Package {
			t.Errorf("finding %+v lacks its content hash or package", finding)
		}
		paths = append(paths, finding.Path)
	}
	return paths
}

// Only the tracked read nothing declares is a finding. Mutants: a reader whose reads line is gone, and a unit whose
// compiler declaration is gone, each turn a declared read into a finding, so the check consults both.
func TestCheckReadsFindsOnlyReadsOutsideTheDeclaredInputs(t *testing.T) {
	t.Parallel()
	traced := func(tree string) []string {
		paths := []string{"/usr/local/go/src/fmt/print.go", "/tmp/go-build123/b001/p.test", filepath.Join(tree, "p/out.log")}
		for _, file := range []string{"p/p.go", "p/p_test.go", "lib/lib.go", "p/testdata/case.txt", "q/declared.txt",
			"compiler/compile.go", "q/undeclared.txt"} {
			paths = append(paths, filepath.Join(tree, filepath.FromSlash(file)))
		}
		return paths
	}
	compilers := []string{"example.com/readcheck/compiler"}
	for _, check := range []struct {
		name      string
		executors string
		compilers []string
		want      []string
	}{
		{"declared", "reads p q/declared.txt\n", compilers, []string{"q/undeclared.txt"}},
		{"mutant: the reader's reads line gone", "# none\n", compilers, []string{"q/declared.txt", "q/undeclared.txt"}},
		{"mutant: the compiler declaration gone", "reads p q/declared.txt\n", nil, []string{"compiler/compile.go", "q/undeclared.txt"}},
	} {
		t.Run(check.name, func(t *testing.T) {
			t.Parallel()
			tree, gateTools := readCheckFixture(t, check.executors)
			if got := findingPaths(t, tree, gateTools, check.compilers, traced(tree)); !reflect.DeepEqual(got, check.want) {
				t.Fatalf("findings %v, want %v", got, check.want)
			}
		})
	}
}

// A submodule is read as its gitlink: a reads line naming the gitlink hashes its recorded commit (not the directory),
// a read inside it with that line is declared, without it is a finding on the gitlink, and a closure file inside it
// stays declared by its closure either way.
func TestASubmoduleIsReadAsItsGitlink(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")
	root := t.TempDir()
	git := func(directory string, arguments ...string) {
		t.Helper()
		if output, err := exec.Command("git", append([]string{"-C", directory, "-c", "user.name=t", "-c", "user.email=t@t"}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", arguments, err, output)
		}
	}
	sub, tree, gateTools := filepath.Join(root, "sub"), filepath.Join(root, "tree"), filepath.Join(root, "tools")
	writeFiles(t, sub, map[string]string{"go.mod": "module example.com/shim\n\ngo 1.22\n", "shim.go": "package shim\n", "data.txt": "data\n"})
	git(root, "init", "-q", sub)
	git(sub, "add", ".")
	git(sub, "commit", "-q", "-m", "one")
	writeFiles(t, tree, map[string]string{
		"go.mod":      "module example.com/gitlink\n\ngo 1.22\n\nrequire example.com/shim v0.0.0\n\nreplace example.com/shim => ./sub\n",
		"p/p.go":      "package p\n\nimport _ \"example.com/shim\"\n",
		"p/p_test.go": "package p\n\nimport \"testing\"\n\nfunc TestP(t *testing.T) {}\n",
	})
	git(root, "init", "-q", tree)
	git(tree, "submodule", "add", "-q", sub, "sub")
	git(tree, "add", ".")
	for _, check := range []struct {
		executors string
		want      []string
	}{{"reads p sub\n", []string{}}, {"# none\n", []string{"sub"}}} {
		writeFiles(t, gateTools, map[string]string{"cloud/fast-gate/executors.txt": check.executors})
		traced := []string{filepath.Join(tree, "sub/shim.go"), filepath.Join(tree, "sub/data.txt")}
		if got := findingPaths(t, tree, gateTools, nil, traced); !reflect.DeepEqual(got, check.want) {
			t.Errorf("with %q: findings %v, want %v", check.executors, got, check.want)
		}
	}
	writeFiles(t, gateTools, map[string]string{"cloud/fast-gate/executors.txt": "reads p sub\n"})
	reads, err := DeclaredReads(tree, gateTools, "p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadsHash(tree, reads); err != nil || !reflect.DeepEqual(reads, []string{"sub"}) {
		t.Fatalf("reads %v hashed with %v, want the gitlink hashed by its commit", reads, err)
	}
}

// packageOf is the import path of the fixture's package p.
func packageOf(t *testing.T, tree string) string {
	t.Helper()
	module, err := modulePath(tree)
	if err != nil {
		t.Fatal(err)
	}
	return module + "/p"
}

// A read set's trace: a stat, a stat of a descriptor (AT_EMPTY_PATH), a missed path, an O_PATH probe and a failed
// open are lookups, and so is a directory the run made (it was absent); a listing names its decoded directory; a file
// the run created names nothing. A listing whose directory wasn't decoded is refused, since the set can't name it.
func TestTraceAccessesReadsLookupsAndListings(t *testing.T) {
	t.Parallel()
	trace := strings.Join([]string{
		`201 openat(AT_FDCWD</work/p>, "testdata/case.txt", O_RDONLY|O_CLOEXEC) = 3</work/p/testdata/case.txt>`,
		`202 newfstatat(AT_FDCWD</work/p>, "../sub/data.txt", {st_mode=S_IFREG|0644, st_size=5, ...}, 0) = 0`,
		`203 newfstatat(3</work/sub/dir>, "", {st_mode=S_IFDIR|0755, st_size=4096, ...}, AT_EMPTY_PATH) = 0`,
		`204 stat("/work/sub/missing.txt", 0x7ffd) = -1 ENOENT (No such file or directory)`,
		`205 openat(AT_FDCWD</work>, "sub/gone.txt", O_RDONLY|O_CLOEXEC) = -1 ENOENT (No such file or directory)`,
		`206 openat(AT_FDCWD</work>, "sub/probe", O_RDONLY|O_PATH) = 4</work/sub/probe>`,
		`207 getdents64(5</work/sub/dir>, 0x55d0 /* 4 entries */, 32768) = 112`,
		`208 openat(AT_FDCWD</work>, "sub/out.txt", O_WRONLY|O_CREAT|O_TRUNC, 0644) = 6</work/sub/out.txt>`,
		`[pid   209] faccessat2(AT_FDCWD</work>, "sub/bin/tool", X_OK, AT_EACCESS) = 0`,
		`210 readlinkat(AT_FDCWD</work>, "sub/link", "data.txt", 4095) = 8`,
		`211 statx(AT_FDCWD</work>, "sub/x", AT_STATX_SYNC_AS_STAT, STATX_ALL, {stx_mask=STATX_ALL, ...}) = 0`,
		`212 mkdir("/work/sub/made", 0755) = 0`,
	}, "\n")
	accesses, err := TraceAccesses(strings.NewReader(trace), "/work/default")
	if err != nil {
		t.Fatal(err)
	}
	want := TracedAccesses{
		Reads: []string{"/work/p/testdata/case.txt"},
		Lookups: []string{"/work/sub/bin/tool", "/work/sub/data.txt", "/work/sub/dir", "/work/sub/gone.txt", "/work/sub/link", "/work/sub/made",
			"/work/sub/missing.txt", "/work/sub/probe", "/work/sub/x"},
		Listings: []string{"/work/sub/dir"},
		Present:  []string{"/work/sub/bin/tool", "/work/sub/data.txt", "/work/sub/dir", "/work/sub/link", "/work/sub/probe", "/work/sub/x"},
	}
	if !reflect.DeepEqual(accesses, want) {
		t.Fatalf("accesses\n%q\nwant\n%q", accesses, want)
	}
	if _, err := TraceAccesses(strings.NewReader(`7 getdents64(5, 0x55d0 /* 4 entries */, 32768) = 112`), "/work"); err == nil {
		t.Fatal("a listing with no decoded directory was read")
	}
}

// A call that names a relative path without a directory descriptor resolves against its own process's working
// directory (unit-reads review, finding 4): one a child was born with, its parent's when the fork started, though
// strace prints the child's calls before its parent's fork returns; one chdir or fchdir moved; one its latest decoded
// AT_FDCWD shows. A call by a numbered descriptor strace didn't decode is refused. Mutants that each fail it: a child
// born in the traced process's starting directory; chdir not followed; fchdir not followed; a fork placed where it
// returned rather than where it started; execveat not read.
func TestTraceAccessesFollowEachProcesssWorkingDirectory(t *testing.T) {
	t.Parallel()
	trace := strings.Join([]string{
		`100 openat(AT_FDCWD</work/p>, "x.txt", O_RDONLY) = 3`,
		`100 clone(child_stack=NULL, flags=CLONE_CHILD_CLEARTID|SIGCHLD <unfinished ...>`,
		`101 chdir("../sub") = 0`,
		`101 execve("./script.sh", ["./script.sh"], 0x7ffd /* 3 vars */) = 0`,
		`100 <... clone resumed>, child_tidptr=0x7f00) = 101`,
		`101 stat("data.txt", {st_mode=S_IFREG|0644, ...}) = 0`,
		`101 clone3({flags=CLONE_VM|CLONE_VFORK, exit_signal=SIGCHLD, stack=0x7f, stack_size=0x9000}, 88) = 102`,
		`102 open("dir/a.txt", O_RDONLY) = 3`,
		`101 fchdir(4</work/sub/dir>) = 0`,
		`101 access("b.txt", R_OK) = -1 ENOENT (No such file or directory)`,
		`100 execveat(AT_FDCWD</work/p>, "tool", ["tool"], 0x7ffd /* 3 vars */, 0) = 0`,
		`103 stat("orphan.txt", 0x7ffd) = -1 ENOENT (No such file or directory)`,
	}, "\n")
	accesses, err := TraceAccesses(strings.NewReader(trace), "/work/default/x")
	if err != nil {
		t.Fatal(err)
	}
	want := TracedAccesses{
		Reads:    []string{"/work/p/tool", "/work/p/x.txt", "/work/sub/dir/a.txt", "/work/sub/script.sh"},
		Lookups:  []string{"/work/default/x/orphan.txt", "/work/sub", "/work/sub/data.txt", "/work/sub/dir/b.txt"},
		Listings: []string{},
		Present:  []string{"/work/sub", "/work/sub/data.txt"},
	}
	if !reflect.DeepEqual(accesses, want) {
		t.Fatalf("accesses\n%q\nwant\n%q", accesses, want)
	}
	if _, err := TraceAccesses(strings.NewReader(`7 openat(5, "x", O_RDONLY) = 3`), "/work"); err == nil {
		t.Fatal("a call by a numbered descriptor with no decoded directory was read")
	}
}

// What the run made itself is never its input (unit-reads review, finding 5): after it creates or empties a file,
// makes a directory, renames or links onto a path, or renames or removes one, accesses there read the run's own doing.
// Accesses before that call still count: a miss before a create, the source a rename moved, what a removal found. An
// append that opened a file says it was there, and a file read through a link the run made is still read. Mutants
// that each fail it: a created file read back as an input; a directory the run made not covering what it then made
// inside; a rename's target read as an input; what the run made counted from the trace's start rather than from the
// call that made it; a read through a link the run made dropped.
func TestTraceAccessesLeaveOutWhatTheRunMade(t *testing.T) {
	t.Parallel()
	trace := strings.Join([]string{
		`9 newfstatat(AT_FDCWD</w>, "sub/out.txt", 0x7ffd, 0) = -1 ENOENT (No such file or directory)`,
		`9 openat(AT_FDCWD</w>, "sub/out.txt", O_WRONLY|O_CREAT|O_TRUNC|O_CLOEXEC, 0644) = 3</w/sub/out.txt>`,
		`9 openat(AT_FDCWD</w>, "sub/out.txt", O_RDONLY|O_CLOEXEC) = 3</w/sub/out.txt>`,
		`9 mkdir("sub/cache", 0755) = 0`,
		`9 openat(AT_FDCWD</w>, "sub/cache/entry", O_WRONLY|O_CREAT|O_APPEND, 0600) = 4</w/sub/cache/entry>`,
		`9 openat(AT_FDCWD</w>, "sub/lock", O_RDWR|O_CREAT|O_EXCL, 0600) = 4</w/sub/lock>`,
		`9 newfstatat(AT_FDCWD</w>, "sub/lock", {st_mode=S_IFREG|0600, ...}, 0) = 0`,
		`9 openat(AT_FDCWD</w>, "sub/cache/entry", O_RDONLY) = 3</w/sub/cache/entry>`,
		`9 getdents64(5</w/sub/cache>, 0x55 /* 3 entries */, 32768) = 72`,
		`9 renameat2(AT_FDCWD</w>, "sub/input.txt", AT_FDCWD</w>, "sub/moved.txt", RENAME_NOREPLACE) = 0`,
		`9 openat(AT_FDCWD</w>, "sub/moved.txt", O_RDONLY) = 3</w/sub/moved.txt>`,
		`9 newfstatat(AT_FDCWD</w>, "sub/input.txt", 0x7ffd, 0) = -1 ENOENT (No such file or directory)`,
		`9 unlink("/w/sub/old.txt") = 0`,
		`9 openat(AT_FDCWD</w>, "sub/old.txt", O_RDONLY) = -1 ENOENT (No such file or directory)`,
		`9 symlinkat("data.txt", AT_FDCWD</w>, "sub/alias") = 0`,
		`9 openat(AT_FDCWD</w>, "sub/alias", O_RDONLY) = 3</w/sub/data.txt>`,
		`9 openat(AT_FDCWD</w>, "sub/log.txt", O_WRONLY|O_CREAT|O_APPEND, 0644) = 3</w/sub/log.txt>`,
	}, "\n")
	accesses, err := TraceAccesses(strings.NewReader(trace), "/w")
	if err != nil {
		t.Fatal(err)
	}
	want := TracedAccesses{
		Reads:    []string{"/w/sub/data.txt"},
		Lookups:  []string{"/w/sub/cache", "/w/sub/input.txt", "/w/sub/log.txt", "/w/sub/old.txt", "/w/sub/out.txt"},
		Listings: []string{},
		Present:  []string{"/w/sub/input.txt", "/w/sub/old.txt"},
	}
	if !reflect.DeepEqual(accesses, want) {
		t.Fatalf("accesses\n%q\nwant\n%q", accesses, want)
	}
}

// The review's proof (finding 5): a unit whose test writes a fresh-named file inside the submodule and reads it back
// is clean on its second traced run, where every run was beyond its key and its set grew forever.
func TestARunReadingWhatItWroteIsntBeyondItsKey(t *testing.T) {
	useReadSets(t)
	fixture := newReadSetFixture(t)
	parts, _ := fixture.key(t)
	for run := 0; run < 3; run++ {
		name := "sub/tmp-" + strconv.Itoa(run)
		trace := `9 openat(AT_FDCWD<` + fixture.tree + `>, "` + name + `", O_WRONLY|O_CREAT|O_TRUNC, 0644) = 3` + "\n" +
			`9 openat(AT_FDCWD<` + fixture.tree + `>, "` + name + `", O_RDONLY) = 3` + "\n" +
			`9 openat(AT_FDCWD<` + fixture.tree + `>, "sub/data.txt", O_RDONLY) = 3` + "\n"
		check, err := CheckTrace(fixture.tree, fixture.gateTools, parts, strings.NewReader(trace))
		if err != nil {
			t.Fatal(err)
		}
		if run > 0 && len(check.Findings) > 0 {
			t.Fatalf("run %d: findings %+v for a file the run wrote itself", run, check.Findings)
		}
		if want := []string{"sub/data.txt"}; !reflect.DeepEqual(check.Measured.Paths, want) {
			t.Fatalf("run %d measured %v, want %v", run, check.Measured.Paths, want)
		}
		if _, err := RecordReadSet(ReadSetsDirectory, check.CodeKey, parts, check.Measured); err != nil {
			t.Fatal(err)
		}
		parts, _ = fixture.key(t)
	}
}

// subTrace is a trace of p's run reading each of the submodule paths given from the package's directory, a name
// ending in / listed.
func subTrace(fixture readSetFixture, paths ...string) string {
	lines := []string{}
	for _, name := range paths {
		if directory, listed := strings.CutSuffix(name, "/"); listed {
			lines = append(lines, `9 getdents64(5<`+filepath.Join(fixture.tree, directory)+`>, 0x55 /* 3 entries */, 32768) = 72`)
			continue
		}
		lines = append(lines, `9 openat(AT_FDCWD<`+filepath.Join(fixture.tree, "p")+`>, "../`+name+`", O_RDONLY|O_CLOEXEC) = 3`)
	}
	return strings.Join(lines, "\n") + "\n"
}

// The check of a traced run measures what it read in the submodules and, on a key with a read set, names every path
// beyond it as Loom's void. Keyed on the gitlink (no set yet), the declared submodule covers every read and the run
// measures the unit's first set; keyed on that set, a run that reads only it is clean, and one that reads another
// path, misses one or lists another directory is beyond its key, each named, and the measured set grows by them. Parts
// this tree doesn't key are refused. Mutants that each fail it: a read beyond the set that isn't a finding; lookups or
// listings left out of the check; the measured set not grown from the keyed one; the parts not keyed again on the tree.
func TestCheckTraceNamesReadsBeyondTheKeysReadSet(t *testing.T) {
	useReadSets(t)
	fixture := newReadSetFixture(t)
	check := func(parts KeyParts, trace string) TraceCheck {
		t.Helper()
		result, err := CheckTrace(fixture.tree, fixture.gateTools, parts, strings.NewReader(trace))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	coarse, _ := fixture.key(t)
	first := check(coarse, subTrace(fixture, "sub/data.txt", "sub/shim.go", "sub/dir/"))
	if len(first.Findings) != 0 {
		t.Fatalf("keyed on the declared gitlink, the run has findings %+v", first.Findings)
	}
	if want := (ReadSet{Paths: []string{"sub/data.txt"}, Listings: []string{"sub/dir"}}); !reflect.DeepEqual(first.Measured, want) {
		t.Fatalf("measured %+v, want %+v (sub/shim.go is the closure's)", first.Measured, want)
	}
	id, err := RecordReadSet(ReadSetsDirectory, first.CodeKey, coarse, first.Measured)
	if err != nil {
		t.Fatal(err)
	}
	keyed, _ := fixture.key(t)
	if keyed.ReadSet != id {
		t.Fatalf("the next key holds read set %q, want the measured %s", keyed.ReadSet, id)
	}
	if clean := check(keyed, subTrace(fixture, "sub/data.txt", "sub/dir/")); len(clean.Findings) != 0 || clean.CodeKey != first.CodeKey {
		t.Fatalf("a run that read only its set: findings %+v, code key %s (want %s)", clean.Findings, clean.CodeKey, first.CodeKey)
	}
	beyond := check(keyed, subTrace(fixture, "sub/data.txt", "sub/other.txt", "sub/deep/")+
		`9 newfstatat(AT_FDCWD<`+fixture.tree+`>, "sub/nothing.txt", 0x7ffd, 0) = -1 ENOENT (No such file or directory)`+"\n")
	named := []string{}
	for _, finding := range beyond.Beyond() {
		if finding.ReadSet != id || finding.Package != fixture.unit.Package || finding.State == "" {
			t.Errorf("finding %+v doesn't name the set, the unit and the path's state", finding)
		}
		named = append(named, finding.Path)
	}
	if want := []string{"sub/deep", "sub/nothing.txt", "sub/other.txt"}; !reflect.DeepEqual(named, want) || len(beyond.Findings) != 3 {
		t.Fatalf("beyond the key: %v (of %d findings), want %v", named, len(beyond.Findings), want)
	}
	want := ReadSet{Paths: []string{"sub/data.txt", "sub/nothing.txt", "sub/other.txt"}, Listings: []string{"sub/deep", "sub/dir"}}
	if !reflect.DeepEqual(beyond.Measured, want) {
		t.Fatalf("the refreshed set is %+v, want %+v", beyond.Measured, want)
	}
	stale := keyed
	stale.Closure = strings.Repeat("e", 64)
	if _, err := CheckTrace(fixture.tree, fixture.gateTools, stale, strings.NewReader("")); err == nil {
		t.Fatal("parts this tree doesn't key were checked")
	}
}
