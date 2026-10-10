package lander

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The independent review's ways in (push-go review, Oct 10), each against real git: what an order, the lander's clone
// or GitHub could do to make the lander push the wrong thing, create or rewind a branch, or wedge a change.

// An annotated tag's sha as the future is no commit: held before the push, nothing posted.
func TestATagObjectAsTheFutureIsHeld(t *testing.T) {
	made := newWorld(t)
	tested := made.commit(t, "tested", made.main)
	git(t, made.work, "tag", "-a", "-m", "t", "t1", tested)
	git(t, made.work, "push", "-q", "origin", "refs/tags/t1")
	tag := git(t, made.work, "rev-parse", "refs/tags/t1")
	queue := &fakeQueue{landings: []Order{orderOf(change, tag, made.main)}}
	if pass := Land(queue, made.hands("main"), made.log); pass != (Pass{Held: 1}) || len(queue.posts) != 0 || made.tip(t, "main") != made.main {
		t.Fatalf("pass %+v, posts %+v, main %s", pass, queue.posts, made.tip(t, "main"))
	}
	if !strings.Contains(made.logged[0], "is a tag, not a commit") {
		t.Fatalf("logged %q", made.logged)
	}
}

// A ref in the lander's clone named exactly like the sha, or a stale remote-tracking ref, can't stand in for the sha:
// the order's sha is fetched into a ref of its own and that ref is what is pushed.
func TestARefNamedLikeTheShaIsNeverPushedInItsPlace(t *testing.T) {
	made := newWorld(t)
	tested := made.commit(t, "tested", made.main)
	made.publish(t, tested, "a")
	evil := made.commit(t, "evil", tested)
	made.publish(t, evil, "e")
	git(t, made.lander, "fetch", "-q", "origin", "refs/heads/e:refs/heads/e")
	git(t, made.lander, "update-ref", "refs/heads/"+tested, evil)
	git(t, made.lander, "update-ref", "refs/remotes/origin/main", evil)
	queue := &fakeQueue{landings: []Order{orderOf(change, tested, made.main)}}
	if pass := Land(queue, made.hands("main"), made.log); pass != (Pass{Landed: 1}) || made.tip(t, "main") != tested {
		t.Fatalf("pass %+v, main %s, not the tested %s (evil %s): %q", pass, made.tip(t, "main"), tested, evil, made.logged)
	}
	if want := []post{{"/landings/" + change, map[string]any{"main": tested, "from": made.main, "landed": tested}}}; !reflect.DeepEqual(queue.posts, want) {
		t.Fatalf("posted %+v", queue.posts)
	}
	if refs := git(t, made.lander, "for-each-ref", "refs/loom/"); refs != "" {
		t.Fatalf("the landing's own ref stayed: %s", refs)
	}
}

// raceOn makes the stand-in GitHub do something to the branch at the lander's second upload-pack, the fetch after its
// tip was read and before the push.
func raceOn(t *testing.T, made *world, action string) {
	counter := filepath.Join(t.TempDir(), "n")
	script := filepath.Join(t.TempDir(), "upload-pack")
	os.WriteFile(script, []byte("#!/bin/sh\necho x >> "+counter+"\nif [ $(wc -l < "+counter+") -eq 2 ]; then git -C "+made.origin+" "+action+"; fi\nexec git-upload-pack \"$@\"\n"), 0o755)
	git(t, made.lander, "config", "remote.origin.uploadpack", script)
}

