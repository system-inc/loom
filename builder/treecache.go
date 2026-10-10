package builder

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// treeHashPattern is a git tree hash, the name of one tree's buildcache directory.
var treeHashPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// TreeCache is the buildcache directory one tree's build uses: base/<the commit's tree hash>. adamic's buildcache keys
// a product by the files its caller declares (and GoBuild by what go list reports), so a product can read a file its
// key doesn't name: a header reached by a relative #include, a hand-rolled product's undeclared input. Within one tree
// no file changes, so sharing a cache among that tree's actions is safe; across trees it could serve a stale product
// (Release's gate-truth question, Oct 10 02:25Z). Two commits with the same content share a tree hash and so a cache.
// A tree with changes to tracked files is refused, since its hash wouldn't name what's built. Only the keep most
// recent tree caches stay; older ones are removed file by file (RemoveTree), and only directories named by a tree
// hash are touched.
func TreeCache(base, tree string, keep int) (string, error) {
	status, err := exec.Command("git", "-C", tree, "status", "--porcelain", "--untracked-files=no").Output()
	if err != nil {
		return "", fmt.Errorf("git status in %s: %w", tree, err)
	}
	if strings.TrimSpace(string(status)) != "" {
		return "", fmt.Errorf("%s has changes to tracked files, so its tree hash wouldn't name what's built:\n%s", tree, status)
	}
	hash, err := exec.Command("git", "-C", tree, "rev-parse", "HEAD^{tree}").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD^{tree} in %s: %w", tree, err)
	}
	directory := filepath.Join(base, strings.TrimSpace(string(hash)))
	// A removal that stopped partway left a .removing- directory, never a tree's: finish it.
	if err = os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	if err = SweepRemoving(base); err != nil {
		return "", err
	}
	if err = os.MkdirAll(directory, 0o755); err != nil {
		return "", err
	}
	// The tree being built is the newest, whatever its directory's age.
	if err = os.Chtimes(directory, time.Now(), time.Now()); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", err
	}
	type cache struct {
		path     string
		modified int64
	}
	caches := []cache{}
	for _, entry := range entries {
		if !entry.IsDir() || !treeHashPattern.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return "", err
		}
		caches = append(caches, cache{filepath.Join(base, entry.Name()), info.ModTime().UnixNano()})
	}
	sort.Slice(caches, func(left, right int) bool { return caches[left].modified > caches[right].modified })
	for index, old := range caches {
		if index >= max(1, keep) && old.path != directory {
			// A tree another build still holds stays until a later build finds it free.
			if err = RemoveTree(base, old.path); err != nil && !errors.Is(err, ErrTreeInUse) {
				return "", err
			}
		}
	}
	// A tree's lock file outlives its directory; one whose tree is gone and that no build holds goes too.
	for _, entry := range entries {
		hash, isLock := strings.CutSuffix(entry.Name(), ".lock")
		if !isLock || !treeHashPattern.MatchString(hash) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(base, hash)); err == nil {
			continue
		}
		lock, err := os.OpenFile(filepath.Join(base, entry.Name()), os.O_RDWR, 0o644)
		if err != nil {
			continue
		}
		if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			os.Remove(filepath.Join(base, entry.Name()))
		}
		lock.Close()
	}
	return directory, nil
}
