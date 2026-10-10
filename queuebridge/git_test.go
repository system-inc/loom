package queuebridge

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The facts from real git: a bare origin standing in for GitHub and the bridge's clone of it. Each mutant below must
// make a test here fail:
//
//	historyPaths read from the diff: TestHistoryNamesAPathALaterCommitTookBackOutOfTheDiff
//	a Python test only docs name counted as gate logic: TestAPythonTestAScriptNamesIsGateLogicAndOneOnlyDocsNameIsNot
//	a revert matched by anything but its exact inverse: TestARevertOfAMainCommitIsNamedAndAnythingElseIsNone
//	main's head read from the local ref: TestMainsHeadIsOriginsOwnEvenWhenTheShasFetchFails
//	any failed fetch read as "origin lacks the sha": TestARemoteThatCantBeReadChecksNothingThisTick
//	a git exit code left unchecked: TestGitsExitCodeIsCheckedEverywhere

// TestMain keeps the machine's git settings out (a global core.hooksPath or init.defaultBranch would change the fixtures).
func TestMain(m *testing.M) {
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Exit(m.Run())
}

var identity = []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"}

type world struct {
	root, origin, work, base string
	clone                    Clone
}

func gitIn(t *testing.T, where string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", where}, arguments...)...)
	command.Env = append(os.Environ(), identity...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

// newWorld is origin with one commit on main, which runs a Python test by name, and a working clone of it.
func newWorld(t *testing.T) *world {
	root := t.TempDir()
	made := &world{root: root, origin: filepath.Join(root, "origin.git"), work: filepath.Join(root, "work")}
	gitIn(t, root, "init", "-q", "-b", "main", "--bare", made.origin)
	gitIn(t, root, "init", "-q", "-b", "main", made.work)
	gitIn(t, made.work, "remote", "add", "origin", made.origin)
	made.base = made.commit(t, map[string]string{"cloud/run.sh": "python3 cloud/a_test.py\n", "a.go": "package a\n"})
	gitIn(t, made.work, "push", "-q", "origin", "HEAD:refs/heads/main")
	gitIn(t, made.work, "fetch", "-q", "origin")
	made.clone = Clone{Repository: made.work}
	return made
}

func (made *world) commit(t *testing.T, files map[string]string, remove ...string) string {
	t.Helper()
	for name, text := range files {
		path := filepath.Join(made.work, name)
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte(text), 0o644)
	}
	for _, name := range remove {
		os.Remove(filepath.Join(made.work, name))
	}
	gitIn(t, made.work, "add", "-A")
	gitIn(t, made.work, "commit", "-q", "-m", "c")
	return gitIn(t, made.work, "rev-parse", "HEAD")
}

func TestHistoryNamesAPathALaterCommitTookBackOutOfTheDiff(t *testing.T) {
	made := newWorld(t)
	made.commit(t, map[string]string{"x_test.go": "package a\n", "cloud/gate.sh": "echo\n"})
	sha := made.commit(t, nil, "cloud/gate.sh")
	if history, err := made.clone.historyOf(made.base, sha); err != nil || !reflect.DeepEqual(history, []string{"cloud/gate.sh", "x_test.go"}) {
		t.Fatalf("history %q, %v", history, err)
	}
	if diff := gitIn(t, made.work, "diff", "--name-only", made.base, sha); diff != "x_test.go" {
		t.Fatalf("the diff is %q", diff)
	}
}

func TestAPythonTestAScriptNamesIsGateLogicAndOneOnlyDocsNameIsNot(t *testing.T) {
	made := newWorld(t)
	sha := made.commit(t, map[string]string{"cloud/a_test.py": "x\n", "cloud/b_test.py": "y\n", "docs/b.md": "cloud/b_test.py\n"})
	named, err := made.clone.gateNamedOf(sha, []string{"cloud/a_test.py", "cloud/b_test.py", "docs/b.md"})
	if err != nil || !reflect.DeepEqual(named, []map[string]any{{"path": "cloud/a_test.py", "users": []string{"cloud/run.sh"}}}) {
		t.Fatalf("named %v, %v", named, err)
	}
}

func TestARevertOfAMainCommitIsNamedAndAnythingElseIsNone(t *testing.T) {
	made := newWorld(t)
	landed := made.commit(t, map[string]string{"a.go": "package a\n\nvar x = 1\n"})
	gitIn(t, made.work, "push", "-q", "origin", "HEAD:refs/heads/main")
	gitIn(t, made.work, "fetch", "-q", "origin")
	reverted := made.commit(t, map[string]string{"a.go": "package a\n"})
	if reverts, err := made.clone.revertOf(landed, reverted); err != nil || reverts != landed {
		t.Fatalf("reverts %v, %v", reverts, err)
	}
	unrelated := made.commit(t, map[string]string{"b.go": "package a\n"})
	if reverts, err := made.clone.revertOf(reverted, unrelated); err != nil || reverts != nil {
		t.Fatalf("an unrelated change reverts %v, %v", reverts, err)
	}
}

// pushFromAnotherTree is a landing pushed to origin from another clone, as the pusher lands: the bridge's origin/main
// doesn't follow.
func (made *world) pushFromAnotherTree(t *testing.T) string {
	elsewhere := filepath.Join(made.root, "elsewhere")
	gitIn(t, made.root, "clone", "-q", made.origin, elsewhere)
	gitIn(t, elsewhere, "commit", "-q", "--allow-empty", "-m", "landed")
	gitIn(t, elsewhere, "push", "-q", "origin", "HEAD:refs/heads/main")
	return gitIn(t, elsewhere, "rev-parse", "HEAD")
}

func TestMainsHeadIsOriginsOwnEvenWhenTheShasFetchFails(t *testing.T) {
	made := newWorld(t)
	landed := made.pushFromAnotherTree(t)
	if stale := gitIn(t, made.work, "rev-parse", "origin/main"); stale == landed {
		t.Fatal("the clone followed a push it never fetched")
	}
	// A sha origin doesn't hold fails its fetch; the head is still origin's, not the old local ref.
	facts, err := made.clone.Facts(strings.Repeat("9", 40), made.base)
	if err != nil || facts["shaExists"] != false || facts["mainHead"] != landed {
		t.Fatalf("facts %v, %v", facts, err)
	}
	// main was fetched on its own and checked, so the clone's origin/main follows origin despite the sha's failed fetch.
	if followed := gitIn(t, made.work, "rev-parse", "origin/main"); followed != landed {
		t.Fatalf("origin/main is %s", followed)
	}
	facts, err = made.clone.Facts(made.base, made.base)
	if err != nil || facts["shaExists"] != true || facts["baseOnMain"] != true || facts["mainHead"] != landed {
		t.Fatalf("facts %v, %v", facts, err)
	}
	// Every fact the queue reads is there, empty lists as lists, a change that reverts nothing as null.
	for _, key := range []string{"baseIsAncestor", "diffPaths", "historyPaths", "gateNamed", "revertOf"} {
		if _, ok := facts[key]; !ok {
			t.Errorf("no %s in %v", key, facts)
		}
	}
	if facts["revertOf"] != nil || !reflect.DeepEqual(facts["diffPaths"], []string{}) || !reflect.DeepEqual(facts["gateNamed"], []map[string]any{}) {
		t.Errorf("facts %v", facts)
	}
}

func TestARemoteThatCantBeReadChecksNothingThisTick(t *testing.T) {
	made := newWorld(t)
	gitIn(t, made.work, "remote", "set-url", "origin", filepath.Join(made.root, "gone.git"))
	var gitError *GitError
	if _, err := made.clone.Facts(made.base, made.base); !errors.As(err, &gitError) {
		t.Fatalf("facts from a gone origin: %v", err)
	}
	// A fetch that fails for any reason but origin lacking the sha says nothing about the sha.
	if _, err := made.clone.fetchSha(strings.Repeat("9", 40)); !errors.As(err, &gitError) {
		t.Fatalf("fetching from a gone origin: %v", err)
	}
	queue := newQueue()
	queue.unchecked = []map[string]any{{"change": change, "sha": made.base, "base": made.base, "paths": []string{}}}
	Bridge{Queue: queue, Gate: made.clone, Decides: true, Log: func(string) {}}.Tick(newMemory())
	if posts := queue.posts(); len(posts) != 0 {
		t.Fatalf("posted %+v", posts)
	}
}

func TestGitsExitCodeIsCheckedEverywhere(t *testing.T) {
	made := newWorld(t)
	var gitError *GitError
	if _, err := made.clone.git([]string{"rev-parse", "--verify", "refs/heads/nowhere"}); !errors.As(err, &gitError) {
		t.Fatalf("a failing rev-parse: %v", err)
	}
	if said, err := made.clone.git([]string{"rev-parse", "--verify", "-q", "refs/heads/nowhere"}, 0, 1); said != "" || err != nil {
		t.Fatalf("an allowed exit: %q, %v", said, err)
	}
	if ancestor, err := made.clone.isAncestor(made.base, made.base); !ancestor || err != nil {
		t.Fatalf("a commit is its own ancestor: %v, %v", ancestor, err)
	}
	if _, err := made.clone.isAncestor(strings.Repeat("9", 40), made.base); !errors.As(err, &gitError) {
		t.Fatalf("an unknown sha's ancestry: %v", err)
	}
	if parents, err := made.clone.Parents(strings.Repeat("9", 40)); len(parents) != 0 || err != nil {
		t.Fatalf("a sha origin lacks: %q, %v", parents, err)
	}
	if record, err := made.clone.Record(made.base); record != nil || err != nil {
		t.Fatalf("no record yet: %+v, %v", record, err)
	}
}

// A record is the newest finished one of the tree: one still running (no status yet) is passed over, and the sha it
// gated comes from fast.json.
func TestTheNewestFinishedRecordIsTheTreesVerdictInput(t *testing.T) {
	made := newWorld(t)
	record := func(stamp string, files map[string]string) {
		elsewhere := filepath.Join(made.root, "record-"+stamp)
		gitIn(t, made.root, "init", "-q", "-b", "record", elsewhere)
		for name, text := range files {
			os.WriteFile(filepath.Join(elsewhere, name), []byte(text), 0o644)
		}
		gitIn(t, elsewhere, "add", "-A")
		gitIn(t, elsewhere, "commit", "-q", "-m", "record")
		gitIn(t, elsewhere, "push", "-q", made.origin, "HEAD:refs/heads/gate-logs/"+made.base[:12]+"/"+stamp+"/fast")
	}
	record("20261010T0100", map[string]string{"status.txt": "red: one failed\n", "fast.json": `{"sha":"` + made.base + `"}`})
	record("20261010T0200", map[string]string{"status.txt": "green\n", "fast.json": `{"gated":"` + strings.Repeat("5", 40) + `","sha":"` + made.base + `"}`})
	record("20261010T0300", map[string]string{"log.txt": "running\n"})
	got, err := made.clone.Record(made.base)
	if err != nil || got == nil || *got != (Record{Ref: "gate-logs/" + made.base[:12] + "/20261010T0200/fast", Status: "green", Gated: strings.Repeat("5", 40)}) {
		t.Fatalf("record %+v, %v", got, err)
	}
	// A record that names no test events can't be excused.
	if names, ok := made.clone.Failing(got.Ref); ok {
		t.Fatalf("failing %q from a record with no test.jsonl.gz", names)
	}
}