// The branch deleted between its tip's read and the push: the lease refuses it, so it is never created again, and the
// order holds.
func TestABranchDeletedMidPassIsNeverCreatedAgain(t *testing.T) {
	made := newWorld(t)
	tested := made.commit(t, "tested", made.main)
	made.publish(t, tested, "a")
	made.publish(t, made.main, "rehearsal")
	raceOn(t, made, "update-ref -d refs/heads/rehearsal")
	queue := &fakeQueue{landings: []Order{orderOf(change, tested, made.main)}}
	pass := Land(queue, made.hands("rehearsal"), made.log)
	if _, _, err := Git(made.origin, nil, "rev-parse", "--verify", "-q", "refs/heads/rehearsal"); err == nil {
		t.Fatalf("the branch was created again: pass %+v, %q", pass, made.logged)
	}
	if len(made.logged) != 1 || !strings.Contains(made.logged[0], "rehearsal is gone") || !strings.Contains(made.logged[0], "(stale info)") {
		t.Fatalf("held as %q", made.logged)
	}
	if pass != (Pass{Held: 1}) || len(queue.posts) != 0 {
		t.Fatalf("pass %+v, posts %+v, %q", pass, queue.posts, made.logged)
	}
}

// The branch moved between its tip's read and the push: the lease refuses it, the branch keeps what moved it, and the
// change parks on the main the pass read.
func TestABranchMovedMidPassParksAndKeepsItsTip(t *testing.T) {
	made := newWorld(t)
	tested := made.commit(t, "tested", made.main)
	made.publish(t, tested, "a")
	ahead := made.commit(t, "ahead", made.main)
	made.publish(t, ahead, "ahead")
	made.publish(t, made.main, "rehearsal")
	raceOn(t, made, "update-ref refs/heads/rehearsal "+ahead)
	queue := &fakeQueue{landings: []Order{orderOf(change, tested, made.main)}}
	pass := Land(queue, made.hands("rehearsal"), made.log)
	if made.tip(t, "rehearsal") != ahead || pass != (Pass{Parked: 1}) || len(queue.posts) != 1 || queue.posts[0].Body["main"] != made.main ||
		!strings.Contains(queue.posts[0].Body["refused"].(string), "(stale info)") {
		t.Fatalf("rehearsal %s, pass %+v, posts %+v, %q", made.tip(t, "rehearsal"), pass, queue.posts, made.logged)
	}
}

// A lander clone set up as a mirror can't push one ref: git refuses, the order holds, and nothing on GitHub changes.
func TestAMirrorCloneHoldsAndTouchesNothing(t *testing.T) {
	made := newWorld(t)
	tested := made.commit(t, "tested", made.main)
	made.publish(t, tested, "a")
	other := made.commit(t, "o", made.main)
	made.publish(t, other, "side")
	git(t, made.lander, "config", "remote.origin.mirror", "true")
	git(t, made.lander, "config", "remote.origin.fetch", "+refs/*:refs/*")
	git(t, made.lander, "fetch", "-q", "origin")
	git(t, made.lander, "update-ref", "-d", "refs/heads/side")
	before := git(t, made.origin, "for-each-ref")
	queue := &fakeQueue{landings: []Order{orderOf(change, tested, made.main)}}
	if pass := Land(queue, made.hands("main"), made.log); pass != (Pass{Held: 1}) || len(queue.posts) != 0 {
		t.Fatalf("pass %+v, posts %+v, %q", pass, queue.posts, made.logged)
	}
	if after := git(t, made.origin, "for-each-ref"); after != before {
		t.Fatalf("GitHub's refs changed:\n%s\nto\n%s", before, after)
	}
}

// A landing whose report was lost leaves the branch at the order's sha. The next pass reports it again, from that sha
// (Queue takes it as the same landing), and pushes nothing; a report the queue refuses holds the order, never counted
// landed.
func TestALandingWhoseReportWasLostIsReportedAgain(t *testing.T) {
	made := newWorld(t)
	tested := made.commit(t, "tested", made.main)
	made.publish(t, tested, "a")
	queue := &fakeQueue{landings: []Order{orderOf(change, tested, made.main)}, postStatus: 503}
	if pass := Land(queue, made.hands("main"), made.log); pass != (Pass{Held: 1}) || made.tip(t, "main") != tested {
		t.Fatalf("a refused report: pass %+v, main %s", pass, made.tip(t, "main"))
	}
	queue.posts, queue.postStatus = nil, 0
	if pass := Land(queue, made.hands("main"), made.log); pass != (Pass{Landed: 1}) {
		t.Fatalf("again: pass %+v, %q", pass, made.logged)
	}
	if want := []post{{"/landings/" + change, map[string]any{"main": tested, "from": tested, "landed": tested}}}; !reflect.DeepEqual(queue.posts, want) ||
		!strings.Contains(made.logged[len(made.logged)-1], "a landing whose report was lost") {
		t.Fatalf("posted %+v, %q", queue.posts, made.logged)
	}
	queue.posts, queue.postStatus = nil, 400
	if pass := Land(queue, made.hands("main"), made.log); pass != (Pass{Held: 1}) {
		t.Fatalf("the report refused again: pass %+v", pass)
	}
}

