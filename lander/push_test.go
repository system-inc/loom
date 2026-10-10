package lander

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The pusher against real git: a bare origin standing in for GitHub, the lander's bare clone, and a stand-in queue.
// Each mutant below must make a test here, or in cmd/loom/push_test.go, fail:
//
//	any "rejected" read as the branch moving (so GH013 parks every change): TestARulesetRefusalHoldsEveryOrderAndParksNothing,
//	TestRefusalsAreReadByWhatGitAndGitHubSaid
//	a ruled refusal going on to the next order: TestARulesetRefusalHoldsEveryOrderAndParksNothing
//	a failure that isn't a refusal stopping the pass: TestAShaGitHubDoesntHaveIsHeldAndTheNextOrderStillLands
//	refs/heads/main pushed whatever the branch: TestAnotherBranchLandsAndMainNeverMoves
//	the sha pushed by its name, not its own ref (a branch named like it goes instead): TestARefNamedLikeTheShaIsNeverPushedInItsPlace
//	the ancestry check dropped, or always passing (the lease then rewinds the branch): TestMainMovedPastTheShaParks,
//	TestTheBranchMovesOnlyByFastForwardToTheExactTestedSha, TestABranchNamedForARefusalStillParksWhenItMoved
//	the lease dropped (a branch deleted mid-pass is created again): TestABranchDeletedMidPassIsNeverCreatedAgain
//	a lease refusal on a branch now gone parking the change: TestABranchDeletedMidPassIsNeverCreatedAgain
//	"stale info" not read as the branch moving: TestABranchMovedMidPassParksAndKeepsItsTip, TestRefusalsAreReadByWhatGitAndGitHubSaid
//	the branch not read back after the push: TestAPushThatDidntStickIsHeld
//	the future not checked to be a commit: TestATagObjectAsTheFutureIsHeld
//	a report the queue refused counted landed: TestALandingWhoseReportWasLostIsReportedAgain
//	rule words matched against all of git's output, or the branch's name left in: TestRefusalsAreReadByWhatGitAndGitHubSaid
//	Queue refusing from equal to main for the future itself, or taking it for any main: wire/test/pipeline/Queue.test.ts ('takes a
//	landing reported again')
//	the push forced (`--force`, no lease): TestABranchDeletedMidPassIsNeverCreatedAgain, TestABranchMovedMidPassParksAndKeepsItsTip
//	the order's future not checked to be a sha: TestAFutureThatIsntAShaIsNeverPushed
//	the landing posted with from and main swapped: TestTheBranchMovesOnlyByFastForwardToTheExactTestedSha
//	a missing branch pushed to (created): TestAnotherBranchLandsAndMainNeverMoves
//	the block pass's panic reaching the landing pass: TestAFailingBlockBuilderNeverStopsALanding
//	the chain built on main whatever the branch: TestABlockIsAMergeChainOnTheBranchAndAConflictIsLeftOutWithItsPaths
//	a chain of conflicts posting null prefixes: TestABlockOfConflictsPostsEmptyPrefixes
//	push.conf taking a branch that isn't a plain name: TestPushConfReadsEachSettingOverWorkshopsDefaults
//	install enabling or starting the timer: TestInstallWritesTheUnitsAndNeverStartsThem
//	install without push.conf: TestInstallRefusesAMachineThatIsntTheLander
//	the hook installing on a machine without push.conf: TestTheHookInstallsOnlyOnTheLander
//	the lock dropped: TestOnePassAtATime (cmd/loom)
//	a held order exiting 0: TestLoomPushHoldsARulesetRefusalAndSaysSo (cmd/loom)

const change = "chg_cccccccccccccccccccccccccc"
const other = "chg_dddddddddddddddddddddddddd"

// TestMain keeps the machine's git settings out (a global core.hooksPath would run its hooks, not the stand-in GitHub's).
func TestMain(m *testing.M) {
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Exit(m.Run())
}

var testIdentity = []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"}

func git(t *testing.T, where string, arguments ...string) string {
	t.Helper()
	stdout, stderr, err := Git(where, testIdentity, arguments...)
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, stderr)
	}
	return strings.TrimSpace(stdout)
}

// A fakeQueue answers GET /landings and GET /blocks?state=unbuilt from its lists and records every POST, its body as the
// JSON the wire would read, and answers each POST with postStatus (200 unless set).
type fakeQueue struct {
	landings   []Order
	blocks     []map[string]any
	posts      []post
	postStatus int
}

type post struct {
	Path string
	Body map[string]any
}

