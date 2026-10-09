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
	"sort"
	"strings"
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
	reference, err := store.get(store.Read + "/refs/action/" + key)
	if err != nil {
		return Manifest{}, err
	}
	content, err := store.blob(strings.TrimSpace(string(reference)))
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err = json.Unmarshal(content, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("manifest for %s: %w", key, err)
	}
	canonical, err := manifest.Canonical()
	if err != nil {
		return Manifest{}, err
	}
	if manifest.Key != key || !bytes.Equal(canonical, content) {
		return Manifest{}, fmt.Errorf("the manifest refs/action/%s names isn't that key's canonical manifest: the store is poisoned", key)
	}
	return manifest, nil
}

// Fetch writes an action's outputs under directory (a runner's ADAMIC_BUILD_CACHE_DIR). Every blob is read and
// checked against its hash and size into a scratch directory first, and only when all of them check is each product
// renamed into place whole, as buildcache itself publishes one, so a poisoned store leaves nothing behind. A product
// already in the cache is left as it is, since its key says what it holds.
func (store Store) Fetch(key, directory string) error {
	manifest, err := store.Manifest(key)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	scratch, err := os.MkdirTemp(directory, ".fetching-"+key[:min(12, len(key))]+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	for _, output := range manifest.Outputs {
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
		file := filepath.Join(scratch, filepath.FromSlash(output.Path))
		if err = os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return err
		}
		if err = os.WriteFile(file, content, mode); err != nil {
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
	for _, output := range outputs {
		content, err := os.ReadFile(files[output.Path])
		if err != nil {
			return "", err
		}
		if status, answer, err := store.put(store.Write+"/blobs/"+output.Sha256, content); err != nil || status >= 300 {
			return "", fmt.Errorf("blob %s (%s): %d %s %v", output.Sha256, output.Path, status, answer, err)
		}
	}
	canonical, err := Manifest{Key: key, Outputs: outputs}.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	manifestSha256 := hex.EncodeToString(sum[:])
	if status, answer, err := store.put(store.Write+"/blobs/"+manifestSha256, canonical); err != nil || status >= 300 {
		return "", fmt.Errorf("manifest %s: %d %s %v", manifestSha256, status, answer, err)
	}
	status, answer, err := store.put(store.Write+"/"+key, []byte(manifestSha256))
	if err != nil {
		return "", err
	}
	switch {
	case status == http.StatusCreated || status == http.StatusOK:
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
// with its environment (go test, in the tree).
type Builder struct {
	Tree    string
	Store   Store
	Key     func(action Action) (string, error)
	Run     func(action Action, environment []string) ([]byte, error)
	Scratch string // where each action's fresh cache directory goes
}

// A Result is what happened to one action.
type Result struct {
	Action   Action
	Key      string
	Outcome  string // stored (already in the store), built, failed
	Manifest string
	Outputs  int
	Seconds  float64
	Error    string
}

// Build builds every action whose productKey the store doesn't hold, once each, and uploads it. An action already
// stored builds nothing. A product test that fails, or a ref that conflicts, is that action's failure, reported, and
// never uploaded.
func (builder Builder) Build(actions []Action) []Result {
	results := []Result{}
	for _, action := range actions {
		results = append(results, builder.build(action))
	}
	return results
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
	switch _, err = builder.Store.Manifest(key); {
	case err == nil:
		return finish("stored", nil)
	case !errors.Is(err, ErrNotStored):
		return finish("failed", err)
	}
	cache, err := os.MkdirTemp(builder.Scratch, "action-"+key[:12]+"-")
	if err != nil {
		return finish("failed", err)
	}
	defer os.RemoveAll(cache)
	environment := append(GateEnvironment(), "ADAMIC_BUILD_CACHE_DIR="+cache, "ADAMIC_BUILD_STORE=off", "ADAMIC_BUILD_CACHE=on")
	if output, err := builder.Run(action, environment); err != nil {
		tail := output
		if len(tail) > 4000 {
			tail = tail[len(tail)-4000:]
		}
		return finish("failed", fmt.Errorf("%s %s: %v\n%s", action.Directory, action.Test, err, tail))
	}
	outputs, files, err := Outputs(cache)
	if err != nil {
		return finish("failed", err)
	}
	if len(outputs) == 0 {
		return finish("failed", fmt.Errorf("%s %s built no product", action.Directory, action.Test))
	}
	result.Outputs = len(outputs)
	manifest, err := builder.Store.Upload(key, outputs, files)
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
