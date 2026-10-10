package builder

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// A tree's npm projects arrive installed, never installed on a runner (#v03v751, Oct 10): the first witness of adamic
// main broke at start on every box, prepare.sh running npm ci of stage3/api from the public registry where the serve
// unit has no npm, and a runner never installs and never reads the network. Workshop installs each project once per
// lockfile, from the lockfile alone (npm ci --ignore-scripts), and its node_modules goes into the tree's source as one
// chunk of its own (chunks.go): the tests find it where a checkout's install put it, at <project>/node_modules inside
// the tree (adamic's load walks up to stage3/api/node_modules/@types/node and keeps its path's spelling, and its json
// corpus pins 18 files there by content), and a runner holds and fetches it as it does every chunk. The same lockfile
// makes the same chunk, so a kept tree keeps it and a warm runner unpacks nothing.

// NodeProjects are the tree's npm projects Workshop installs, by directory: what prepare.sh installed on a box.
var NodeProjects = []string{"stage3/api"}

// npmRelease is the npm that installs them, read from the registry and held to npmIntegrity, so no machine's own npm
// (Workshop has none) decides what a tree holds. 11.11.0 writes every file adamic's json corpus pins under
// stage3/api/node_modules byte for byte, .package-lock.json among them (checked Oct 10). Another release may write
// them otherwise, so changing it is a new TreeIndexFormat.
const (
	npmRelease   = "11.11.0"
	npmIntegrity = "sha512-82gRxKrh/eY5UnNorkTFcdBQAGpgjWehkfGVqAGlJjejEtJZGGJUqjo3mbBTNbc5BTnPKGVtGPBZGhElujX5cw=="
)

// npmRegistry is where npm and every package a lockfile resolves come from. Tests set it.
var npmRegistry = "https://registry.npmjs.org"

// A NodeProject is one npm project of the tree as its index names it: its directory, the sha256 of its lockfile, and
// the blob of the source chunk holding its node_modules.
type NodeProject struct {
	Directory string `json:"directory"`
	Lockfile  string `json:"lockfile"`
	Chunk     string `json:"chunk"`
}

// A NodeInstall is one project Workshop installed: its directory in the tree, its lockfile's sha256, and the
// node_modules it made, outside the tree.
type NodeInstall struct {
	Directory   string
	Lockfile    string
	NodeModules string
}

// nodeModules is where a project's packages are in the tree.
func nodeModules(directory string) string {
	return path.Join(directory, "node_modules")
}

// InstallNodePackages installs each of NodeProjects the tree holds a lockfile for under base, once per lockfile: npm ci
// --ignore-scripts with prepare.sh's flags, from npmRegistry, in a staging directory holding only the project's
// package.json and lockfile, with no user or global npm configuration and an npm cache of its own, then moved to
// <base>/<lockfile sha256>/node_modules whole. node is the tree's toolchain's, found on PATH. A project whose install
// fails fails it, named: Loom's, never the change's.
func InstallNodePackages(tree, base string) ([]NodeInstall, error) {
	installs := []NodeInstall{}
	for _, directory := range NodeProjects {
		lockfile := filepath.Join(tree, filepath.FromSlash(directory), "package-lock.json")
		content, err := os.ReadFile(lockfile)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(content)
		install := NodeInstall{Directory: directory, Lockfile: hex.EncodeToString(sum[:])}
		installed := filepath.Join(base, install.Lockfile)
		install.NodeModules = filepath.Join(installed, "node_modules")
		if _, err = os.Stat(install.NodeModules); err != nil {
			if err = npmCi(filepath.Join(tree, filepath.FromSlash(directory)), base, installed); err != nil {
				return nil, fmt.Errorf("installing %s's npm packages (lockfile %s): %w", directory, install.Lockfile, err)
			}
		}
		installs = append(installs, install)
	}
	return installs, nil
}

