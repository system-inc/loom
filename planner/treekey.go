package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
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

// ReadTreeIdentity reads a checked-out tree's identity: its git tree hash (TreeHash), and go env GOVERSION, GOOS and
// GOARCH asked inside the tree under this process's environment, the one its builds run under, so the release is the
// one the tree's go.mod and Workshop's GOTOOLCHAIN resolve there, never the one of whatever directory the process
// stands in. A go that doesn't answer within TreeGoVersionBound refuses the tree.
func ReadTreeIdentity(tree string) (TreeIdentity, error) {
	hash, err := TreeHash(tree)
	if err != nil {
		return TreeIdentity{}, err
	}
	values, err := goEnv(tree, os.Environ(), TreeGoVersionBound, "GOVERSION", "GOOS", "GOARCH")
	if err != nil {
		return TreeIdentity{}, err
	}
	return TreeIdentity{Tree: hash, Go: values[0], Goos: values[1], Goarch: values[2]}, nil
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
