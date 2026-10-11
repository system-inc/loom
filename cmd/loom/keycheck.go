package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/runner"
)

// The key check (#nm31pcn, Loom Oct 10 16:17 MDT, design A sampled): a product's buildcache key must be one key on
// Workshop and on a runner, or a unit that reads it misses it and the runner refuses to build it (verify run 4: every
// GoBuild product keyed under Workshop's GOFLAGS=-p=7 and asked for under the box's). Before a tree's index goes up,
// build-tree runs each package's product tests once more the way a runner would: Workshop's own test binary, in the
// tree's source assembled from its chunks at another path, under planner.UnitEnvironment, behind the runner's stand-in
// go (runner.StandInGo), against a cache holding only that package's products. Every product a test asks for must be a
// hit; one that isn't moved between Workshop and a runner, and that package's index entry becomes Workshop's failure,
// so its units are Loom's from the start, named, before any runs.
//
// A moved key fails safe without it (the runner refuses the build, the unit is void naming the go command), so the
// check runs only where drift is likely: on the first tree after the loom release, the unit environment or the tree's
// Go release changes, and then on one tree in ten (keyCheckEvery).

// keyCheckEvery is how many trees a check covers: after a checked tree, the tenth that follows is checked again.
const keyCheckEvery = 10

// A keyCheckState is the check's sampling, kept under build-tree's cache base: the signature of the last tree it
// checked and how many trees were built since.
type keyCheckState struct {
	Signature string `json:"signature"`
	Since     int    `json:"since"`
}

// keyCheckSignature is what a check is due on when it changes: this build-tree's own binary (the loom release), the
// unit environment and the tree's Go release.
func keyCheckSignature(goRelease string) (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	content, err := os.ReadFile(executable)
	if err != nil {
		return "", err
	}
	binary := sha256.Sum256(content)
	sum := sha256.Sum256([]byte("loom-key-check-v1\n" + hex.EncodeToString(binary[:]) + "\n" + strings.Join(planner.UnitEnvironment(planner.RunnersGoos, planner.RunnersGoarch), "\n") + "\n" + goRelease))
	return hex.EncodeToString(sum[:]), nil
}

// keyCheckDue decides, under a lock on the state file, whether this tree is checked, and records the decision:
// mode "always" and "never" decide alone; "sampled" checks a new signature, or the keyCheckEvery-th tree since the last
// check. The reason says why, for the summary.
func keyCheckDue(base, mode, signature string) (bool, string, error) {
	switch mode {
	case "always":
		return true, "always (--key-check always)", nil
	case "never":
		return false, "never (--key-check never)", nil
	case "sampled":
	default:
		return false, "", fmt.Errorf("--key-check is always, sampled or never, not %q", mode)
	}
	path := filepath.Join(base, "key-check.json")
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false, "", err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return false, "", err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	state := keyCheckState{}
	if content, err := os.ReadFile(path); err == nil {
		if err = json.Unmarshal(content, &state); err != nil {
			state = keyCheckState{}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, "", err
	}
	due, reason := false, ""
	switch {
	case state.Signature != signature:
		due, reason = true, "the first tree since the loom release, the unit environment or the tree's Go release changed"
	case state.Since+1 >= keyCheckEvery:
		due, reason = true, fmt.Sprintf("one tree in %d", keyCheckEvery)
	default:
		reason = fmt.Sprintf("sampled: %d trees since the last check, of %d", state.Since+1, keyCheckEvery)
	}
	if due {
		state = keyCheckState{Signature: signature}
	} else {
		state.Since++
	}
	content, err := json.Marshal(state)
	if err != nil {
		return false, "", err
	}
	if err = os.WriteFile(path+".new", content, 0o644); err != nil {
		return false, "", err
	}
	return due, reason, os.Rename(path+".new", path)
}

// A keyCheck is one check's outcome: the packages it ran and, for each package with a product its tests asked for and
// didn't find, what moved (each product's census line, or the go command the stand-in refused). Failed are packages
// whose product tests failed with nothing moved, said but never gating: a test's own flake isn't a key's drift.
type keyCheck struct {
	Packages int `json:"packages"`
	// Hits counts the products the check's tests found where Workshop put them.
	Hits    int                 `json:"hits"`
	Moved   map[string][]string `json:"moved"`
	Failed  map[string]string   `json:"failed"`
	Seconds float64             `json:"seconds"`
	// AssembleSeconds and CopySeconds are the source's assembly from its chunks and the products' copies.
	AssembleSeconds float64 `json:"assembleSeconds"`
	CopySeconds     float64 `json:"copySeconds"`
}

// censusOutcome reads a buildcache census line in ADAMIC_BUILD_LOG: build <name> <key12> <outcome> <seconds>.
func censusOutcome(line string) (name, key, outcome string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) != 5 || fields[0] != "build" {
		return "", "", "", false
	}
	return fields[1], fields[2], fields[3], true
}

