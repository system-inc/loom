// Package builder is Loom's Builder (gospel on #system_adamic_loom): Workshop builds each action once, keyed by its
// productKey, into loom's action store, and test runners only fetch.
//
// An action is one product test, TestProduct_X in package P, on one tree. Its productKey is Planner's unit key with
// kind product (Loom, Oct 9 00:0xZ: one key function, never a fork), whose closure covers the test code that defines
// the build, so one refs/action namespace serves candidates and main alike. Its outputs are every product directory
// adamic's internal/buildcache made while the test ran, named by their buildcache keys: the runner writes them back
// into its ADAMIC_BUILD_CACHE_DIR, and the product test then finds them there and builds nothing.
//
// The store is store.go's: one gzipped archive of the outputs at blobs/<sha256>, and refs/action/<productKey> naming
// it, written once, straight to R2 with the builder's own key.
package builder

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// An Action is one product test on a tree.
type Action struct {
	Package   string // import path
	Directory string // the package's directory in the tree, repo-relative, as go test takes it (./...)
	Test      string // TestProduct_X
}

// An Output is one file of an action's products, at its path under the cache directory.
type Output struct {
	Path       string
	Bytes      int64
	Executable bool
}

// nativeToolLine is how buildcache's description of a product names clang among its tools.
const nativeToolLine = "tool clang --version:"

// native reports whether a product's .inputs description names clang as one of its tools.
func native(inputs []byte) bool {
	for _, line := range strings.Split(string(inputs), "\n") {
		if strings.HasPrefix(line, nativeToolLine) {
			return true
		}
	}
	return false
}

// productKeyPattern is a buildcache key, the first part of every output path, and a sha256 anywhere in the store.
var productKeyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// buildcachePath is a product's file under its buildcache key, <key>/<file>, or its description beside it, <key>.inputs.
func buildcachePath(path string) bool {
	first, rest, nested := strings.Cut(path, "/")
	if nested {
		return productKeyPattern.MatchString(first) && rest != ""
	}
	return strings.HasSuffix(first, ".inputs") && productKeyPattern.MatchString(strings.TrimSuffix(first, ".inputs"))
}

// ProductEntries is what one product's archive may hold, for Unpack: that product's files, <key>/<file>, and its
// description, <key>.inputs, and nothing of any other product's.
func ProductEntries(key string) func(name string) bool {
	return func(name string) bool {
		return name == key+".inputs" || (strings.HasPrefix(name, key+"/") && len(name) > len(key)+1)
	}
}

// Outputs lists every file under directory (a fresh ADAMIC_BUILD_CACHE_DIR after one product test) as the action's
// outputs, skipping buildcache's own bookkeeping: lock files and unfinished .building- directories.
func Outputs(directory string) ([]Output, map[string]string, error) {
	outputs := []Output{}
	files := map[string]string{}
	err := filepath.WalkDir(directory, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(directory, file)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == "." {
			return nil
		}
		top, _, _ := strings.Cut(relative, "/")
		if strings.HasPrefix(top, ".") || strings.HasSuffix(top, ".lock") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link; an action's outputs are regular files", relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		outputs = append(outputs, Output{Path: relative, Bytes: info.Size(), Executable: info.Mode()&0o111 != 0})
		files[relative] = file
		return nil
	})
	sort.Slice(outputs, func(left, right int) bool { return outputs[left].Path < outputs[right].Path })
	return outputs, files, err
}

// A Builder builds actions on one tree: Key is Planner's productKey for an action, and Run runs one product test
// with its environment (go test, in the tree). Every action of one Build shares one buildcache directory, Cache, so
// an upstream product several product tests need (a stage 0, a checker archive) builds once for all of them, and
// buildcache's own lock keeps two jobs from building one key at once. Jobs actions run at a time.
type Builder struct {
	Tree    string
	Store   Store
	Key     func(action Action) (string, error)
	Run     func(action Action, environment []string) ([]byte, error)
	Cache   string // the build's buildcache directory, shared by its actions
	Scratch string // where each action's build log goes
	Jobs    int
	Report  func(Result) // when set, hears each action's result as it finishes, one at a time
}

// A Result is what happened to one action.
type Result struct {
	Action   Action
	Key      string
	Outcome  string // stored (already in the store), built, failed
	Archive  string // the sha256 of its archive
	Outputs  int
	Products int
	Seconds  float64
	Error    string
	Note     string `json:",omitempty"` // why a product the store has a ref for was built again
}

// Build builds every action whose productKey the store doesn't hold whole, once each, and uploads it. An
// action already stored builds nothing. A product test that fails, or a ref that conflicts, is that action's failure,
// reported, and never uploaded. Results come back in the actions' order.
func (builder Builder) Build(actions []Action) []Result {
	jobs := max(1, builder.Jobs)
	results := make([]Result, len(actions))
	next := make(chan int)
	var group sync.WaitGroup
	var reporting sync.Mutex
	for range jobs {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range next {
				results[index] = builder.build(actions[index])
				if builder.Report != nil {
					reporting.Lock()
					builder.Report(results[index])
					reporting.Unlock()
				}
			}
		}()
	}
	for index := range actions {
		next <- index
	}
	close(next)
	group.Wait()
	return results
}

