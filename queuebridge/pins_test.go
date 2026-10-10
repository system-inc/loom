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

// The pins fact against a real smart-HTTP git server (git http-backend), standing in for GitHub: cohere, TypeScript and
// a private repository it serves, the last answering 401 without a key. Pins name github.com urls, as adamic's do, and
// the clone's mirror fetches them from the stand-in. Each mutant below must make a test here fail:
//
//	pins not followed into a pin (cohere's TypeScript): TestEveryPinIsFoundRecursivelyAndProvenFetchable
//	a pin to a commit never pushed counted fetchable: TestAPinToACommitThatExistsOnlyLocallyIsUnfetchableAndNamed
//	a url off github.com, or an ssh one, fetched or counted fetchable: TestOnlyGithubComOverHttpsIsEverFetched
//	PinUrl and prepare.sh's pinUrl disagreeing on any url: TestTheBridgeAndPrepareShReadEveryPinUrlAlike
//	the fetch run with the machine's credentials (a helper, ~/.netrc, inherited GIT_CONFIG_*), the environment added to the
//	process's, or credential.helper not emptied: TestAPinBehindAKeyIsUnfetchableEvenWhenThisMachineHoldsOne
//	GitHub down or throttling (403, 429) read as unfetchable: TestARemoteThatIsDownOrThrottlingSaysNothingAndTheFactsWait
//	pins past PinDepth fetched, or passed unproven: TestAPinNestedPastTheDepthIsRefusedByName