func (queue *fakeQueue) Call(method, path string, body any) (int, []byte, error) {
	switch {
	case method == "GET" && path == "/landings":
		answer, _ := json.Marshal(map[string]any{"landings": queue.landings})
		return 200, answer, nil
	case method == "GET" && path == "/blocks?state=unbuilt":
		answer, _ := json.Marshal(map[string]any{"blocks": append([]map[string]any{}, queue.blocks...)})
		return 200, answer, nil
	case method == "POST":
		encoded, _ := json.Marshal(body)
		decoded := map[string]any{}
		json.Unmarshal(encoded, &decoded)
		queue.posts = append(queue.posts, post{path, decoded})
		if queue.postStatus != 0 && queue.postStatus != 200 {
			return queue.postStatus, []byte(`{"error":"refused"}`), nil
		}
		return 200, []byte(`{"ok":true}`), nil
	}
	return 404, []byte(`{"error":"no route"}`), nil
}

// A world is GitHub (a bare origin), a working clone that makes commits, and the lander's bare clone.
type world struct {
	origin, work, lander, main string
	logged                     []string
}

func newWorld(t *testing.T) *world {
	root := t.TempDir()
	made := &world{origin: filepath.Join(root, "origin.git"), work: filepath.Join(root, "work"), lander: filepath.Join(root, "lander.git")}
	git(t, root, "init", "-q", "--bare", made.origin)
	git(t, root, "init", "-q", made.work)
	git(t, made.work, "remote", "add", "origin", made.origin)
	made.main = made.commit(t, "main", "")
	git(t, made.work, "push", "-q", "origin", "HEAD:refs/heads/main")
	git(t, root, "init", "-q", "--bare", made.lander)
	git(t, made.lander, "remote", "add", "origin", made.origin)
	return made
}

func (made *world) commit(t *testing.T, message, on string) string {
	if on != "" {
		git(t, made.work, "checkout", "-q", "--detach", on)
	}
	os.WriteFile(filepath.Join(made.work, message), []byte(message), 0o644)
	git(t, made.work, "add", "-A")
	git(t, made.work, "commit", "-qm", message)
	return git(t, made.work, "rev-parse", "HEAD")
}

// publish puts sha on GitHub, as the queue's git facts required at submit.
func (made *world) publish(t *testing.T, sha, branch string) {
	git(t, made.work, "push", "-q", "origin", sha+":refs/heads/"+branch)
}

func (made *world) tip(t *testing.T, branch string) string {
	return git(t, made.origin, "rev-parse", "refs/heads/"+branch)
}

func (made *world) hands(branch string) Hands {
	return Hands{Repository: made.lander, Branch: branch}
}

func (made *world) log(text string) {
	made.logged = append(made.logged, text)
}

func orderOf(changeId, future, base string) Order {
	return Order{Change: changeId, Future: future, Base: base, Owner: "o", Run: "r"}
}

func TestTheBranchMovesOnlyByFastForwardToTheExactTestedSha(t *testing.T) {
	made := newWorld(t)
	tested := made.commit(t, "tested", made.main)
	made.publish(t, tested, "a")
	sibling := made.commit(t, "sibling", made.main)
	made.publish(t, sibling, "b")
	queue := &fakeQueue{landings: []Order{orderOf(change, tested, made.main)}}
	if pass := Land(queue, made.hands("main"), made.log); pass != (Pass{Landed: 1}) {
		t.Fatalf("pass %+v, %q", pass, made.logged)
	}
	if made.tip(t, "main") != tested {
		t.Fatalf("main is %s, not the tested sha %s", made.tip(t, "main"), tested)
	}
	want := []post{{"/landings/" + change, map[string]any{"main": tested, "from": made.main, "landed": tested}}}
	if !reflect.DeepEqual(queue.posts, want) {
		t.Fatalf("posted %+v", queue.posts)
	}
	// A sha built on the old main isn't a fast-forward of main now: refused, reported with the main it found, and main
	// stays. The change parks.
	queue = &fakeQueue{landings: []Order{orderOf(change, sibling, made.main)}}
	if pass := Land(queue, made.hands("main"), made.log); pass != (Pass{Parked: 1}) {
		t.Fatalf("pass %+v, %q", pass, made.logged)
	}
	if made.tip(t, "main") != tested || len(queue.posts) != 1 {
		t.Fatalf("main %s, posts %+v", made.tip(t, "main"), queue.posts)
	}
	body := queue.posts[0].Body
	if queue.posts[0].Path != "/landings/"+change || body["main"] != tested || len(body) != 2 ||
		!strings.HasPrefix(body["refused"].(string), "not a fast-forward of main: ") || !strings.Contains(body["refused"].(string), "doesn't contain") {
		t.Fatalf("the refusal posted %+v", queue.posts[0])
	}
}

