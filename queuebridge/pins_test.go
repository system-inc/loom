package queuebridge

import (
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The pins fact against a real smart-HTTP git server (git http-backend), standing in for GitHub: cohere and TypeScript
// as bare repositories it serves keyless, and a private one that answers 401 without a key. Each mutant below must make
// a test here fail:
//
//	pins not followed into a pin (cohere's TypeScript): TestEveryPinIsFoundRecursivelyAndProvenFetchable
//	a pin to a commit never pushed counted fetchable: TestAPinToACommitThatExistsOnlyLocallyIsUnfetchableAndNamed
//	an ssh url fetched with whatever key the machine holds, or counted fetchable: TestAnSshPinIsUnfetchableForAKeylessMachine
//	the fetch run with the machine's credentials (a helper, ~/.netrc): TestAPinBehindAKeyIsUnfetchableEvenWhenThisMachineHoldsOne
//	a remote that's down read as unfetchable (refused on a guess): TestARemoteThatIsDownSaysNothingAndTheFactsWait

// A server is GitHub as the pins see it: bare repositories under root, served over HTTP.
type server struct {
	root, url string
	down      bool
}

func newServer(t *testing.T) *server {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	made := &server{root: t.TempDir()}
	backend := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + made.root, "GIT_HTTP_EXPORT_ALL=1"},
		InheritEnv: []string{"PATH"}}
	listener := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if made.down {
			http.Error(writer, "GitHub is having a moment", http.StatusServiceUnavailable)
			return
		}
		// The private repository answers only a caller with a key.
		if strings.HasPrefix(request.URL.Path, "/private.git/") && request.Header.Get("Authorization") == "" {
			writer.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			http.Error(writer, "authentication required", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(writer, request)
	}))
	t.Cleanup(listener.Close)
	made.url = listener.URL
	return made
}

// repository makes a bare repository the server serves, as GitHub serves one: a commit is fetchable by its sha only
// when a ref reaches it, with blobs left out on request.
func (made *server) repository(t *testing.T, name string) string {
	bare := filepath.Join(made.root, name+".git")
	gitIn(t, made.root, "init", "-q", "-b", "main", "--bare", bare)
	for _, setting := range [][2]string{{"http.uploadpack", "true"}, {"uploadpack.allowFilter", "true"}, {"uploadpack.allowReachableSHA1InWant", "true"}} {
		gitIn(t, bare, "config", setting[0], setting[1])
	}
	return bare
}

// commitIn makes a commit in a new working repository holding files and gitlinks (path to sha), and pushes it to push's
// main unless push is "": the commit then exists only in that working repository.
func commitIn(t *testing.T, files map[string]string, links map[string]string, push string) string {
	work := t.TempDir()
	gitIn(t, work, "init", "-q", "-b", "main")
	for name, text := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755)
		os.WriteFile(filepath.Join(work, name), []byte(text), 0o644)
	}
	gitIn(t, work, "add", "-A")
	for where, sha := range links {
		gitIn(t, work, "update-index", "--add", "--cacheinfo", "160000,"+sha+","+where)
	}
	gitIn(t, work, "commit", "-q", "--allow-empty", "-m", "c")
	if push != "" {
		gitIn(t, work, "push", "-q", push, "HEAD:refs/heads/main")
	}
	return gitIn(t, work, "rev-parse", "HEAD")
}

func gitmodules(entries ...[2]string) string {
	var text strings.Builder
	for _, entry := range entries {
		text.WriteString("[submodule \"" + entry[0] + "\"]\n\tpath = " + entry[0] + "\n\turl = " + entry[1] + "\n")
	}
	return text.String()
}

// pinned brings a commit made elsewhere into the bridge's clone, as fetching the change from origin does.
func pinned(t *testing.T, clone Clone, files map[string]string, links map[string]string) string {
	origin := filepath.Join(t.TempDir(), "change.git")
	gitIn(t, filepath.Dir(origin), "init", "-q", "--bare", origin)
	sha := commitIn(t, files, links, origin)
	gitIn(t, clone.Repository, "fetch", "-q", origin, sha)
	return sha
}

func TestEveryPinIsFoundRecursivelyAndProvenFetchable(t *testing.T) {
	served := newServer(t)
	typeScript := commitIn(t, map[string]string{"src/a.ts": "let a = 1\n"}, nil, served.repository(t, "TypeScript"))
	cohere := commitIn(t, map[string]string{".gitmodules": gitmodules([2]string{"TypeScript", served.url + "/TypeScript.git"}), "a.go": "package cohere\n"},
		map[string]string{"TypeScript": typeScript}, served.repository(t, "cohere"))
	clone := newWorld(t).clone
	sha := pinned(t, clone, map[string]string{".gitmodules": gitmodules([2]string{"cohere", served.url + "/cohere.git"})}, map[string]string{"cohere": cohere})
	pins, err := clone.PinsOf(sha)
	want := []Pin{{Path: "cohere", Url: served.url + "/cohere.git", Sha: cohere, Fetchable: true},
		{Path: "cohere/TypeScript", Url: served.url + "/TypeScript.git", Sha: typeScript, Fetchable: true}}
	if err != nil || !reflect.DeepEqual(pins, want) {
		t.Fatalf("pins %+v, %v", pins, err)
	}
	// A tree with no gitlinks has no pins, as a list.
	if pins, err := clone.PinsOf(gitIn(t, clone.Repository, "rev-parse", "HEAD")); err != nil || pins == nil || len(pins) != 0 {
		t.Fatalf("no gitlinks: %+v, %v", pins, err)
	}
	// The scratch stores are gone.
	if left, _ := filepath.Glob(filepath.Join(os.TempDir(), "loom-pins-*")); len(left) != 0 {
		t.Fatalf("scratch left behind: %q", left)
	}
}