// buildLinePattern is a line buildcache's record writes to ADAMIC_BUILD_LOG: build <name> <key prefix> <outcome> <s>.
var buildLinePattern = regexp.MustCompile(`^build \S+ ([0-9a-f]{12}) (hit|miss|fetched|audited) `)

// Touched reads one action's build log and names every buildcache product it used, by its full key, resolved from
// the 12-character prefix the log carries against the product directories in cache. A prefix that names no product
// or two is an error, never a guess.
func Touched(log, cache string) ([]string, error) {
	content, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		return nil, err
	}
	products := []string{}
	for _, entry := range entries {
		if entry.IsDir() && len(entry.Name()) == 64 && !strings.HasPrefix(entry.Name(), ".") {
			products = append(products, entry.Name())
		}
	}
	keys := map[string]bool{}
	for _, line := range strings.Split(string(content), "\n") {
		match := buildLinePattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		found := []string{}
		for _, product := range products {
			if strings.HasPrefix(product, match[1]) {
				found = append(found, product)
			}
		}
		if len(found) != 1 {
			return nil, fmt.Errorf("the build log names product %s, and the cache holds %d products with that prefix", match[1], len(found))
		}
		keys[found[0]] = true
	}
	touched := make([]string, 0, len(keys))
	for key := range keys {
		touched = append(touched, key)
	}
	sort.Strings(touched)
	return touched, nil
}

// OutputsOf lists the files of the named buildcache products in cache, each directory's files under its key and
// its .inputs file beside it, as one action's outputs.
func OutputsOf(cache string, products []string) ([]Output, map[string]string, error) {
	outputs := []Output{}
	files := map[string]string{}
	for _, product := range products {
		listed, listedFiles, err := Outputs(filepath.Join(cache, product))
		if err != nil {
			return nil, nil, err
		}
		for _, output := range listed {
			files[product+"/"+output.Path] = listedFiles[output.Path]
			output.Path = product + "/" + output.Path
			outputs = append(outputs, output)
		}
		inputs := filepath.Join(cache, product+".inputs")
		if info, err := os.Stat(inputs); err == nil {
			outputs = append(outputs, Output{Path: product + ".inputs", Bytes: info.Size()})
			files[product+".inputs"] = inputs
		}
	}
	sort.Slice(outputs, func(left, right int) bool { return outputs[left].Path < outputs[right].Path })
	return outputs, files, nil
}

func (builder Builder) build(action Action) Result {
	started := time.Now()
	result := Result{Action: action}
	finish := func(outcome string, err error) Result {
		result.Outcome = outcome
		result.Seconds = time.Since(started).Seconds()
		if err != nil {
			result.Error = err.Error()
		}
		return result
	}
	key, err := builder.Key(action)
	if err != nil {
		return finish("failed", fmt.Errorf("productKey: %w", err))
	}
	result.Key = key
	archive, stored, err := builder.Store.Stored(key)
	if err != nil {
		return finish("failed", err)
	}
	if stored {
		result.Archive = archive
		return finish("stored", nil)
	}
	if archive != "" {
		result.Note = fmt.Sprintf("refs/action/%s names blob %s, which the store no longer holds, so it is built again", key, archive)
	}
	return builder.buildAndUpload(action, key, &result, finish)
}

// buildAndUpload runs one action's product test and uploads what it used, as one archive.
func (builder Builder) buildAndUpload(action Action, key string, result *Result, finish func(string, error) Result) Result {
	if err := os.MkdirAll(builder.Cache, 0o755); err != nil {
		return finish("failed", err)
	}
	logs, err := os.MkdirTemp(builder.Scratch, "action-"+key[:12]+"-")
	if err != nil {
		return finish("failed", err)
	}
	defer os.RemoveAll(logs)
	log := filepath.Join(logs, "builds.log")
	environment := append(GateEnvironment(), "ADAMIC_BUILD_CACHE_DIR="+builder.Cache, "ADAMIC_BUILD_LOG="+log, "ADAMIC_BUILD_STORE=off", "ADAMIC_BUILD_CACHE=on")
	if output, err := builder.Run(action, environment); err != nil {
		tail := output
		if len(tail) > 4000 {
			tail = tail[len(tail)-4000:]
		}
		return finish("failed", fmt.Errorf("%s %s: %v\n%s", action.Directory, action.Test, err, tail))
	}
	products, err := Touched(log, builder.Cache)
	if err != nil {
		return finish("failed", err)
	}
	// A product test that passed and used no buildcache product is stored too, as an empty archive, so the next
	// build knows it needs nothing rather than running it again.
	archive, files, err := ProductArchive(builder.Cache, products)
	if err != nil {
		return finish("failed", err)
	}
	result.Products, result.Outputs = len(products), files
	sum, err := builder.Store.Publish(key, archive)
	if err != nil {
		return finish("failed", err)
	}
	result.Archive = sum
	return finish("built", nil)
}

// GoTest runs one product test in tree with go test, the environment on top of this process's: the default Run.
func GoTest(tree string) func(action Action, environment []string) ([]byte, error) {
	return func(action Action, environment []string) ([]byte, error) {
		command := exec.Command("go", "test", "-count=1", "-run", "^"+action.Test+"$", "./"+path.Clean(action.Directory))
		command.Dir = tree
		command.Env = append(os.Environ(), environment...)
		return command.CombinedOutput()
	}
}