// A push GitHub answered but whose branch doesn't read back as the sha (here a hook puts it back) has not landed: held,
// nothing reported.
func TestAPushThatDidntStickIsHeld(t *testing.T) {
	made := newWorld(t)
	tested := made.commit(t, "tested", made.main)
	made.publish(t, tested, "a")
	hook := "#!/bin/sh\nwhile read old new ref; do git update-ref \"$ref\" \"$old\"; done\n"
	os.WriteFile(filepath.Join(made.origin, "hooks", "post-receive"), []byte(hook), 0o755)
	queue := &fakeQueue{landings: []Order{orderOf(change, tested, made.main)}}
	if pass := Land(queue, made.hands("main"), made.log); pass != (Pass{Held: 1}) || len(queue.posts) != 0 || made.tip(t, "main") != made.main {
		t.Fatalf("pass %+v, posts %+v, main %s: %q", pass, queue.posts, made.tip(t, "main"), made.logged)
	}
	if !strings.Contains(made.logged[0], "the push was answered, but refs/heads/main reads as") {
		t.Fatalf("logged %q", made.logged)
	}
}

// A branch whose name holds a rule word ("denied") moving ahead of the sha is the branch moving: the change parks.
func TestABranchNamedForARefusalStillParksWhenItMoved(t *testing.T) {
	made := newWorld(t)
	made.publish(t, made.main, "denied-rehearsal")
	ahead := made.commit(t, "ahead", made.main)
	made.publish(t, ahead, "denied-rehearsal")
	sibling := made.commit(t, "sib", made.main)
	made.publish(t, sibling, "s")
	queue := &fakeQueue{landings: []Order{orderOf(change, sibling, made.main)}}
	if pass := Land(queue, made.hands("denied-rehearsal"), made.log); pass != (Pass{Parked: 1}) || queue.posts[0].Body["main"] != ahead {
		t.Fatalf("pass %+v, posts %+v, %q", pass, queue.posts, made.logged)
	}
}

// A tip the lander's clone never saw (main moved by someone else), or a sha already under main's tip: the change parks,
// and main keeps its tip.
func TestMainMovedPastTheShaParks(t *testing.T) {
	for name, setup := range map[string]func(made *world) string{
		"main moved, unseen": func(made *world) string {
			ahead := made.commit(t, "ahead", made.main)
			made.publish(t, ahead, "main")
			sibling := made.commit(t, "sib", made.main)
			made.publish(t, sibling, "s")
			return sibling
		},
		"the sha under main": func(made *world) string {
			a := made.commit(t, "a", made.main)
			b := made.commit(t, "b", a)
			made.publish(t, b, "main")
			return a
		},
	} {
		made := newWorld(t)
		future := setup(made)
		tip := made.tip(t, "main")
		queue := &fakeQueue{landings: []Order{orderOf(change, future, made.main)}}
		if pass := Land(queue, made.hands("main"), made.log); pass != (Pass{Parked: 1}) || queue.posts[0].Body["main"] != tip || made.tip(t, "main") != tip {
			t.Errorf("%s: pass %+v, posts %+v, main %s", name, pass, queue.posts, made.tip(t, "main"))
		}
	}
}