func TestAPinToACommitThatExistsOnlyLocallyIsUnfetchableAndNamed(t *testing.T) {
	served := newServer(t)
	commitIn(t, map[string]string{"a.go": "package cohere\n"}, nil, served.repository(t, "cohere"))
	unpushed := commitIn(t, map[string]string{"b.go": "package cohere\n"}, nil, "")
	clone := newWorld(t).clone
	sha := pinned(t, clone, map[string]string{".gitmodules": gitmodules([2]string{"cohere", served.url + "/cohere.git"})}, map[string]string{"cohere": unpushed})
	pins, err := clone.PinsOf(sha)
	if err != nil || len(pins) != 1 || pins[0].Fetchable || pins[0].Sha != unpushed || !strings.Contains(pins[0].Reason, "not our ref") {
		t.Fatalf("pins %+v, %v", pins, err)
	}
}

func TestAnSshPinIsUnfetchableForAKeylessMachine(t *testing.T) {
	clone := newWorld(t).clone
	for _, address := range []string{"git@github.com:system-inc/cohere.git", "ssh://git@github.com/system-inc/cohere.git", "git@github-lander:system-inc/cohere.git"} {
		sha := pinned(t, clone, map[string]string{".gitmodules": gitmodules([2]string{"cohere", address})}, map[string]string{"cohere": strings.Repeat("7", 40)})
		pins, err := clone.PinsOf(sha)
		if err != nil || len(pins) != 1 || pins[0].Fetchable || !strings.Contains(pins[0].Reason, "ssh") {
			t.Errorf("%s: pins %+v, %v", address, pins, err)
		}
	}
	// A pin .gitmodules doesn't name has no url to fetch from.
	sha := pinned(t, clone, map[string]string{"a.go": "package a\n"}, map[string]string{"cohere": strings.Repeat("7", 40)})
	if pins, err := clone.PinsOf(sha); err != nil || len(pins) != 1 || pins[0].Fetchable || !strings.Contains(pins[0].Reason, "names no url") {
		t.Fatalf("no .gitmodules: pins %+v, %v", pins, err)
	}
}

// This machine holds a key (a credential helper in its git settings, a ~/.netrc), and a runner wouldn't: the private pin
// is unfetchable all the same.
func TestAPinBehindAKeyIsUnfetchableEvenWhenThisMachineHoldsOne(t *testing.T) {
	served := newServer(t)
	private := commitIn(t, map[string]string{"a.go": "package cohere\n"}, nil, served.repository(t, "private"))
	home := t.TempDir()
	settings := filepath.Join(home, "gitconfig")
	os.WriteFile(settings, []byte("[credential]\n\thelper = \"!f() { echo username=kirk; echo password=key; }; f\"\n"), 0o644)
	os.WriteFile(filepath.Join(home, ".netrc"), []byte("machine 127.0.0.1 login kirk password key\n"), 0o600)
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", settings)
	// The machine's own fetch, with its key, gets it: the key is real.
	holder := t.TempDir()
	gitIn(t, holder, "init", "-q", "--bare")
	gitIn(t, holder, "fetch", "-q", served.url+"/private.git", private)
	clone := newWorld(t).clone
	sha := pinned(t, clone, map[string]string{".gitmodules": gitmodules([2]string{"cohere", served.url + "/private.git"})}, map[string]string{"cohere": private})
	pins, err := clone.PinsOf(sha)
	if err != nil || len(pins) != 1 || pins[0].Fetchable {
		t.Fatalf("pins %+v, %v", pins, err)
	}
}

func TestARemoteThatIsDownSaysNothingAndTheFactsWait(t *testing.T) {
	served := newServer(t)
	cohere := commitIn(t, map[string]string{"a.go": "package cohere\n"}, nil, served.repository(t, "cohere"))
	made := newWorld(t)
	sha := pinned(t, made.clone, map[string]string{".gitmodules": gitmodules([2]string{"cohere", served.url + "/cohere.git"})}, map[string]string{"cohere": cohere})
	served.down = true
	var gitError *GitError
	if pins, err := made.clone.PinsOf(sha); !errors.As(err, &gitError) {
		t.Fatalf("with GitHub down: pins %+v, %v", pins, err)
	}
	// Through the pass: no facts are posted, so the change stays unchecked and is read again next tick.
	gitIn(t, made.work, "push", "-q", "origin", sha+":refs/heads/change")
	queue := &fakeQueue{unchecked: []map[string]any{{"change": change, "sha": sha, "base": made.base, "paths": []string{}}}}
	tick(queue, made.clone)
	if posts := queue.posts(); len(posts) != 0 {
		t.Fatalf("posted %+v", posts)
	}
	served.down = false
	tick(queue, made.clone)
	if posts := queue.posts(); len(posts) != 1 || len(posts[0].Body["pins"].([]any)) != 1 {
		t.Fatalf("once GitHub is back: posted %+v", posts)
	}
}