// GitHub's ruleset (GH013) or a protected branch refusing the lander is no change's fault: the order holds, nothing is
// posted, so nothing parks, and no order after it is tried. The stand-in GitHub is a pre-receive hook saying what
// GitHub says.
func TestARulesetRefusalHoldsEveryOrderAndParksNothing(t *testing.T) {
	made := newWorld(t)
	tested := made.commit(t, "tested", made.main)
	made.publish(t, tested, "a")
	next := made.commit(t, "next", tested)
	made.publish(t, next, "b")
	tries := filepath.Join(t.TempDir(), "tries")
	hook := "#!/bin/sh\necho try >> " + tries + "\n" +
		"echo 'error: GH013: Repository rule violations found for refs/heads/main.' >&2\n" +
		"echo '- Cannot update this protected ref.' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(made.origin, "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	queue := &fakeQueue{landings: []Order{orderOf(change, tested, made.main), orderOf(other, next, made.main)}}
	if pass := Land(queue, made.hands("main"), made.log); pass != (Pass{Held: 1}) {
		t.Fatalf("pass %+v, %q", pass, made.logged)
	}
	tried, _ := os.ReadFile(tries)
	if made.tip(t, "main") != made.main || len(queue.posts) != 0 || string(tried) != "try\n" {
		t.Fatalf("main %s, posts %+v, %q pushes reached GitHub", made.tip(t, "main"), queue.posts, tried)
	}
	if last := made.logged[len(made.logged)-1]; !strings.Contains(last, "HELD "+change) || !strings.Contains(last, "GH013") {
		t.Fatalf("logged %q", made.logged)
	}
}

// A sha GitHub doesn't have can't be fetched: that order holds, unreported, and the pass goes on to the next.
func TestAShaGitHubDoesntHaveIsHeldAndTheNextOrderStillLands(t *testing.T) {
	made := newWorld(t)
	tested := made.commit(t, "tested", made.main)
	made.publish(t, tested, "a")
	queue := &fakeQueue{landings: []Order{orderOf(other, strings.Repeat("f", 40), made.main), orderOf(change, tested, made.main)}}
	if pass := Land(queue, made.hands("main"), made.log); pass != (Pass{Landed: 1, Held: 1}) {
		t.Fatalf("pass %+v, %q", pass, made.logged)
	}
	if made.tip(t, "main") != tested || len(queue.posts) != 1 || queue.posts[0].Path != "/landings/"+change {
		t.Fatalf("main %s, posts %+v", made.tip(t, "main"), queue.posts)
	}
}

// push.conf's branch is the one that moves: a rehearsal lands on its own branch and main never moves, and a branch
// GitHub doesn't have holds every order rather than being created.
func TestAnotherBranchLandsAndMainNeverMoves(t *testing.T) {
	made := newWorld(t)
	made.publish(t, made.main, "loom-rehearsal")
	tested := made.commit(t, "tested", made.main)
	made.publish(t, tested, "a")
	sibling := made.commit(t, "sibling", made.main)
	made.publish(t, sibling, "b")
	queue := &fakeQueue{landings: []Order{orderOf(change, tested, made.main)}}
	if pass := Land(queue, made.hands("loom-rehearsal"), made.log); pass != (Pass{Landed: 1}) {
		t.Fatalf("pass %+v, %q", pass, made.logged)
	}
	if made.tip(t, "loom-rehearsal") != tested || made.tip(t, "main") != made.main {
		t.Fatalf("loom-rehearsal %s, main %s", made.tip(t, "loom-rehearsal"), made.tip(t, "main"))
	}
	if want := (post{"/landings/" + change, map[string]any{"main": tested, "from": made.main, "landed": tested}}); !reflect.DeepEqual(queue.posts, []post{want}) {
		t.Fatalf("posted %+v", queue.posts)
	}
	queue = &fakeQueue{landings: []Order{orderOf(change, sibling, made.main)}}
	if pass := Land(queue, made.hands("loom-rehearsal"), made.log); pass != (Pass{Parked: 1}) || queue.posts[0].Body["main"] != tested ||
		!strings.HasPrefix(queue.posts[0].Body["refused"].(string), "not a fast-forward of loom-rehearsal: ") {
		t.Fatalf("pass %+v, posts %+v", pass, queue.posts)
	}
	queue = &fakeQueue{landings: []Order{orderOf(change, sibling, made.main), orderOf(other, tested, made.main)}}
	made.logged = nil
	if pass := Land(queue, made.hands("loom-nowhere"), made.log); pass != (Pass{Held: 1}) || len(queue.posts) != 0 ||
		len(made.logged) != 1 || !strings.HasPrefix(made.logged[0], "can't read refs/heads/loom-nowhere") {
		t.Fatalf("a missing branch: pass %+v, posts %+v, %q", pass, queue.posts, made.logged)
	}
	if _, _, err := Git(made.origin, nil, "rev-parse", "--verify", "-q", "refs/heads/loom-nowhere"); err == nil {
		t.Fatal("the lander created a branch")
	}
	if made.tip(t, "main") != made.main {
		t.Fatalf("main moved to %s", made.tip(t, "main"))
	}
}

