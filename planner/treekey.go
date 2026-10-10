package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// A tree key names Workshop's build of one tree (#pcn6prz, builder/tree.go): sha256 of "loom-tree-v2", the commit's
// git tree hash, the Go release that built it, the platform it was built for (GOOS/GOARCH: a Linux binary is no use to
// a Mac runner) and the gate environment. Its index is trees/<key>.json in the action store.
//
// One function keys it, ReadTreeIdentity's Key: `loom build-tree` keys the tree it builds with it, and the planner,
// which has every future's tree checked out on Workshop, keys the tree it planned with it and carries the key on each
// test unit of the plan (PlannedResult.Tree), so the placer can name the build a unit runs (#w7agfa9). Both run on
// Workshop, so the Go release and the platform are Workshop's, linux/amd64 today, the runners' target.

// TreeKey is the key of one tree's build for the platform goos/goarch, under the gate environment.
func TreeKey(treeHash, goVersion, goos, goarch string) string {
	sum := sha256.Sum256([]byte("loom-tree-v2\n" + treeHash + "\n" + goVersion + "\n" + goos + "/" + goarch + "\n" + strings.Join(GateEnvironmentList(), "\n")))
	return hex.EncodeToString(sum[:])
}

// GateEnvironmentList is GateEnvironment as NAME=value lines, sorted: the environment a tree is built under on
// Workshop, so what buildcache builds there under each key is what a gate unit would ask for, and the last part of the
// tree key.
func GateEnvironmentList() []string {
	environment := []string{}
	for name, value := range GateEnvironment {
		environment = append(environment, name+"="+value)
	}
	sort.Strings(environment)
	return environment
}

// A TreeIdentity is what a tree key is made of: the commit's git tree hash, the Go release go resolves in the tree,
// and the platform it builds for.
type TreeIdentity struct {
	Tree   string `json:"tree"`
	Go     string `json:"go"`
	Goos   string `json:"goos"`
	Goarch string `json:"goarch"`
}

// Key is the identity's tree key.
func (identity TreeIdentity) Key() string {
	return TreeKey(identity.Tree, identity.Go, identity.Goos, identity.Goarch)
}

// The runners' target, the platform every tree is built for: linux/amd64 today.
const (
	RunnersGoos   = "linux"
	RunnersGoarch = "amd64"
)

// TreeBuildEnvironment pins what a tree's key reads and its build runs under, whatever the process's own environment
// (review of tree-wiring, finding 4): go is the local one, never a release GOTOOLCHAIN would fetch, and it builds for the
// runners' target. The planner reads the key under it and build-tree reads and builds under it, so the two can't key
// one tree apart by how their services were started.
func TreeBuildEnvironment() []string {
	return []string{"GOTOOLCHAIN=local", "GOOS=" + RunnersGoos, "GOARCH=" + RunnersGoarch}
}

// ReadTreeIdentity reads a checked-out tree's identity: its git tree hash (TreeHash), and go env GOVERSION, GOOS and
// GOARCH asked inside the tree under TreeBuildEnvironment. The Go release is the tree's: when the tree names a
// toolchain (go.work's toolchain line in a workspace, else go.mod's), the local go must be exactly that release or the
// tree is refused, naming both, at plan time rather than at every build; a tree naming none is built by the local go.
// A go that doesn't answer within TreeGoVersionBound refuses the tree.
func ReadTreeIdentity(tree string) (TreeIdentity, error) {
	hash, err := TreeHash(tree)
	if err != nil {
		return TreeIdentity{}, err
	}
	values, err := goEnv(tree, append(os.Environ(), TreeBuildEnvironment()...), TreeGoVersionBound, "GOVERSION", "GOOS", "GOARCH")
	if err != nil {
		return TreeIdentity{}, err
	}
	identity := TreeIdentity{Tree: hash, Go: values[0], Goos: values[1], Goarch: values[2]}
	named, file, err := TreeToolchain(tree)
	if err != nil {
		return TreeIdentity{}, err
	}
	if named != "" && named != identity.Go {
		return TreeIdentity{}, fmt.Errorf("%s names toolchain %s, and this go is %s: the tree is built only by its own release", file, named, identity.Go)
	}
	return identity, nil
}

// TreeToolchain is the Go release a tree names and the file naming it: go.work's toolchain line when the tree is a
// workspace (go reads go.mod's then only for its module), else go.mod's. None named is "".
func TreeToolchain(tree string) (string, string, error) {
	for _, name := range []string{"go.work", "go.mod"} {
		content, err := os.ReadFile(filepath.Join(tree, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", "", err
		}
		for _, line := range strings.Split(string(content), "\n") {
			line, _, _ = strings.Cut(line, "//")
			if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "toolchain" {
				return fields[1], name, nil
			}
		}
		return "", name, nil
	}
	return "", "", nil
}

// TreeHash is a checked-out tree's commit's git tree hash. A tree with changes to tracked files is refused, since its
// hash wouldn't name what's built or planned.
func TreeHash(tree string) (string, error) {
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
	return strings.TrimSpace(string(hash)), nil
}
