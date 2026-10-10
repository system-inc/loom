package runner

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// prepare.sh's submodules (#10xhgkp): a submodule clone killed midway leaves a .git file naming a gitdir that isn't a
// repository, and every later update failed on it. Home-f3279c broke every unit for half an hour on Oct 10 that way: each
// one started the remake of all submodules, a full TypeScript clone, was killed at its start deadline, and left the same
// stub for the next. Each mutant below must make a test here fail:
//
//	clearSubmoduleStubs never called (the old remake only): TestSubmodulesClearAKilledClonesStub,
//	TestSubmodulesRecoverFromAKilledPrepare (mid, whole, is made again)
//	clearSubmoduleStubs keeping the .git file: TestSubmodulesClearAKilledClonesStub (the update still fails)
//	the stub's gitdir kept: TestSubmodulesClearAKilledClonesStub (git refuses to clone over it)
//	a whole submodule cleared too: TestSubmodulesClearAKilledClonesStub (the kept submodule goes)

// submoduleHouse serves three bare repositories over plain HTTP (git's dumb protocol, a file server): leaf, mid holding
// leaf, and super holding mid, as adamic holds cohere and cohere holds TypeScript. block, while set, holds every request
// for leaf's objects until it is cleared, so a clone can be killed in the middle of fetching it.
type submoduleHouse struct {
	directory, url, leaf string
	mutex                sync.Mutex
	block                chan struct{}
	asked                chan struct{}
}

func newSubmoduleHouse(t *testing.T) *submoduleHouse {
	t.Helper()
	house := &submoduleHouse{directory: t.TempDir(), asked: make(chan struct{}, 64)}
	files := http.FileServer(http.Dir(filepath.Join(house.directory, "served")))
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/leaf.git/objects/") {
			house.mutex.Lock()
			block := house.block
			house.mutex.Unlock()
			if block != nil {
				select {
				case house.asked <- struct{}{}:
				default:
				}
				select {
				case <-block:
				case <-request.Context().Done():
					return
				}
			}
		}
		files.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)
	house.url = server.URL
	house.leaf = house.repository(t, "leaf", "")
	house.repository(t, "mid", "leaf")
	house.repository(t, "super", "mid")
	return house
}

func (house *submoduleHouse) hold() {
	house.mutex.Lock()
	defer house.mutex.Unlock()
	house.block = make(chan struct{})
}

func (house *submoduleHouse) release() {
	house.mutex.Lock()
	defer house.mutex.Unlock()
	if house.block != nil {
		close(house.block)
		house.block = nil
	}
}

// repository makes <name>.git, served, with one file of its own and, when child is named, child's served repository as
// a submodule at its current commit. It returns the commit.
func (house *submoduleHouse) repository(t *testing.T, name, child string) string {
	t.Helper()
	work := filepath.Join(house.directory, "work", name)
	runGit(t, "", "init", "-q", work)
	// Enough bytes that leaf's clone takes several requests, so a hold lands in its middle.
	os.WriteFile(filepath.Join(work, name+".txt"), []byte(strings.Repeat(name+"\n", 20000)), 0o644)
	runGit(t, work, "add", name+".txt")
	if child != "" {
		childCommit := runGit(t, filepath.Join(house.directory, "served", child+".git"), "rev-parse", "HEAD")
		modules := "[submodule \"" + child + "\"]\n\tpath = " + child + "\n\turl = " + house.url + "/" + child + ".git\n"
		os.WriteFile(filepath.Join(work, ".gitmodules"), []byte(modules), 0o644)
		runGit(t, work, "add", ".gitmodules")
		runGit(t, work, "update-index", "--add", "--cacheinfo", "160000,"+childCommit+","+child)
	}
	runGit(t, work, "-c", "user.email=loom@test", "-c", "user.name=loom", "commit", "-q", "-m", name)
	served := filepath.Join(house.directory, "served", name+".git")
	runGit(t, "", "clone", "-q", "--bare", work, served)
	runGit(t, served, "update-server-info")
	return runGit(t, served, "rev-parse", "HEAD")
}

// checkout clones super into a fresh tree, as prepare.sh's own clone would, without its submodules.
func (house *submoduleHouse) checkout(t *testing.T) string {
	t.Helper()
	tree := filepath.Join(t.TempDir(), "tree")
	runGit(t, "", "clone", "-q", house.url+"/super.git", tree)
	return tree
}

func runGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	if directory != "" {
		arguments = append([]string{"-C", directory}, arguments...)
	}
	output, err := exec.Command("git", arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}

