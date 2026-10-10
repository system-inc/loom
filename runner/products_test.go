package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/system-inc/loom/protocol"
)

// productStore is the public action store as artifacts.loom.system.inc serves it: refs/action/<key> holds an
// archive's sha256 as text, and blobs/<sha256> holds bytes. It notes every Authorization header it was sent.
type productStore struct {
	mutex   sync.Mutex
	objects map[string][]byte
	tokens  []string
	server  *httptest.Server
}

func newProductStore(t *testing.T) *productStore {
	store := &productStore{objects: map[string][]byte{}}
	store.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		store.mutex.Lock()
		defer store.mutex.Unlock()
		if token := request.Header.Get("Authorization"); token != "" {
			store.tokens = append(store.tokens, token)
		}
		object, held := store.objects[strings.TrimPrefix(request.URL.Path, "/")]
		if !held {
			http.NotFound(writer, request)
			return
		}
		writer.Write(object)
	}))
	t.Cleanup(store.server.Close)
	return store
}

func digest(bytes []byte) string {
	sum := sha256.Sum256(bytes)
	return hex.EncodeToString(sum[:])
}

// build puts a product's archive, a gzipped tar of files, and its ref in the store, as a builder would. Its entries
// go in as given, unchecked, for the cases where a builder or a store lies.
func (store *productStore) build(key string, files map[string]string, executable map[string]bool) []byte {
	var buffer bytes.Buffer
	compressor := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressor)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		mode := int64(0o644)
		if executable[name] {
			mode = 0o755
		}
		writer.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(files[name])), Typeflag: tar.TypeReg})
		writer.Write([]byte(files[name]))
	}
	writer.Close()
	compressor.Close()
	store.put(key, buffer.Bytes())
	return buffer.Bytes()
}

// put stores archive under its sha256 and points key's ref at it.
func (store *productStore) put(key string, archive []byte) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	store.objects["blobs/"+digest(archive)] = archive
	store.objects["refs/action/"+key] = []byte(digest(archive) + "\n")
}

var productKey = strings.Repeat("a", 64)

// cached is a path inside a product as buildcache lays it out: under one of its own keys.
var cached = strings.Repeat("d", 64) + "/"

func productUnit(store *productStore, argv ...string) protocol.Unit {
	unit := testUnit(argv...)
	unit.Products = []protocol.Product{{Key: productKey, Directory: "products/lint"}}
	unit.ProductStore = store.server.URL
	return unit
}

func fetchError(events []protocol.Event) string {
	for _, event := range events {
		if event.Type == "error" && event.Phase == protocol.PhaseFetch {
			return event.Message
		}
	}
	return ""
}

// Runners fetch and never build (contracts v1.1): a product's outputs land in its directory, each at its archive
// path, executable where the archive says, before the command runs; and the run's token never goes to the public store.
func TestAProductsOutputsLandInItsDirectoryBeforeTheCommand(t *testing.T) {
	store := newProductStore(t)
	store.build(productKey, map[string]string{cached + "lint": "#!/bin/sh\necho linted\n", cached + "rules.json": "{}\n"}, map[string]bool{cached + "lint": true})
	directory := "products/lint/" + cached
	unit := productUnit(store, "sh", "-c", directory+"lint && cat "+directory+"rules.json && [ ! -x "+directory+"rules.json ] && echo modes")
	result, events, _ := runUnit(t, unit, testOptions(t))
	if result.Status != protocol.StatusPassed {
		t.Fatalf("status %s, fetch error %q", result.Status, fetchError(events))
	}
	said := ""
	for _, event := range events {
		if event.Type == "output" {
			said += event.Text + "\n"
		}
	}
	if said != "linted\n{}\nmodes\n" {
		t.Fatalf("the command said %q", said)
	}
	store.mutex.Lock()
	defer store.mutex.Unlock()
	if len(store.tokens) > 0 {
		t.Fatalf("the public store was sent %v; the run's token stays with the run", store.tokens)
	}
}

// Every way the store can fail to give a product whole finishes the unit broken, never failed, and the command never
// runs: a broken product says nothing about the change.
func TestAProductTheStoreCantGiveWholeBreaksTheUnitAndTheCommandNeverRuns(t *testing.T) {
	cases := map[string]struct {
		build func(store *productStore)
		says  string
	}{
		"never built": {func(store *productStore) {}, "not in the action store"},
		"an archive that isn't its hash": {func(store *productStore) {
			archive := store.build(productKey, map[string]string{cached + "lint": "real"}, nil)
			// Same length as the real bytes, so only the hash can tell them apart.
			store.objects["blobs/"+digest(archive)] = append(archive[:len(archive)-1:len(archive)-1], archive[len(archive)-1]^1)
		}, "poisoned"},
		"a corrupted archive under its own hash": {func(store *productStore) {
			archive := store.build(productKey, map[string]string{cached + "lint": "x"}, nil)
			store.put(productKey, archive[:len(archive)/2])
		}, "poisoned"},
		"a path out of the product": {func(store *productStore) {
			store.build(productKey, map[string]string{"../../escape": "x"}, nil)
		}, "poisoned"},
		"a path under no buildcache key": {func(store *productStore) {
			store.build(productKey, map[string]string{"lint": "x"}, nil)
		}, "poisoned"},
		"a ref that isn't a hash": {func(store *productStore) {
			store.objects["refs/action/"+productKey] = []byte("latest")
		}, "not an archive's sha256"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			store := newProductStore(t)
			test.build(store)
			// The marker sits outside the workspace, which the runner removes when the unit ends.
			ran := filepath.Join(t.TempDir(), "ran")
			result, events, _ := runUnit(t, productUnit(store, "touch", ran), testOptions(t))
			if result.Status != protocol.StatusBroken {
				t.Fatalf("status %s, not broken", result.Status)
			}
			if said := fetchError(events); !strings.Contains(said, test.says) {
				t.Fatalf("fetch error %q doesn't say %q", said, test.says)
			}
			if _, err := os.Stat(ran); err == nil {
				t.Fatalf("the command ran without its product")
			}
		})
	}
}

func TestAUnitsProductsAreChecked(t *testing.T) {
	good := func() protocol.Unit {
		unit := testUnit("true")
		unit.Products, unit.ProductStore = []protocol.Product{{Key: productKey, Directory: "p"}}, "https://store"
		return unit
	}
	if err := protocol.CheckUnit(good()); err != nil {
		t.Fatalf("the good unit is refused: %v", err)
	}
	cases := map[string]func(*protocol.Unit){
		"a key that isn't a hash":  func(u *protocol.Unit) { u.Products[0].Key = "lint" },
		"a directory out":          func(u *protocol.Unit) { u.Products[0].Directory = "../p" },
		"no directory":             func(u *protocol.Unit) { u.Products[0].Directory = "" },
		"no store":                 func(u *protocol.Unit) { u.ProductStore = "" },
		"a store that isn't a url": func(u *protocol.Unit) { u.ProductStore = "store" },
		"a test job": func(u *protocol.Unit) {
			u.Argv = nil
			u.Test = &protocol.TestJob{Repository: protocol.AdamicRepository, Sha: strings.Repeat("c", 40), Packages: []protocol.TestPackage{{Package: protocol.AdamicModule}}}
		},
	}
	for name, breakIt := range cases {
		unit := good()
		breakIt(&unit)
		if err := protocol.CheckUnit(unit); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
