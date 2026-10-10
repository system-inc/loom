// Package builder is Loom's Builder (gospel on #system_adamic_loom): Workshop builds each action once, keyed by its
// productKey, into loom-pipeline's action store, and test runners only fetch.
//
// An action is one product test, TestProduct_X in package P, on one tree. Its productKey is Planner's unit key with
// kind product (Loom, Oct 9 00:0xZ: one key function, never a fork), whose closure covers the test code that defines
// the build, so one refs/action namespace serves candidates and main alike. Its outputs are every product directory
// adamic's internal/buildcache made while the test ran, named by their buildcache keys: the runner writes them back
// into its ADAMIC_BUILD_CACHE_DIR, and the product test then finds them there and builds nothing.
//
// The store's contract is wire/source/Actions.ts: a manifest blob, canonical, listing each output's path, sha256,
// size and whether it is executable; refs/action/<productKey> naming it, written once, by a build token only.
package builder

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	Bytes      int64  `json:"bytes"`
	Executable bool   `json:"executable"`
	Path       string `json:"path"`
	Sha256     string `json:"sha256"`
}

// A Manifest is what refs/action/<productKey> names: the outputs, sorted by path.
type Manifest struct {
	Key     string   `json:"key"`
	Outputs []Output `json:"outputs"`
}

