package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/system-inc/loom/lander"
	"github.com/system-inc/loom/protocol"
)

// A stand-in Queue on the wire's seam: GET /landings and /blocks?state=unbuilt, POST /landings/<change>, each only with a
// coordinator token the secret signed, as wire/source/Pipeline.ts lets them through.
type standInQueue struct {
	secret   []byte
	mutex    sync.Mutex
	landings []lander.Order
	requests []string
	posts    []string
}

func (queue *standInQueue) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	queue.mutex.Lock()
	defer queue.mutex.Unlock()
	queue.requests = append(queue.requests, request.Method+" "+request.URL.RequestURI())
	claims, err := protocol.VerifyToken(queue.secret, strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "), time.Now())
	if err != nil || claims.Scope != protocol.ScopeCoordinator {
		writer.WriteHeader(http.StatusUnauthorized)
		io.WriteString(writer, `{"error":"a coordinator token"}`)
		return
	}
	switch {
	case request.Method == "GET" && request.URL.RequestURI() == "/landings":
		json.NewEncoder(writer).Encode(map[string]any{"landings": queue.landings})
	case request.Method == "GET" && request.URL.RequestURI() == "/blocks?state=unbuilt":
		io.WriteString(writer, `{"blocks":[]}`)
	case request.Method == "POST" && strings.HasPrefix(request.URL.Path, "/landings/chg_"):
		body, _ := io.ReadAll(request.Body)
		queue.posts = append(queue.posts, request.URL.Path+" "+string(body))
		io.WriteString(writer, `{"state":"landed"}`)
	default:
		writer.WriteHeader(http.StatusNotFound)
		io.WriteString(writer, `{"error":"no route"}`)
	}
}

func pushGit(t *testing.T, where string, arguments ...string) string {
	t.Helper()
	stdout, stderr, err := lander.Git(where, []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"}, arguments...)
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, stderr)
	}
	return strings.TrimSpace(stdout)
}

// A lander for `loom push` to run: a bare origin for GitHub with main and loom-rehearsal, the lander's bare clone, a
// token secret, push.conf naming the branch and the stand-in queue, and a commit maker.
type landerFixture struct {
	origin, work, conf, state, main string
	queue                           *standInQueue
}

func newLanderFixture(t *testing.T, branch string) *landerFixture {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	made := &landerFixture{origin: filepath.Join(root, "origin.git"), work: filepath.Join(root, "work"), conf: filepath.Join(root, "push.conf"), state: filepath.Join(root, "state")}
	pushGit(t, root, "init", "-q", "--bare", made.origin)
	pushGit(t, root, "init", "-q", made.work)
	pushGit(t, made.work, "remote", "add", "origin", made.origin)
	made.main = made.commit(t, "main", "")
	pushGit(t, made.work, "push", "-q", "origin", "HEAD:refs/heads/main", "HEAD:refs/heads/loom-rehearsal")
	clone := filepath.Join(root, "lander.git")
	pushGit(t, root, "init", "-q", "--bare", clone)
	pushGit(t, clone, "remote", "add", "origin", made.origin)
	secret := filepath.Join(root, "token-secret")
	os.WriteFile(secret, []byte(strings.Repeat("s", 64)+"\n"), 0o600)
	made.queue = &standInQueue{secret: []byte(strings.Repeat("s", 64))}
	server := httptest.NewServer(made.queue)
	t.Cleanup(server.Close)
	conf := "branch = " + branch + "\nqueue = " + server.URL + "\nrepository = " + clone + "\nstate = " + made.state + "\nsecret = " + secret + "\n"
	os.WriteFile(made.conf, []byte(conf), 0o644)
	return made
}

func (made *landerFixture) commit(t *testing.T, message, on string) string {
	if on != "" {
		pushGit(t, made.work, "checkout", "-q", "--detach", on)
	}
	os.WriteFile(filepath.Join(made.work, message), []byte(message), 0o644)
	pushGit(t, made.work, "add", "-A")
	pushGit(t, made.work, "commit", "-qm", message)
	sha := pushGit(t, made.work, "rev-parse", "HEAD")
	pushGit(t, made.work, "push", "-q", "origin", sha+":refs/heads/change-"+message)
	return sha
}

func (made *landerFixture) push() (int, string) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"push", "--config", made.conf}, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