// An order names a change and a 40-hex sha, or git never sees it: a forced refspec, a ref or an option is held.
func TestAFutureThatIsntAShaIsNeverPushed(t *testing.T) {
	made := newWorld(t)
	tested := made.commit(t, "tested", made.main)
	made.publish(t, tested, "a")
	git(t, made.lander, "fetch", "-q", "origin", tested)
	sibling := made.commit(t, "sibling", made.main)
	made.publish(t, sibling, "b")
	git(t, made.lander, "fetch", "-q", "origin", "refs/heads/b:refs/heads/b")
	made.publish(t, tested, "main")
	for _, future := range []string{"+" + sibling, "+refs/heads/b", "b", "--force", strings.ToUpper(sibling)} {
		queue := &fakeQueue{landings: []Order{orderOf(change, future, made.main)}}
		made.logged = nil
		if pass := Land(queue, made.hands("main"), made.log); pass != (Pass{Held: 1}) || len(queue.posts) != 0 ||
			len(made.logged) != 1 || !strings.Contains(made.logged[0], "isn't a 40-hex sha") {
			t.Errorf("%q: pass %+v, posts %+v, %q", future, pass, queue.posts, made.logged)
		}
	}
	queue := &fakeQueue{landings: []Order{orderOf("chg_x --force", tested, made.main)}}
	if pass := Land(queue, made.hands("main"), made.log); pass != (Pass{Held: 1}) {
		t.Errorf("a change that isn't one: pass %+v", pass)
	}
	if made.tip(t, "main") != tested {
		t.Fatalf("main moved to %s", made.tip(t, "main"))
	}
}

// broken is a block builder whose git fails every way it can.
type broken struct{ panics bool }

func (builder broken) Tip() string {
	if builder.panics {
		panic("git timed out")
	}
	return ""
}

func (broken) Build(int, string, []BlockChange) ([]Prefix, []Conflict, string, bool) {
	return nil, nil, "", false
}

func (broken) Publish(int, string) bool { return false }

func TestAFailingBlockBuilderNeverStopsALanding(t *testing.T) {
	for _, builder := range []broken{{panics: true}, {panics: false}} {
		made := newWorld(t)
		tested := made.commit(t, "tested", made.main)
		made.publish(t, tested, "a")
		queue := &fakeQueue{landings: []Order{orderOf(change, tested, made.main)}, blocks: []map[string]any{{"block": 1, "changes": []any{}}}}
		if pass := Once(queue, made.hands("main"), builder, made.log); pass != (Pass{Landed: 1}) {
			t.Fatalf("panics %v: pass %+v, %q", builder.panics, pass, made.logged)
		}
		want := []post{{"/landings/" + change, map[string]any{"main": tested, "from": made.main, "landed": tested}}}
		if made.tip(t, "main") != tested || !reflect.DeepEqual(queue.posts, want) {
			t.Fatalf("panics %v: main %s, posts %+v", builder.panics, made.tip(t, "main"), queue.posts)
		}
	}
}

