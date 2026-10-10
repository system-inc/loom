package builder

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/system-inc/loom/r2"
)

// A tree builds only the products the store doesn't hold (Loom, Oct 10). Which products a product test needs is known
// only to adamic's buildcache, which keys each one as the test runs, so the store is offered to buildcache itself: a
// HeldProducts server on 127.0.0.1 speaks buildcache's shared-tier protocol (its store.go, ADAMIC_BUILD_STORE), and a
// product buildcache asks for is answered from refs/action/<key> and the archive it names. buildcache then writes it
// into the tree's cache and logs it fetched, and the product test only checks it, so an unchanged product is neither
// rebuilt nor sent again: with Workshop the one builder, a ref can only conflict through a real race or a key that
// isn't honest. A held archive uploaded more than FreshFor ago is sent again, the same bytes, which resets its clock.
//
// buildcache's protocol: GET <store>/refs/build/<key> is the sha256 of a manifest, {"version":1,"key":...,"name":...,
// "files":[{"path","sha256","size","mode"}]}, and GET <store>/blobs/<sha256> is the manifest or one file; a 404 is a
// miss, which buildcache builds. It never writes here: build-tree gives it no write credential.

// heldManifest is buildcache's manifest of one product.
type heldManifest struct {
	Version int        `json:"version"`
	Key     string     `json:"key"`
	Name    string     `json:"name"`
	Files   []heldFile `json:"files"`
}

type heldFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
}

// HeldProducts serves the store's products to buildcache for one tree's build.
type HeldProducts struct {
	// Address is what ADAMIC_BUILD_STORE names, http://127.0.0.1:<port>.
	Address string

	store    Store
	scratch  string
	server   *http.Server
	mutex    sync.Mutex
	asked    map[string]*sync.Mutex
	refs     map[string]string // product key to its manifest's sha256, for a product the store holds
	blobs    map[string]string // a manifest's or a file's sha256 to the file holding it
	held     map[string]string // product key to its archive's sha256
	failures []error
}

// ServeHeldProducts starts the server on 127.0.0.1, unpacking what it fetches under scratch.
func ServeHeldProducts(store Store, scratch string) (*HeldProducts, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	held := &HeldProducts{
		Address: "http://" + listener.Addr().String(),
		store:   store, scratch: scratch,
		asked: map[string]*sync.Mutex{}, refs: map[string]string{}, blobs: map[string]string{}, held: map[string]string{},
	}
	held.server = &http.Server{Handler: held}
	go held.server.Serve(listener)
	return held, nil
}

// Close stops the server.
func (held *HeldProducts) Close() error {
	return held.server.Close()
}

// Held is every product the server gave buildcache, by key, with its archive's sha256: products this build didn't
// make, which it needn't send.
func (held *HeldProducts) Held() map[string]string {
	held.mutex.Lock()
	defer held.mutex.Unlock()
	copied := map[string]string{}
	for key, sum := range held.held {
		copied[key] = sum
	}
	return copied
}

// Err is every product the store held but couldn't give whole, which buildcache then built: a poisoned store or
// one that failed to answer, which fails the tree's build however the product test went.
func (held *HeldProducts) Err() error {
	held.mutex.Lock()
	defer held.mutex.Unlock()
	return errors.Join(held.failures...)
}

func (held *HeldProducts) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		http.Error(writer, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if key, isRef := strings.CutPrefix(request.URL.Path, "/refs/build/"); isRef && productKeyPattern.MatchString(key) {
		manifest, err := held.prepare(key)
		if err != nil {
			held.mutex.Lock()
			held.failures = append(held.failures, fmt.Errorf("product %s: %w", key, err))
			held.mutex.Unlock()
			http.Error(writer, err.Error(), http.StatusBadGateway)
			return
		}
		if manifest == "" {
			http.NotFound(writer, request)
			return
		}
		writer.Write([]byte(manifest))
		return
	}
	if sum, isBlob := strings.CutPrefix(request.URL.Path, "/blobs/"); isBlob {
		held.mutex.Lock()
		file, found := held.blobs[sum]
		held.mutex.Unlock()
		if found {
			http.ServeFile(writer, request, file)
			return
		}
	}
	http.NotFound(writer, request)
}

// prepare readies key's product once: its ref, its archive (checked, refreshed when stale, unpacked), and the
// manifest buildcache reads. It returns the manifest's sha256, or "" when the store holds no whole product.
func (held *HeldProducts) prepare(key string) (string, error) {
	held.mutex.Lock()
	once, started := held.asked[key]
	if !started {
		once = &sync.Mutex{}
		held.asked[key] = once
	}
	held.mutex.Unlock()
	once.Lock()
	defer once.Unlock()
	held.mutex.Lock()
	manifest, done := held.refs[key]
	held.mutex.Unlock()
	if done {
		return manifest, nil
	}
	sum, err := held.store.heldRef(key)
	if err != nil || sum == "" {
		return "", err
	}
	held.store.read()
	object, err := held.store.Bucket.Head("blobs/" + sum)
	if errors.Is(err, r2.ErrNotFound) {
		// A ref whose blob the lifecycle took first: buildcache builds it, and the ref decides what that build may be.
		return "", nil
	}
	if err != nil {
		return "", err
	}
	held.store.read()
	archive, err := held.store.Bucket.Get("blobs/" + sum)
	if err != nil {
		return "", err
	}
	if actual := digest(archive); actual != sum {
		return "", fmt.Errorf("blob %s hashes to %s: the store is poisoned", sum, actual)
	}
	directory := filepath.Join(held.scratch, key)
	own := func(name string) bool {
		return name == key+".inputs" || (strings.HasPrefix(name, key+"/") && len(name) > len(key)+1)
	}
	if err = Unpack(archive, directory, own); err != nil {
		return "", fmt.Errorf("%w: the store is poisoned", err)
	}
	if held.store.now().Sub(object.Modified) >= FreshFor {
		held.store.wrote()
		if err = held.store.Bucket.Put("blobs/"+sum, archive, r2.PutOptions{ContentType: "application/octet-stream", CacheControl: ImmutableBlob}); err != nil {
			return "", err
		}
	}
	product := heldManifest{Version: 1, Key: key, Files: []heldFile{}}
	blobs := map[string]string{}
	root := filepath.Join(directory, key)
	err = filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s isn't a regular file", file)
		}
		content, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		mode := uint32(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		fileSum := digest(content)
		product.Files = append(product.Files, heldFile{Path: filepath.ToSlash(relative), SHA256: fileSum, Size: info.Size(), Mode: mode})
		blobs[fileSum] = file
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		err = os.MkdirAll(root, 0o755)
	}
	if err != nil {
		return "", err
	}
	sort.Slice(product.Files, func(left, right int) bool { return product.Files[left].Path < product.Files[right].Path })
	encoded, err := json.Marshal(product)
	if err != nil {
		return "", err
	}
	manifest = digest(encoded)
	manifestFile := filepath.Join(directory, "manifest.json")
	if err = os.WriteFile(manifestFile, encoded, 0o644); err != nil {
		return "", err
	}
	held.mutex.Lock()
	defer held.mutex.Unlock()
	for fileSum, file := range blobs {
		held.blobs[fileSum] = file
	}
	held.blobs[manifest] = manifestFile
	held.refs[key] = manifest
	held.held[key] = sum
	return manifest, nil
}