// A server is GitHub as the pins see it: bare repositories under root, served over HTTP, or answering every request
// with status when it isn't 0.
type server struct {
	root, url string
	status    int
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
		if made.status != 0 {
			http.Error(writer, "GitHub is having a moment", made.status)
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

// github is a repository's url as .gitmodules names it; mirrored, the clone fetches it from the stand-in.
func github(name string) string {
	return "https://github.com/system-inc/" + name + ".git"
}

// mirrored is clone fetching every github.com url from made instead.
func (made *server) mirrored(clone Clone) Clone {
	clone.mirror = func(address string) string {
		return strings.Replace(address, "https://github.com/system-inc", made.url, 1)
	}
	return clone
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
	cohere := commitIn(t, map[string]string{".gitmodules": gitmodules([2]string{"TypeScript", github("TypeScript")}), "a.go": "package cohere\n"},
		map[string]string{"TypeScript": typeScript}, served.repository(t, "cohere"))
	clone := served.mirrored(newWorld(t).clone)
	// git@github.com: is read as https, as prepare.sh reads it.
	sha := pinned(t, clone, map[string]string{".gitmodules": gitmodules([2]string{"cohere", "git@github.com:system-inc/cohere.git"})}, map[string]string{"cohere": cohere})
	pins, err := clone.PinsOf(sha)
	want := []Pin{{Path: "cohere", Url: github("cohere"), Sha: cohere, Fetchable: true},
		{Path: "cohere/TypeScript", Url: github("TypeScript"), Sha: typeScript, Fetchable: true}}
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
	clone := served.mirrored(newWorld(t).clone)
	sha := pinned(t, clone, map[string]string{".gitmodules": gitmodules([2]string{"cohere", github("cohere")})}, map[string]string{"cohere": unpushed})
	pins, err := clone.PinsOf(sha)
	want := github("cohere") + " doesn't give " + unpushed[:12] + " to a fetch with no key: the commit isn't pushed there"
	if err != nil || len(pins) != 1 || pins[0].Fetchable || pins[0].Reason != want {
		t.Fatalf("pins %+v, %v", pins, err)
	}
}

// A url off github.com (a house address, another host, ssh to anywhere, a relative path) is refused by name, and nothing
// is fetched from it: the stand-in serves cohere, and none of these reach it.
func TestOnlyGithubComOverHttpsIsEverFetched(t *testing.T) {
	served := newServer(t)
	cohere := commitIn(t, map[string]string{"a.go": "package cohere\n"}, nil, served.repository(t, "cohere"))
	clone := newWorld(t).clone
	fetched := 0
	clone.mirror = func(address string) string {
		fetched++
		return served.url + "/cohere.git"
	}
	for _, address := range []string{served.url + "/cohere.git", "http://10.101.1.1/cohere.git", "https://gitlab.com/system-inc/cohere.git",
		"ssh://git@github.com/system-inc/cohere.git", "git@github-lander:system-inc/cohere.git", "../cohere.git", ""} {
		sha := pinned(t, clone, map[string]string{".gitmodules": gitmodules([2]string{"cohere", address})}, map[string]string{"cohere": cohere})
		pins, err := clone.PinsOf(sha)
		if err != nil || len(pins) != 1 || pins[0].Fetchable || !strings.Contains(pins[0].Reason, "isn't a github.com repository over https") {
			t.Errorf("%q: pins %+v, %v", address, pins, err)
		}
	}
	if fetched != 0 {
		t.Fatalf("fetched %d times from urls off github.com", fetched)
	}
}

// The one rule for pin urls, run through the bridge's PinUrl and the runner's prepare.sh on the same table: both take
// the same urls, as the same https url, and refuse the same ones with the same words.
func TestTheBridgeAndPrepareShReadEveryPinUrlAlike(t *testing.T) {
	table := map[string]string{
		"https://github.com/system-inc/cohere.git":          "https://github.com/system-inc/cohere.git",
		"https://github.com/system-inc/TypeScript":          "https://github.com/system-inc/TypeScript",
		"git@github.com:system-inc/cohere.git":              "https://github.com/system-inc/cohere.git",
		"https://github.com/system-inc/a_b.c-d":             "https://github.com/system-inc/a_b.c-d",
		"http://github.com/system-inc/cohere.git":           "",
		"https://github.com/system-inc/cohere.git/":         "",
		"https://github.com/system-inc":                     "",
		"https://github.com/system-inc/../x.git":            "",
		"https://github.com/a/b/c.git":                      "",
		"https://kirk@github.com/system-inc/x.git":          "",
		"https://github.com.evil/system-inc/x.git":          "",
		"https://GitHub.com/system-inc/x.git":               "",
		"ssh://git@github.com/system-inc/x.git":             "",
		"git@github-lander:system-inc/x.git":                "",
		"git@gitlab.com:system-inc/x.git":                   "",
		"http://10.101.1.1/cohere.git":                      "",
		"file:///tmp/cohere.git":                            "",
		"/tmp/cohere.git":                                   "",
		"../cohere.git":                                     "",
		"":                                                  "",
		"https://github.com/system-inc/x.git\nhttps://evil": "",
		"https://github.com/system-inc/x y":                 "",
	}
	prepare, err := filepath.Abs(filepath.Join("..", "runner", "prepare.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for written, want := range table {
		address, refused := PinUrl(written)
		if address != want || (want == "") != (refused != "") {
			t.Errorf("PinUrl(%q) = %q, %q; want %q", written, address, refused, want)
		}
		command := exec.Command("bash", prepare, "pin-url", written)
		command.Env = []string{"PATH=/usr/bin:/bin"}
		output, _ := command.Output()
		said := strings.TrimSuffix(string(output), "\n")
		code := command.ProcessState.ExitCode()
		switch {
		case want != "" && (code != 0 || said != want):
			t.Errorf("prepare.sh pin-url %q: exit %d, %q; want %q", written, code, said, want)
		case want == "" && (code != 3 || said != refused):
			t.Errorf("prepare.sh pin-url %q: exit %d, %q; want exit 3, %q", written, code, said, refused)
		}
	}
}

// This machine holds a key (a credential helper in its git settings and in an inherited GIT_CONFIG_COUNT, a ~/.netrc),
// and a runner wouldn't: the private pin is unfetchable all the same.
func TestAPinBehindAKeyIsUnfetchableEvenWhenThisMachineHoldsOne(t *testing.T) {
	served := newServer(t)
	private := commitIn(t, map[string]string{"a.go": "package cohere\n"}, nil, served.repository(t, "private"))
	home := t.TempDir()
	settings := filepath.Join(home, "gitconfig")
	helper := "!f() { echo username=kirk; echo password=key; }; f"
	os.WriteFile(settings, []byte("[credential]\n\thelper = \""+helper+"\"\n"), 0o644)
	os.WriteFile(filepath.Join(home, ".netrc"), []byte("machine 127.0.0.1 login kirk password key\n"), 0o600)
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", settings)
	// An inherited header carries the key past any credential helper: only an environment made from nothing drops it.
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "credential.helper")
	t.Setenv("GIT_CONFIG_VALUE_0", helper)
	t.Setenv("GIT_CONFIG_KEY_1", "http.extraHeader")
	t.Setenv("GIT_CONFIG_VALUE_1", "Authorization: Basic a2lyazprZXk=")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'credential.helper'='"+helper+"'")
	// The machine's own fetch, with its key, gets it: the key is real.
	holder := t.TempDir()
	gitIn(t, holder, "init", "-q", "--bare")
	gitIn(t, holder, "fetch", "-q", served.url+"/private.git", private)
	clone := served.mirrored(newWorld(t).clone)
	sha := pinned(t, clone, map[string]string{".gitmodules": gitmodules([2]string{"cohere", github("private")})}, map[string]string{"cohere": private})
	pins, err := clone.PinsOf(sha)
	if err != nil || len(pins) != 1 || pins[0].Fetchable || pins[0].Reason != github("private")+" isn't a public repository: a fetch with no key can't read it" {
		t.Fatalf("pins %+v, %v", pins, err)
	}
	// A credential helper in the clone's own configuration, which no environment drops, is never asked either: every git
	// the bridge runs empties credential.helper first.
	gitIn(t, clone.Repository, "config", "credential.helper", helper)
	if _, err := clone.git([]string{"fetch", "-q", served.url + "/private.git", private}); err == nil {
		t.Fatal("the clone's own credential helper fetched the private repository")
	}
}

func TestARemoteThatIsDownOrThrottlingSaysNothingAndTheFactsWait(t *testing.T) {
	served := newServer(t)
	cohere := commitIn(t, map[string]string{"a.go": "package cohere\n"}, nil, served.repository(t, "cohere"))
	made := newWorld(t)
	clone := served.mirrored(made.clone)
	sha := pinned(t, clone, map[string]string{".gitmodules": gitmodules([2]string{"cohere", github("cohere")})}, map[string]string{"cohere": cohere})
	var gitError *GitError
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusForbidden, http.StatusTooManyRequests, http.StatusBadGateway} {
		served.status = status
		if pins, err := clone.PinsOf(sha); !errors.As(err, &gitError) {
			t.Errorf("GitHub answering %d: pins %+v, %v", status, pins, err)
		}
	}
	// Through the pass: no facts are posted, so the change stays unchecked and is read again next tick.
	served.status = http.StatusTooManyRequests
	gitIn(t, made.work, "push", "-q", "origin", sha+":refs/heads/change")
	queue := &fakeQueue{unchecked: []map[string]any{{"change": change, "sha": sha, "base": made.base, "paths": []string{}}}}
	tick(queue, clone)
	if posts := queue.posts(); len(posts) != 0 {
		t.Fatalf("posted %+v", posts)
	}
	served.status = 0
	tick(queue, clone)
	if posts := queue.posts(); len(posts) != 1 || len(posts[0].Body["pins"].([]any)) != 1 {
		t.Fatalf("once GitHub is back: posted %+v", posts)
	}
}

// A chain of pins one deeper than PinDepth: the four within it are proven, and the fifth is refused by name, unfetched.
func TestAPinNestedPastTheDepthIsRefusedByName(t *testing.T) {
	served := newServer(t)
	names := []string{"one", "two", "three", "four", "five"}
	below := commitIn(t, map[string]string{"a.go": "package five\n"}, nil, served.repository(t, names[4]))
	for level := len(names) - 2; level >= 0; level-- {
		below = commitIn(t, map[string]string{".gitmodules": gitmodules([2]string{names[level+1], github(names[level+1])})},
			map[string]string{names[level+1]: below}, served.repository(t, names[level]))
	}
	clone := served.mirrored(newWorld(t).clone)
	sha := pinned(t, clone, map[string]string{".gitmodules": gitmodules([2]string{"one", github("one")})}, map[string]string{"one": below})
	pins, err := clone.PinsOf(sha)
	if err != nil || len(pins) != 5 {
		t.Fatalf("pins %+v, %v", pins, err)
	}
	for index, pin := range pins[:4] {
		if !pin.Fetchable {
			t.Errorf("pin %d, %s: %+v", index+1, pin.Path, pin)
		}
	}
	if deepest := pins[4]; deepest.Path != "one/two/three/four/five" || deepest.Fetchable || deepest.Reason != "it is nested 5 submodules deep, past the 4 the bridge proves" {
		t.Fatalf("the fifth: %+v", deepest)
	}
}