// What git and GitHub say, as the lander reads it. Only git's own line saying the branch isn't where the push expected
// it, with nothing from the far side naming a rule or a permission, is the branch moving; and a rule word in the
// branch's own name, or in what git itself wrote, never reads as one.
func TestRefusalsAreReadByWhatGitAndGitHubSaid(t *testing.T) {
	to := "To git@github-lander:system-inc/adamic.git\n"
	failed := "error: failed to push some refs to 'github-lander:system-inc/adamic.git'\n"
	denied := "denied-rehearsal"
	for name, test := range map[string]struct {
		stdout, stderr string
		kind           Kind
	}{
		"stale info": {to + "!\tabc:refs/heads/main\t[rejected] (stale info)\nDone\n", failed, Moved},
		"a branch named for a refusal": {"To /tmp/permission-denied/origin.git\n!\trefs/loom/land/abc:refs/heads/" + denied + "\t[rejected] (stale info)\nDone\n",
			"error: failed to push some refs to '/tmp/permission-denied/origin.git'\nhint: Updates were rejected because the tip of the remote-tracking branch has been updated since the last checkout. You may want to integrate those changes locally (e.g., 'git pull ...') before forcing an update. denied\n", Moved},
		"a branch named for a refusal, refused by a rule": {"!\tabc:refs/heads/" + denied + "\t[remote rejected] (push declined due to repository rule violations)\n",
			"remote: error: GH013: Repository rule violations found for refs/heads/" + denied + ".\n", Ruled},
		"a branch named for a refusal, refused for no rule": {"!\tabc:refs/heads/" + denied + "\t[remote rejected] (cannot lock ref 'refs/heads/" + denied + "')\n",
			"remote: error: cannot lock ref 'refs/heads/" + denied + "'\n", Failed},
		"non-fast-forward": {to + "!\tabc:refs/heads/main\t[rejected] (non-fast-forward)\nDone\n",
			failed + "hint: Updates were rejected because a pushed branch tip is behind its remote\n", Moved},
		"fetch first": {to + "!\tabc:refs/heads/main\t[rejected] (fetch first)\nDone\n",
			failed + "hint: Updates were rejected because the remote contains work that you do not\n", Moved},
		"GH013": {to + "!\tabc:refs/heads/main\t[remote rejected] (push declined due to repository rule violations)\nDone\n",
			"remote: error: GH013: Repository rule violations found for refs/heads/main.\nremote: - Cannot update this protected ref.\n" + failed, Ruled},
		"GH006": {to + "!\tabc:refs/heads/main\t[remote rejected] (protected branch hook declined)\nDone\n",
			"remote: error: GH006: Protected branch update failed for refs/heads/main.\n" + failed, Ruled},
		"a ruleset on a non-fast-forward": {to + "!\tabc:refs/heads/main\t[rejected] (non-fast-forward)\nDone\n",
			"remote: error: GH013: Repository rule violations found for refs/heads/main.\n" + failed, Ruled},
		"a read-only key":  {"", "ERROR: The key you are authenticating with has been marked as read only.\nfatal: Could not read from remote repository.\n", Ruled},
		"a deleted key":    {"", "git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.\n", Ruled},
		"another key":      {"", "ERROR: Permission to system-inc/adamic.git denied to deploy key\nfatal: Could not read from remote repository.\n", Ruled},
		"a lost race":      {to + "!\tabc:refs/heads/main\t[remote rejected] (cannot lock ref 'refs/heads/main': is at 1 but expected 2)\nDone\n", failed, Failed},
		"the network":      {"", "ssh: connect to host github.com port 22: Operation timed out\nfatal: Could not read from remote repository.\n", Failed},
		"nothing at all":   {"", "", Failed},
		"rejected, named?": {to + "!\tabc:refs/heads/main\t[remote rejected] (internal server error)\nDone\n", failed, Failed},
	} {
		ref := "refs/heads/main"
		if strings.Contains(test.stdout, denied) {
			ref = "refs/heads/" + denied
		}
		if kind := Classify(test.stdout, test.stderr, ref); kind != test.kind {
			t.Errorf("%s: read as %s, not %s", name, kind, test.kind)
		}
	}
}