// prepareSubmodules is the real prepare.sh's submodules mode over tree, with a root and a HOME of its own.
func prepareSubmodules(t *testing.T, tree string) *exec.Cmd {
	t.Helper()
	directory := t.TempDir()
	script := filepath.Join(directory, "prepare.sh")
	os.WriteFile(script, prepareScript, 0o700)
	command := exec.Command("bash", script, "submodules", tree, filepath.Join(directory, "root"))
	command.Env = append(os.Environ(), "LOOM_PREPARE_ATTEMPTS=1", "HOME="+filepath.Join(directory, "home"))
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return command
}

// leafIsWhole fails the test unless the tree's nested submodule is checked out at leaf's commit with its file.
func leafIsWhole(t *testing.T, house *submoduleHouse, tree string) {
	t.Helper()
	if commit := runGit(t, filepath.Join(tree, "mid", "leaf"), "rev-parse", "HEAD"); commit != house.leaf {
		t.Fatalf("mid/leaf is at %s, not leaf's %s", commit, house.leaf)
	}
	if _, err := os.Stat(filepath.Join(tree, "mid", "leaf", "leaf.txt")); err != nil {
		t.Fatalf("mid/leaf has no leaf.txt: %v", err)
	}
}

// Home's state, planted: the nested submodule's gitdir holds only a partial pack, as a clone killed midway leaves it.
// The next prepare clears that one stub and its .git file, clones leaf again, and keeps mid, which was whole.
func TestSubmodulesClearAKilledClonesStub(t *testing.T) {
	house := newSubmoduleHouse(t)
	tree := house.checkout(t)
	if output, err := prepareSubmodules(t, tree).CombinedOutput(); err != nil {
		t.Fatalf("the first prepare: %v %s", err, output)
	}
	leafIsWhole(t, house, tree)

	stub := filepath.Join(tree, ".git", "modules", "mid", "modules", "leaf")
	if err := os.RemoveAll(stub); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(stub, "objects", "pack"), 0o755)
	os.WriteFile(filepath.Join(stub, "objects", "pack", "tmp_pack_killed"), []byte("half a pack"), 0o444)
	for _, entry := range mustReadDirectory(t, filepath.Join(tree, "mid", "leaf")) {
		if entry.Name() != ".git" {
			os.RemoveAll(filepath.Join(tree, "mid", "leaf", entry.Name()))
		}
	}
	kept := filepath.Join(tree, ".git", "modules", "mid", "loom-kept")
	os.WriteFile(kept, nil, 0o644)

	output, err := prepareSubmodules(t, tree).CombinedOutput()
	if err != nil {
		t.Fatalf("prepare over the stub: %v %s", err, output)
	}
	if !strings.Contains(string(output), "cleared mid/leaf/.git") {
		t.Errorf("prepare didn't say it cleared the stub: %s", output)
	}
	leafIsWhole(t, house, tree)
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("mid's whole gitdir was made again, not kept: %s", output)
	}
}

// A prepare killed while it clones leaf (its objects held mid-fetch, then the process group killed, as a unit's deadline
// kills it) leaves whatever git left; the next prepare readies every submodule anyway, and leaves no gitdir that isn't
// a repository.
func TestSubmodulesRecoverFromAKilledPrepare(t *testing.T) {
	house := newSubmoduleHouse(t)
	tree := house.checkout(t)
	house.hold()
	killed := prepareSubmodules(t, tree)
	if err := killed.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-house.asked:
	case <-time.After(30 * time.Second):
		syscall.Kill(-killed.Process.Pid, syscall.SIGKILL)
		killed.Wait()
		t.Fatal("prepare never asked for leaf's objects")
	}
	syscall.Kill(-killed.Process.Pid, syscall.SIGKILL)
	killed.Wait()
	house.release()
	// mid was whole before leaf's clone began, so the next prepare keeps it rather than making every submodule again.
	kept := filepath.Join(tree, ".git", "modules", "mid", "loom-kept")
	if err := os.WriteFile(kept, nil, 0o644); err != nil {
		t.Fatalf("mid's gitdir isn't there after the kill: %v", err)
	}

	output, err := prepareSubmodules(t, tree).CombinedOutput()
	if err != nil {
		t.Fatalf("prepare after the kill: %v %s", err, output)
	}
	leafIsWhole(t, house, tree)
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("mid's whole gitdir was made again, not kept: %s", output)
	}
	gitdirs, _ := filepath.Glob(filepath.Join(tree, ".git", "modules", "*"))
	nested, _ := filepath.Glob(filepath.Join(tree, ".git", "modules", "*", "modules", "*"))
	for _, gitdir := range append(gitdirs, nested...) {
		if exec.Command("git", "--git-dir="+gitdir, "rev-parse", "-q", "--verify", "HEAD").Run() != nil {
			t.Errorf("%s isn't a repository after the second prepare: %s", gitdir, output)
		}
	}
}

func mustReadDirectory(t *testing.T, directory string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