// The rehearsal, end to end: `loom push` on a branch other than main, against a stand-in queue over HTTP. A landing
// fast-forwards the branch to exactly the tested sha and posts {main, from, landed}; a stale order then parks with
// {refused, main}; main never moves.
func TestLoomPushLandsOnItsBranchThroughTheQueue(t *testing.T) {
	made := newLanderFixture(t, "loom-rehearsal")
	change := "chg_" + strings.Repeat("c", 26)
	tested := made.commit(t, "tested", made.main)
	sibling := made.commit(t, "sibling", made.main)
	made.queue.landings = []lander.Order{{Change: change, Future: tested, Base: made.main, Owner: "o", Run: "r"}}
	if code, output := made.push(); code != 0 || !strings.Contains(output, "pusher: landed "+change+": loom-rehearsal ") {
		t.Fatalf("exit %d: %s", code, output)
	}
	if tip := pushGit(t, made.origin, "rev-parse", "refs/heads/loom-rehearsal"); tip != tested {
		t.Fatalf("loom-rehearsal is %s, not %s", tip, tested)
	}
	want := "/landings/" + change + ` {"from":"` + made.main + `","landed":"` + tested + `","main":"` + tested + `"}`
	if len(made.queue.posts) != 1 || made.queue.posts[0] != want {
		t.Fatalf("posted %q", made.queue.posts)
	}
	made.queue.landings = []lander.Order{{Change: change, Future: sibling, Base: made.main, Owner: "o", Run: "r"}}
	if code, output := made.push(); code != 0 || !strings.Contains(output, "pusher: refused "+change+" on loom-rehearsal ") {
		t.Fatalf("a stale order: exit %d: %s", code, output)
	}
	var refused map[string]string
	if len(made.queue.posts) != 2 || json.Unmarshal([]byte(strings.TrimPrefix(made.queue.posts[1], "/landings/"+change+" ")), &refused) != nil ||
		refused["main"] != tested || !strings.HasPrefix(refused["refused"], "not a fast-forward of loom-rehearsal") || len(refused) != 2 {
		t.Fatalf("posted %q", made.queue.posts)
	}
	if main := pushGit(t, made.origin, "rev-parse", "refs/heads/main"); main != made.main {
		t.Fatalf("main moved to %s", main)
	}
}

// A ruleset refusal holds the order and exits 1, so the unit reads failed; nothing is posted, so nothing parks.
func TestLoomPushHoldsARulesetRefusalAndSaysSo(t *testing.T) {
	made := newLanderFixture(t, "main")
	change := "chg_" + strings.Repeat("c", 26)
	tested := made.commit(t, "tested", made.main)
	hook := "#!/bin/sh\necho 'error: GH013: Repository rule violations found for refs/heads/main.' >&2\nexit 1\n"
	os.WriteFile(filepath.Join(made.origin, "hooks", "pre-receive"), []byte(hook), 0o755)
	made.queue.landings = []lander.Order{{Change: change, Future: tested, Base: made.main, Owner: "o", Run: "r"}}
	if code, output := made.push(); code != 1 || !strings.Contains(output, "HELD "+change) || !strings.Contains(output, "GH013") {
		t.Fatalf("exit %d: %s", code, output)
	}
	if len(made.queue.posts) != 0 || pushGit(t, made.origin, "rev-parse", "refs/heads/main") != made.main {
		t.Fatalf("posted %q", made.queue.posts)
	}
}

// One pass at a time: while another pass holds the lock, a pass is a no-op that asks the queue nothing.
func TestOnePassAtATime(t *testing.T) {
	made := newLanderFixture(t, "main")
	os.MkdirAll(made.state, 0o755)
	lock, err := os.OpenFile(filepath.Join(made.state, "lock"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if code, output := made.push(); code != 0 || len(made.queue.requests) != 0 {
		t.Fatalf("exit %d, asked %q: %s", code, made.queue.requests, output)
	}
	syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if code, output := made.push(); code != 0 || len(made.queue.requests) != 2 {
		t.Fatalf("once free: exit %d, asked %q: %s", code, made.queue.requests, output)
	}
}

// Without push.conf this machine isn't the lander, and the updater's hook finds `loom push install` in loom's usage.
func TestLoomPushNeedsPushConfAndItsInstallIsInTheUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"push", "--config", filepath.Join(t.TempDir(), "push.conf")}, &stdout, &stderr); code != 3 || !strings.Contains(stderr.String(), "makes it the lander") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	stderr.Reset()
	run(nil, &stdout, &stderr)
	if !strings.Contains(stderr.String(), "\n  loom push install\n") {
		t.Fatalf("the usage:\n%s", stderr.String())
	}
}