func TestABlockIsAMergeChainOnTheBranchAndAConflictIsLeftOutWithItsPaths(t *testing.T) {
	made := newWorld(t)
	files := func(names map[string]string, message, on string) string {
		git(t, made.work, "checkout", "-q", "--detach", on)
		for name, text := range names {
			os.WriteFile(filepath.Join(made.work, name), []byte(text), 0o644)
		}
		git(t, made.work, "add", "-A")
		git(t, made.work, "commit", "-qm", message)
		sha := git(t, made.work, "rev-parse", "HEAD")
		made.publish(t, sha, message)
		return sha
	}
	// The branch blocks build on is ahead of main: a chain built on main would not be a fast-forward of it.
	branchTip := files(map[string]string{"x.txt": "x\n", "y.txt": "y\n"}, "loom-rehearsal", made.main)
	a := files(map[string]string{"x.txt": "x from a\n"}, "a", branchTip)
	b := files(map[string]string{"y.txt": "y from b\n"}, "b", branchTip)
	c := files(map[string]string{"x.txt": "x from c\n"}, "c", branchTip)
	changes := []any{map[string]any{"change": "chg_a", "sha": a}, map[string]any{"change": "chg_b", "sha": b}, map[string]any{"change": "chg_c", "sha": c}}
	queue := &fakeQueue{blocks: []map[string]any{{"block": 1, "changes": changes}}}
	BuildBlocks(queue, Chain{Repository: made.lander, Branch: "loom-rehearsal"}, made.log)
	if len(queue.posts) != 1 || queue.posts[0].Path != "/blocks/1/built" {
		t.Fatalf("posted %+v, %q", queue.posts, made.logged)
	}
	body := queue.posts[0].Body
	if body["base"] != branchTip {
		t.Fatalf("built on %v, not the branch's tip %s", body["base"], branchTip)
	}
	prefixes := body["prefixes"].([]any)
	if len(prefixes) != 2 || prefixes[0].(map[string]any)["change"] != "chg_a" || prefixes[1].(map[string]any)["change"] != "chg_b" {
		t.Fatalf("prefixes %+v", prefixes)
	}
	if conflicts := body["conflicts"]; !reflect.DeepEqual(conflicts, []any{map[string]any{"change": "chg_c", "paths": []any{"x.txt"}}}) {
		t.Fatalf("conflicts %+v", conflicts)
	}
	first, second := prefixes[0].(map[string]any)["tree"].(string), prefixes[1].(map[string]any)["tree"].(string)
	// Each prefix is a real commit: the one before it (the branch's tip for the first) and the change, with both
	// changes' content.
	if parents := strings.Fields(git(t, made.origin, "rev-list", "--parents", "-n", "1", first))[1:]; !reflect.DeepEqual(parents, []string{branchTip, a}) {
		t.Fatalf("the first prefix's parents %q", parents)
	}
	if parents := strings.Fields(git(t, made.origin, "rev-list", "--parents", "-n", "1", second))[1:]; !reflect.DeepEqual(parents, []string{first, b}) {
		t.Fatalf("the second prefix's parents %q", parents)
	}
	if git(t, made.origin, "show", second+":x.txt") != "x from a" || git(t, made.origin, "show", second+":y.txt") != "y from b" {
		t.Fatal("the second prefix doesn't hold both changes")
	}
	// The chain's tip is on GitHub under refs/loom/blocks/1, and landing a prefix is a fast-forward of the branch.
	if git(t, made.origin, "rev-parse", "refs/loom/blocks/1") != second {
		t.Fatal("refs/loom/blocks/1 isn't the chain's tip")
	}
	if _, _, err := Git(made.origin, nil, "merge-base", "--is-ancestor", branchTip, first); err != nil {
		t.Fatal("the first prefix isn't a fast-forward of the branch")
	}
}

// A block whose every change conflicts posts empty lists, never null, which the queue would refuse, and publishes nothing.
func TestABlockOfConflictsPostsEmptyPrefixes(t *testing.T) {
	made := newWorld(t)
	git(t, made.work, "checkout", "-q", "--detach", made.main)
	os.WriteFile(filepath.Join(made.work, "main"), []byte("changed by a\n"), 0o644)
	git(t, made.work, "commit", "-qam", "a")
	a := git(t, made.work, "rev-parse", "HEAD")
	made.publish(t, a, "a")
	git(t, made.work, "checkout", "-q", "--detach", made.main)
	os.WriteFile(filepath.Join(made.work, "main"), []byte("changed on main\n"), 0o644)
	git(t, made.work, "commit", "-qam", "main moves")
	made.publish(t, git(t, made.work, "rev-parse", "HEAD"), "main")
	queue := &fakeQueue{blocks: []map[string]any{{"block": 2, "changes": []any{map[string]any{"change": "chg_a", "sha": a}}}}}
	BuildBlocks(queue, Chain{Repository: made.lander, Branch: "main"}, made.log)
	if len(queue.posts) != 1 || !reflect.DeepEqual(queue.posts[0].Body["prefixes"], []any{}) || len(queue.posts[0].Body["conflicts"].([]any)) != 1 {
		t.Fatalf("posted %+v, %q", queue.posts, made.logged)
	}
	if _, _, err := Git(made.origin, nil, "rev-parse", "--verify", "-q", "refs/loom/blocks/2"); err == nil {
		t.Fatal("an empty chain was published")
	}
}
