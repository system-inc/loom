package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/system-inc/loom/protocol"
)

// productStore is the public action store as adamic-store.kirkouimet.com serves it: refs/action/<key> holds a
// manifest's sha256 as text, and blobs/<sha256> holds bytes. It notes every Authorization header it was sent.
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

// build puts a product's outputs, its manifest and its ref in the store, as a builder would. edit, when given, changes
// the manifest before it is stored, for the cases where a builder or a store lies.
func (store *productStore) build(key string, files map[string]string, executable map[string]bool, edit func(*actionManifest)) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	manifest := actionManifest{Key: key}
	for path, text := range files {
		store.objects["blobs/"+digest([]byte(text))] = []byte(text)
		manifest.Outputs = append(manifest.Outputs, actionOutput{Bytes: int64(len(text)), Executable: executable[path], Path: path, Sha256: digest([]byte(text))})
	}
	if edit != nil {
		edit(&manifest)
	}
	encoded, _ := json.Marshal(manifest)
	store.objects["blobs/"+digest(encoded)] = encoded
	store.objects["refs/action/"+key] = []byte(digest(encoded) + "\n")
}

var productKey = strings.Repeat("a", 64)

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

// Runners fetch and never build (contracts v1.1): a product's outputs land in its directory, each at its manifest
// path, executable where the manifest says, before the command runs; and the run's token never goes to the public store.
func TestAProductsOutputsLandInItsDirectoryBeforeTheCommand(t *testing.T) {
	store := newProductStore(t)
	store.build(productKey, map[string]string{"bin/lint": "#!/bin/sh\necho linted\n", "data/rules.json": "{}\n"}, map[string]bool{"bin/lint": true}, nil)
	unit := productUnit(store, "sh", "-c", "products/lint/bin/lint && cat products/lint/data/rules.json && [ ! -x products/lint/data/rules.json ] && echo modes")
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
		"never built": {func(store *productStore) {}, "never built"},
		"blob bytes that aren't their hash": {func(store *productStore) {
			store.build(productKey, map[string]string{"bin/lint": "real"}, nil, nil)
			// Same length as the real bytes, so only the hash can tell them apart.
			store.objects["blobs/"+digest([]byte("real"))] = []byte("fake")
		}, "refused"},
		"a manifest for another key": {func(store *productStore) {
			store.build(productKey, map[string]string{"bin/lint": "x"}, nil, func(manifest *actionManifest) { manifest.Key = strings.Repeat("b", 64) })
		}, "not this product"},
		"a path out of the product": {func(store *productStore) {
			store.build(productKey, map[string]string{"bin/lint": "x"}, nil, func(manifest *actionManifest) { manifest.Outputs[0].Path = "../../escape" })
		}, "inside the product"},
		"a size the manifest doesn't say": {func(store *productStore) {
			store.build(productKey, map[string]string{"bin/lint": "x"}, nil, func(manifest *actionManifest) { manifest.Outputs[0].Bytes = 2 })
		}, "the manifest says 2"},
		"a ref that isn't a hash": {func(store *productStore) {
			store.objects["refs/action/"+productKey] = []byte("latest")
		}, "not a manifest's sha256"},
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