// Canonical is the manifest's one byte form, as Actions.ts's canonicalManifest writes it: keys sorted (the struct
// fields are declared in that order), outputs sorted by path, no whitespace, and no HTML escaping.
func (manifest Manifest) Canonical() ([]byte, error) {
	outputs := append([]Output{}, manifest.Outputs...)
	sort.Slice(outputs, func(left, right int) bool { return outputs[left].Path < outputs[right].Path })
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(Manifest{Key: manifest.Key, Outputs: outputs}); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// A Store is the action store: Read is the public bucket's address (refs and blobs read direct, unauthenticated),
// Write is loom-pipeline's /actions, and Token a build token, which only a builder holds.
type Store struct {
	Read   string
	Write  string
	Token  string
	Client *http.Client
	// SkipNative leaves out of a fetch every product its buildcache description says clang built, so the runner
	// builds those itself (Judge's ruling until native products are reproducible, #tsn1wp8): Go products serve.
	SkipNative bool
	// Requests counts what this store was asked, for measuring a build's cost (#k62gwdt). Shared by copies of the Store.
	Requests *Requests
}

// Requests counts a store's calls: reads are GETs of refs and blobs, writes are PUTs of blobs and refs.
type Requests struct {
	Reads  atomic.Int64
	Writes atomic.Int64
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

// ErrNotStored is an action with no ref.
var ErrNotStored = errors.New("not in the action store")

func (store Store) client() *http.Client {
	if store.Client != nil {
		return store.Client
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (store Store) get(address string) ([]byte, error) {
	if store.Requests != nil {
		store.Requests.Reads.Add(1)
	}
	response, err := store.client().Get(address)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrNotStored
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", address, response.Status)
	}
	return io.ReadAll(response.Body)
}

// blob reads one blob and checks it hashes to what was asked for.
func (store Store) blob(sum string) ([]byte, error) {
	content, err := store.get(store.Read + "/blobs/" + sum)
	if err != nil {
		return nil, err
	}
	if actual := sha256.Sum256(content); hex.EncodeToString(actual[:]) != sum {
		return nil, fmt.Errorf("blob %s hashes to %x: the store is poisoned", sum, actual)
	}
	return content, nil
}

// Manifest reads refs/action/<key> and the manifest it names, checking the manifest is canonical and for key.
func (store Store) Manifest(key string) (Manifest, error) {
	manifest, _, err := store.manifestAndSum(key)
	return manifest, err
}

func (store Store) manifestAndSum(key string) (Manifest, string, error) {
	reference, err := store.get(store.Read + "/refs/action/" + key)
	if err != nil {
		return Manifest{}, "", err
	}
	target := strings.TrimSpace(string(reference))
	if !productKeyPattern.MatchString(target) {
		return Manifest{}, "", fmt.Errorf("refs/action/%s holds %q, not a manifest's sha256: the store is poisoned", key, target[:min(80, len(target))])
	}
	content, err := store.blob(target)
	if err != nil {
		return Manifest{}, "", err
	}
	var manifest Manifest
	if err = json.Unmarshal(content, &manifest); err != nil {
		return Manifest{}, "", fmt.Errorf("manifest for %s: %w", key, err)
	}
	canonical, err := manifest.Canonical()
	if err != nil {
		return Manifest{}, "", err
	}
	if manifest.Key != key || !bytes.Equal(canonical, content) {
		return Manifest{}, "", fmt.Errorf("the manifest refs/action/%s names isn't that key's canonical manifest: the store is poisoned", key)
	}
	return manifest, target, nil
}

// Fetch writes an action's outputs under directory (a runner's ADAMIC_BUILD_CACHE_DIR). Every blob is read and
// checked against its hash and size into a scratch directory first, and only when all of them check is each product
// renamed into place whole, as buildcache itself publishes one, so a poisoned store leaves nothing behind. A product
// already in the cache is left as it is, since its key says what it holds. The runner doesn't trust the store for
// its own paths: every output is a local path under a buildcache key (<key>/<file> or <key>.inputs), listed once,
// and it is written through an os.Root on the scratch directory, so nothing a manifest says lands outside it.
func (store Store) Fetch(key, directory string) error {
	manifest, err := store.Manifest(key)
	if err != nil {
		return err
	}
	if err = checkOutputPaths(manifest.Outputs); err != nil {
		return fmt.Errorf("action %s: %w: the store is poisoned", key, err)
	}
	if err = os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	scratch, err := os.MkdirTemp(directory, ".fetching-"+key[:min(12, len(key))]+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	root, err := os.OpenRoot(scratch)
	if err != nil {
		return err
	}
	defer root.Close()
	// With SkipNative, each product's description is read first, and a product clang built is left out whole.
	skipped := map[string]bool{}
	if store.SkipNative {
		for _, output := range manifest.Outputs {
			product, isInputs := strings.CutSuffix(output.Path, ".inputs")
			if !isInputs || strings.Contains(product, "/") {
				continue
			}
			content, err := store.blob(output.Sha256)
			if err != nil {
				return fmt.Errorf("action %s, %s: %w", key, output.Path, err)
			}
			if native(content) {
				skipped[product] = true
			}
		}
	}
	for _, output := range manifest.Outputs {
		product, _, _ := strings.Cut(strings.TrimSuffix(output.Path, ".inputs"), "/")
		if skipped[product] {
			continue
		}
		content, err := store.blob(output.Sha256)
		if err != nil {
			return fmt.Errorf("action %s, %s: %w", key, output.Path, err)
		}
		if int64(len(content)) != output.Bytes {
			return fmt.Errorf("action %s: %s is %d bytes, the manifest says %d: the store is poisoned", key, output.Path, len(content), output.Bytes)
		}
		mode := os.FileMode(0o644)
		if output.Executable {
			mode = 0o755
		}
		file := filepath.FromSlash(output.Path)
		if parent := filepath.Dir(file); parent != "." {
			if err = root.MkdirAll(parent, 0o755); err != nil {
				return err
			}
		}
		if err = root.WriteFile(file, content, mode); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(scratch)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		target := filepath.Join(directory, entry.Name())
		if _, err := os.Stat(target); err == nil {
			continue
		}
		if err = os.Rename(filepath.Join(scratch, entry.Name()), target); err != nil {
			return err
		}
	}
	return nil
}

// productKeyPattern is a buildcache key, the first part of every output path.
var productKeyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// checkOutputPaths refuses a manifest whose outputs a runner shouldn't write: a path that isn't local (empty,
// absolute, or climbing out with ..), one not under a buildcache key (<key>/<file>, or <key>.inputs beside it), or
// one listed twice.
func checkOutputPaths(outputs []Output) error {
	seen := map[string]bool{}
	for _, output := range outputs {
		local := filepath.FromSlash(output.Path)
		if !filepath.IsLocal(local) || filepath.ToSlash(filepath.Clean(local)) != output.Path {
			return fmt.Errorf("output path %q isn't a clean local path", output.Path)
		}
		first, rest, nested := strings.Cut(output.Path, "/")
		switch {
		case nested && productKeyPattern.MatchString(first) && rest != "":
		case !nested && strings.HasSuffix(first, ".inputs") && productKeyPattern.MatchString(strings.TrimSuffix(first, ".inputs")):
		default:
			return fmt.Errorf("output path %q isn't under a buildcache key", output.Path)
		}
		if seen[output.Path] {
			return fmt.Errorf("output path %q is listed twice", output.Path)
		}
		seen[output.Path] = true
	}
	return nil
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
		content, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		outputs = append(outputs, Output{Bytes: info.Size(), Executable: info.Mode()&0o111 != 0, Path: relative, Sha256: hex.EncodeToString(sum[:])})
		files[relative] = file
		return nil
	})
	sort.Slice(outputs, func(left, right int) bool { return outputs[left].Path < outputs[right].Path })
	return outputs, files, err
}

// put sends one request to loom-pipeline with the build token, and returns the status and body.
func (store Store) put(address string, body []byte) (int, []byte, error) {
	request, err := http.NewRequest(http.MethodPut, address, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	request.ContentLength = int64(len(body))
	request.Header.Set("Authorization", "Bearer "+store.Token)
	if store.Requests != nil {
		store.Requests.Writes.Add(1)
	}
	response, err := store.client().Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	return response.StatusCode, answer, nil
}

// Upload writes an action: each output's blob, then the canonical manifest, then the ref. A ref the store already
// holds for the same manifest is fine; one naming another manifest is a ConflictError naming both builders.
func (store Store) Upload(key string, outputs []Output, files map[string]string) (string, error) {
	return store.UploadWith(key, outputs, files, nil)
}

// UploadWith is Upload with Workshop's index: a blob the index holds isn't sent, and every blob and ref the store
// takes is recorded in it.
func (store Store) UploadWith(key string, outputs []Output, files map[string]string, index *Index) (string, error) {
	sendBlob := func(sum string, content func() ([]byte, error), name string) error {
		if index != nil && index.Blob(sum) {
			return nil
		}
		body, err := content()
		if err != nil {
			return err
		}
		if status, answer, err := store.put(store.Write+"/blobs/"+sum, body); err != nil || status >= 300 {
			return fmt.Errorf("blob %s (%s): %d %s %v", sum, name, status, answer, err)
		}
		if index != nil {
			return index.AddBlob(sum)
		}
		return nil
	}
	for _, output := range outputs {
		file := files[output.Path]
		if err := sendBlob(output.Sha256, func() ([]byte, error) { return os.ReadFile(file) }, output.Path); err != nil {
			return "", err
		}
	}
	canonical, err := Manifest{Key: key, Outputs: outputs}.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	manifestSha256 := hex.EncodeToString(sum[:])
	if err := sendBlob(manifestSha256, func() ([]byte, error) { return canonical, nil }, "manifest"); err != nil {
		return "", err
	}
	status, answer, err := store.put(store.Write+"/"+key, []byte(manifestSha256))
	if err != nil {
		return "", err
	}
	switch {
	case status == http.StatusCreated || status == http.StatusOK:
		if index != nil {
			if err := index.AddRef(key, manifestSha256); err != nil {
				return "", err
			}
		}
		return manifestSha256, nil
	case status == http.StatusConflict:
		var conflict ConflictError
		if json.Unmarshal(answer, &conflict) == nil && conflict.HeldBuilder != "" {
			return "", conflict
		}
	}
	return "", fmt.Errorf("ref %s: %d %s", key, status, answer)
}

// ConflictError is the store's 409 for an action ref: one key, two different manifests, both builders named. It is a
// reproducibility failure (or a key that isn't honest), and it always fails the build that met it.
type ConflictError struct {
	Message     string `json:"error"`
	Held        string `json:"held"`
	HeldBuilder string `json:"heldBuilder"`
	Builder     string `json:"builder"`
	Sha256      string `json:"sha256"`
}

func (conflict ConflictError) Error() string { return conflict.Message }

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
	Index   *Index       // when set, Workshop's record of the store: deciding reads nothing, uploading skips what it holds
}

// A Result is what happened to one action.
type Result struct {
	Action   Action
	Key      string
	Outcome  string // stored (already in the store), built, failed
	Manifest string
	Outputs  int
	Products int
	Seconds  float64
	Error    string
}

// Build builds every action whose productKey the store doesn't hold, once each, and uploads it. An action already
// stored builds nothing. A product test that fails, or a ref that conflicts, is that action's failure, reported, and
// never uploaded. Results come back in the actions' order.
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
		if content, err := os.ReadFile(inputs); err == nil {
			sum := sha256.Sum256(content)
			outputs = append(outputs, Output{Bytes: int64(len(content)), Path: product + ".inputs", Sha256: hex.EncodeToString(sum[:])})
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
	if builder.Index != nil {
		if manifest, held := builder.Index.Ref(key); held {
			result.Manifest = manifest
			return finish("stored", nil)
		}
	}
	// An index miss (or no index) asks the store once; what it holds goes into the index, so the next build reads
	// nothing for this key.
	switch manifest, manifestSum, err := builder.Store.manifestAndSum(key); {
	case err == nil:
		if builder.Index != nil {
			for _, output := range manifest.Outputs {
				if err = builder.Index.AddBlob(output.Sha256); err != nil {
					return finish("failed", err)
				}
			}
			if err = builder.Index.AddBlob(manifestSum); err != nil {
				return finish("failed", err)
			}
			if err = builder.Index.AddRef(key, manifestSum); err != nil {
				return finish("failed", err)
			}
		}
		result.Manifest = manifestSum
		return finish("stored", nil)
	case !errors.Is(err, ErrNotStored):
		return finish("failed", err)
	}
	if err = os.MkdirAll(builder.Cache, 0o755); err != nil {
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
	// A product test that passed and used no buildcache product is stored too, with no outputs, so the next build
	// knows it needs nothing rather than running it again.
	outputs, files, err := OutputsOf(builder.Cache, products)
	if err != nil {
		return finish("failed", err)
	}
	result.Products, result.Outputs = len(products), len(outputs)
	manifest, err := builder.Store.UploadWith(key, outputs, files, builder.Index)
	result.Manifest = manifest
	if err != nil {
		return finish("failed", err)
	}
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