// checkKeys runs the check on a built tree: built are its packages (a package whose binary failed is left out),
// products each package's buildcache keys, source its chunks, release its Go release. jobs packages run at once.
func checkKeys(checkContext context.Context, build builder.TreeBuild, directory string, built []builder.TreePackage, products map[string][]string,
	source builder.Source, release string, jobs int) (keyCheck, error) {
	started := time.Now()
	result := keyCheck{Moved: map[string][]string{}, Failed: map[string]string{}}
	root := filepath.Join(directory, "key-check")
	if err := os.RemoveAll(root); err != nil {
		return result, err
	}
	defer os.RemoveAll(root)
	// The source a runner assembles, from the same chunks, at a path that isn't the tree's.
	tree := filepath.Join(root, "source")
	for _, chunk := range source.Chunks {
		blob, held := source.Blobs[chunk.Blob]
		if !held {
			return result, fmt.Errorf("the source's chunk %s isn't held", chunk.Blob)
		}
		if err := builder.UnpackChunk(bytes.NewReader(blob), tree, chunk); err != nil {
			return result, err
		}
	}
	result.AssembleSeconds = time.Since(started).Seconds()
	moduleCache, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		return result, err
	}
	checked := []builder.TreePackage{}
	for _, pkg := range built {
		if pkg.Error == "" && len(products[pkg.Package]) > 0 {
			checked = append(checked, pkg)
		}
	}
	result.Packages = len(checked)
	var mutex sync.Mutex
	var copySeconds float64
	errs := make([]error, len(checked))
	next := make(chan int)
	var group sync.WaitGroup
	for range max(1, jobs) {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range next {
				pkg := checked[index]
				unit := filepath.Join(root, "units", fmt.Sprint(index))
				copied := time.Now()
				cache := filepath.Join(unit, "adamic-build")
				for _, key := range products[pkg.Package] {
					if err := copyProduct(build.Cache, cache, key); err != nil {
						errs[index] = fmt.Errorf("%s: %w", pkg.Package, err)
						break
					}
				}
				// The tree's local pointers beside them, as a runner unpacks them.
				if errs[index] == nil {
					if err := copyPointers(build.Cache, cache); err != nil {
						errs[index] = fmt.Errorf("%s: %w", pkg.Package, err)
					}
				}
				seconds := time.Since(copied).Seconds()
				if errs[index] != nil {
					continue
				}
				moved, hits, failed, err := checkPackage(checkContext, build, pkg, products[pkg.Package], tree, unit, cache, release, strings.TrimSpace(string(moduleCache)))
				mutex.Lock()
				copySeconds += seconds
				result.Hits += hits
				if len(moved) > 0 {
					result.Moved[pkg.Package] = moved
				} else if failed != "" {
					result.Failed[pkg.Package] = failed
				}
				mutex.Unlock()
				errs[index] = err
			}
		}()
	}
	for index := range checked {
		next <- index
	}
	close(next)
	group.Wait()
	result.CopySeconds = copySeconds
	result.Seconds = time.Since(started).Seconds()
	return result, errors.Join(errs...)
}