// npmCi installs the project at project into installed, through staging beside it.
func npmCi(project, base, installed string) error {
	npm, err := ensureNpm(base)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(base, 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(base, ".staging-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err = os.MkdirAll(filepath.Join(staging, "project"), 0o755); err != nil {
		return err
	}
	for _, name := range []string{"package.json", "package-lock.json"} {
		content, err := os.ReadFile(filepath.Join(project, name))
		if err == nil {
			err = os.WriteFile(filepath.Join(staging, "project", name), content, 0o644)
		}
		if err != nil {
			return err
		}
	}
	// No user or global configuration: an empty file of its own for each, since npm refuses one file read as both
	// (Workshop, Oct 10: "double-loading config /dev/null as global, previously loaded as user").
	for _, name := range []string{"userconfig", "globalconfig"} {
		if err = os.WriteFile(filepath.Join(staging, name), nil, 0o644); err != nil {
			return err
		}
	}
	command := exec.Command("node", npm, "ci", "--ignore-scripts", "--no-audit", "--no-fund", "--install-strategy=hoisted", "--registry="+npmRegistry)
	command.Dir = filepath.Join(staging, "project")
	command.Env = append(os.Environ(), "npm_config_update_notifier=false", "npm_config_userconfig="+filepath.Join(staging, "userconfig"),
		"npm_config_globalconfig="+filepath.Join(staging, "globalconfig"), "npm_config_cache="+filepath.Join(staging, "cache"))
	if output, err := command.CombinedOutput(); err != nil {
		tail := output
		if len(tail) > 4000 {
			tail = tail[len(tail)-4000:]
		}
		return fmt.Errorf("npm ci: %v\n%s", err, tail)
	}
	if err = os.MkdirAll(filepath.Join(staging, "installed"), 0o755); err != nil {
		return err
	}
	if err = os.Rename(filepath.Join(staging, "project", "node_modules"), filepath.Join(staging, "installed", "node_modules")); err != nil {
		return err
	}
	return os.Rename(filepath.Join(staging, "installed"), installed)
}

// ensureNpm is npm-cli.js of npmRelease under base, fetched from the registry once and held to npmIntegrity.
func ensureNpm(base string) (string, error) {
	directory := filepath.Join(base, "npm-"+npmRelease)
	cli := filepath.Join(directory, "package", "bin", "npm-cli.js")
	if _, err := os.Stat(cli); err == nil {
		return cli, nil
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	url := npmRegistry + "/npm/-/npm-" + npmRelease + ".tgz"
	response, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("npm %s: %w", npmRelease, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("npm %s: %s answered %s", npmRelease, url, response.Status)
	}
	tarball, err := io.ReadAll(io.LimitReader(response.Body, 256<<20))
	if err != nil {
		return "", fmt.Errorf("npm %s: %w", npmRelease, err)
	}
	sum := sha512.Sum512(tarball)
	if got := "sha512-" + base64.StdEncoding.EncodeToString(sum[:]); got != npmIntegrity {
		return "", fmt.Errorf("npm %s from %s is %s, and Loom pins %s", npmRelease, url, got, npmIntegrity)
	}
	if err = os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(base, ".npm-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	if err = Unpack(bytes.NewReader(tarball), filepath.Join(staging, "npm"), func(name string) bool { return strings.HasPrefix(name, "package/") }); err != nil {
		return "", fmt.Errorf("npm %s: %w", npmRelease, err)
	}
	if err = os.Rename(filepath.Join(staging, "npm"), directory); err != nil {
		return "", err
	}
	return cli, nil
}

// nodeEntries are an install's files and links as the tree holds them, under <directory>/node_modules: a link Unpack
// would refuse, or anything but a file, a link or a directory, fails it.
func nodeEntries(install NodeInstall) ([]archiveEntry, error) {
	entries := []archiveEntry{}
	top := nodeModules(install.Directory)
	err := filepath.WalkDir(install.NodeModules, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(install.NodeModules, file)
		if err != nil || relative == "." {
			return err
		}
		name := path.Join(top, filepath.ToSlash(relative))
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(file)
			if err != nil {
				return err
			}
			if err = checkLink(name, target); err != nil {
				return fmt.Errorf("the npm packages: %w", err)
			}
			entries = append(entries, archiveEntry{Name: name, Link: target})
		case info.Mode().IsRegular():
			entries = append(entries, archiveEntry{Name: name, File: file, Executable: info.Mode()&0o111 != 0})
		default:
			return fmt.Errorf("the npm packages hold %s, neither a file, a link nor a directory", name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%s's npm install holds nothing", install.Directory)
	}
	return entries, nil
}

// NodeChunks names each install's chunk in source, the one chunk holding exactly its node_modules.
func NodeChunks(source Source, installs []NodeInstall) ([]NodeProject, error) {
	projects := []NodeProject{}
	for _, install := range installs {
		prefix := nodeModules(install.Directory) + "/"
		found := ""
		for _, chunk := range source.Chunks {
			if strings.HasPrefix(chunk.First, prefix) || strings.HasPrefix(chunk.Last, prefix) {
				if found != "" || !strings.HasPrefix(chunk.First, prefix) || !strings.HasPrefix(chunk.Last, prefix) {
					return nil, fmt.Errorf("%s's npm packages aren't one chunk of the source", install.Directory)
				}
				found = chunk.Blob
			}
		}
		if found == "" {
			return nil, fmt.Errorf("%s's npm packages are in no chunk of the source", install.Directory)
		}
		projects = append(projects, NodeProject{Directory: install.Directory, Lockfile: install.Lockfile, Chunk: found})
	}
	return projects, nil
}

// checkNode refuses an index's npm projects unless each is one of NodeProjects, once, with a lockfile's sha256, and a
// chunk of its source whose range is inside its node_modules.
func checkNode(index TreeIndex) error {
	known, seen := map[string]bool{}, map[string]bool{}
	for _, directory := range NodeProjects {
		known[directory] = true
	}
	chunks := map[string]SourceChunk{}
	for _, chunk := range index.Source {
		chunks[chunk.Blob] = chunk
	}
	for _, project := range index.Node {
		prefix := nodeModules(project.Directory) + "/"
		chunk, held := chunks[project.Chunk]
		switch {
		case !known[project.Directory] || seen[project.Directory]:
			return fmt.Errorf("its npm project %q isn't one Loom installs, or is named twice", project.Directory)
		case !productKeyPattern.MatchString(project.Lockfile):
			return fmt.Errorf("its npm project %s's lockfile is %q", project.Directory, project.Lockfile)
		case !held || !strings.HasPrefix(chunk.First, prefix) || !strings.HasPrefix(chunk.Last, prefix):
			return fmt.Errorf("its npm project %s's chunk %q isn't a chunk of its source inside %s", project.Directory, project.Chunk, prefix)
		}
		seen[project.Directory] = true
	}
	return nil
}

// CheckNodePackages refuses a tree's assembled source at tree unless the index names an install of every lockfile of
// NodeProjects the source holds, by its sha256, and nothing for a project the source has no lockfile for: an index
// lacking one would run the tests without the packages they read, and they would fail red, charged to the change.
func CheckNodePackages(tree string, index TreeIndex) error {
	named := map[string]string{}
	for _, project := range index.Node {
		named[project.Directory] = project.Lockfile
	}
	for _, directory := range NodeProjects {
		content, err := os.ReadFile(filepath.Join(tree, filepath.FromSlash(directory), "package-lock.json"))
		lockfile, listed := named[directory]
		switch {
		case errors.Is(err, fs.ErrNotExist) && listed:
			return fmt.Errorf("the tree's index names npm packages for %s, and the source has no lockfile there", directory)
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return err
		case !listed:
			return fmt.Errorf("the source holds %s/package-lock.json, and the tree's index names no npm packages installed from it", directory)
		}
		if sum := sha256.Sum256(content); hex.EncodeToString(sum[:]) != lockfile {
			return fmt.Errorf("the tree's index names npm packages for %s installed from lockfile %s, and the source's is %x", directory, lockfile, sum)
		}
	}
	return nil
}