// checkPackage runs one package's product tests as a runner would and returns each product that wasn't a hit, how many
// were, and the output's tail when the tests failed with nothing moved.
func checkPackage(checkContext context.Context, build builder.TreeBuild, pkg builder.TreePackage, products []string, tree, unit, cache, release, moduleCache string) ([]string, int, string, error) {
	log := filepath.Join(unit, "builds.log")
	environment := map[string]string{}
	for _, variable := range os.Environ() {
		if name, value, found := strings.Cut(variable, "="); found {
			environment[name] = value
		}
	}
	for _, variable := range append(planner.UnitEnvironment(runtime.GOOS, runtime.GOARCH), "GOCACHE="+filepath.Join(unit, "gocache"),
		"ADAMIC_BUILD_CACHE_DIR="+cache, "ADAMIC_BUILD_CACHE=on", "ADAMIC_BUILD_STORE=off", "ADAMIC_BUILD_LOG="+log) {
		name, value, _ := strings.Cut(variable, "=")
		environment[name] = value
	}
	delete(environment, "GOCACHEPROG")
	standIn, err := runner.StandInGo(checkContext, unit, tree, release, "off", moduleCache, "", false, environment, func(string) {})
	if err != nil {
		return nil, 0, "", fmt.Errorf("%s: %w", pkg.Package, err)
	}
	directory := filepath.Join(tree, filepath.FromSlash(pkg.Directory))
	binary := filepath.Join(build.Out, strings.ReplaceAll(pkg.Package, "/", "_")+".test")
	command := exec.CommandContext(checkContext, binary, "-test.run=^TestProduct_", "-test.count=1", "-test.v", "-test.timeout=10m")
	command.Dir = directory
	for name, value := range environment {
		command.Env = append(command.Env, name+"="+value)
	}
	command.Env = append(command.Env, "PWD="+directory)
	output, runErr := command.CombinedOutput()
	// Each product the runner side didn't find, with the key Workshop built it under beside the runner's, since the
	// pair says which side drifted: Workshop's products of that name its tests didn't hit.
	type missed struct{ name, key, outcome string }
	misses, hit, hits := []missed{}, map[string]bool{}, 0
	if file, err := os.Open(log); err == nil {
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			name, key, outcome, ok := censusOutcome(scanner.Text())
			switch {
			case ok && outcome == "hit":
				hits++
				hit[key] = true
			case ok:
				misses = append(misses, missed{name, key, outcome})
			}
		}
		file.Close()
	}
	moved := []string{}
	for _, miss := range misses {
		workshop := []string{}
		for _, key := range products {
			if !hit[key[:min(12, len(key))]] && productName(cache, key) == miss.name {
				workshop = append(workshop, key[:min(12, len(key))])
			}
		}
		if len(workshop) == 0 {
			workshop = []string{"none"}
		}
		moved = append(moved, fmt.Sprintf("%s workshop %s runner %s %s", miss.name, strings.Join(workshop, ","), miss.key, miss.outcome))
	}
	for _, refused := range standIn.Refused() {
		moved = append(moved, "a test ran "+refused)
	}
	sort.Strings(moved)
	failed := ""
	if runErr != nil && len(moved) == 0 {
		tail := output
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		failed = fmt.Sprintf("%v\n%s", runErr, tail)
	}
	return moved, hits, failed, nil
}

// productName is a product's name as its census line says it, from the first line of the .inputs buildcache wrote
// beside it ("name <name>"), wherever the product is (builder.ProductPath), spaces as underscores; "" when it has none.
func productName(cache, key string) string {
	place, err := builder.ProductPath(cache, key)
	if err != nil {
		return ""
	}
	content, err := os.ReadFile(filepath.Join(cache, filepath.FromSlash(place)+".inputs"))
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(string(content), "\n")
	name, found := strings.CutPrefix(first, "name ")
	if !found {
		return ""
	}
	return strings.ReplaceAll(name, " ", "_")
}

// copyProduct copies product key, its directory and its .inputs, from one buildcache directory into another, to the
// same place, at the top or in builder.LocalDirectory, as a runner unpacks it: a copy, never a link, so a test that
// writes into a product changes none of the tree's.
func copyProduct(from, to, key string) error {
	place, err := builder.ProductPath(from, key)
	if err != nil {
		return err
	}
	place = filepath.FromSlash(place)
	if err := os.MkdirAll(filepath.Dir(filepath.Join(to, place)), 0o755); err != nil {
		return err
	}
	if err := copyFile(filepath.Join(from, place+".inputs"), filepath.Join(to, place+".inputs"), 0o644); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	source := filepath.Join(from, place)
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, place, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, 0o755)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			return copyFile(path, target, info.Mode().Perm())
		}
	})
}

// copyPointers copies every local pointer (builder.LocalPointers) from one buildcache directory into another, as a
// runner unpacks the tree's: how adamic finds a product another product names by its name key.
func copyPointers(from, to string) error {
	pointers, err := builder.LocalPointers(from)
	if err != nil {
		return err
	}
	for _, pointer := range pointers {
		target := filepath.Join(to, filepath.FromSlash(pointer))
		if err = os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err = copyFile(filepath.Join(from, filepath.FromSlash(pointer)), target, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(from, to string, mode fs.FileMode) error {
	input, err := os.Open(from)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(output, input)
	if closeErr := output.Close(); err == nil {
		err = closeErr
	}
	return err
}
