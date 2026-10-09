package builder

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeStore is the action store's rules from wire/source/Actions.ts, in memory: blobs by sha256 (checked), refs
// written once with their builder, a different second write a 409 naming both, a ref only to a held canonical
// manifest whose outputs are all held. Reads are /blobs/<sha256> and /refs/action/<key>; writes are /actions/... with
// "Bearer build:<builder>".
type fakeStore struct {
	mutex    sync.Mutex
	blobs    map[string][]byte
	refs     map[string]string
	builders map[string]string
	writes   int
}

func newFakeStore() *fakeStore {
	return &fakeStore{blobs: map[string][]byte{}, refs: map[string]string{}, builders: map[string]string{}}
}

func (store *fakeStore) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	path := request.URL.Path
	reply := func(status int, body any) {
		writer.WriteHeader(status)
		json.NewEncoder(writer).Encode(body)
	}
	switch {
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/blobs/"):
		content, held := store.blobs[strings.TrimPrefix(path, "/blobs/")]
		if !held {
			reply(404, map[string]string{"error": "no blob"})
			return
		}
		writer.Write(content)
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/refs/action/"):
		target, held := store.refs[strings.TrimPrefix(path, "/refs/action/")]
		if !held {
			reply(404, map[string]string{"error": "no ref"})
			return
		}
		io.WriteString(writer, target)
	case request.Method == http.MethodPut && strings.HasPrefix(path, "/actions/"):
		builder, ok := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer build:")
		if !ok {
			reply(403, map[string]string{"error": "a build token only"})
			return
		}
		body, _ := io.ReadAll(request.Body)
		store.writes++
		if sum, isBlob := strings.CutPrefix(path, "/actions/blobs/"); isBlob {
			actual := sha256.Sum256(body)
			if hex.EncodeToString(actual[:]) != sum {
				reply(400, map[string]string{"error": "the body doesn't hash"})
				return
			}
			store.blobs[sum] = body
			reply(201, map[string]any{"stored": true})
			return
		}
		key := strings.TrimPrefix(path, "/actions/")
		target := strings.TrimSpace(string(body))
		content, held := store.blobs[target]
		if !held {
			reply(409, map[string]string{"error": "manifest not held"})
			return
		}
		var manifest Manifest
		if json.Unmarshal(content, &manifest) != nil || manifest.Key != key {
			reply(400, map[string]string{"error": "not this key's manifest"})
			return
		}
		if canonical, _ := manifest.Canonical(); !bytes.Equal(canonical, content) {
			reply(400, map[string]string{"error": "not canonical"})
			return
		}
		for _, output := range manifest.Outputs {
			if _, held := store.blobs[output.Sha256]; !held {
				reply(409, map[string]any{"error": "outputs first", "missing": []string{output.Path}})
				return
			}
		}
		if held, exists := store.refs[key]; exists {
			if held == target {
				reply(200, map[string]any{"created": false, "builder": store.builders[key]})
				return
			}
			reply(409, map[string]string{
				"error": key + ": " + store.builders[key] + " built " + held + " and " + builder + " built " + target,
				"held":  held, "heldBuilder": store.builders[key], "builder": builder, "sha256": target,
			})
			return
		}
		store.refs[key], store.builders[key] = target, builder
		reply(201, map[string]any{"created": true})
	default:
		reply(405, map[string]string{"error": "no"})
	}
}

func serve(t *testing.T, store *fakeStore, builder string) Store {
	server := httptest.NewServer(store)
	t.Cleanup(server.Close)
	return Store{Read: server.URL, Write: server.URL + "/actions", Token: "build:" + builder}
}

func keyOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// fakeRun writes what a product test's buildcache would: one product directory per name under the cache, an
// executable and a data file in each, and buildcache's .inputs file and lock beside it.
func fakeRun(products map[string]map[string]string, runs *int) func(Action, []string) ([]byte, error) {
	return func(action Action, environment []string) ([]byte, error) {
		*runs++
		cache := ""
		for _, entry := range environment {
			if value, ok := strings.CutPrefix(entry, "ADAMIC_BUILD_CACHE_DIR="); ok {
				cache = value
			}
		}
		for product, files := range products {
			for name, content := range files {
				file := filepath.Join(cache, product, name)
				os.MkdirAll(filepath.Dir(file), 0o755)
				mode := os.FileMode(0o644)
				if strings.HasPrefix(name, "bin/") {
					mode = 0o755
				}
				os.WriteFile(file, []byte(content), mode)
			}
			os.WriteFile(filepath.Join(cache, product+".inputs"), []byte("inputs of "+product), 0o644)
			os.WriteFile(filepath.Join(cache, product+".lock"), nil, 0o644)
			os.MkdirAll(filepath.Join(cache, ".building-"+product[:12]+"-x"), 0o755)
		}
		return []byte("ok"), nil
	}
}

func TestWorkshopBuildsAMissingActionOnceAndARunnerFetchesItIntoItsCache(t *testing.T) {
	store := newFakeStore()
	oracle, stage0 := keyOf("oracle"), keyOf("stage0")
	products := map[string]map[string]string{
		oracle: {"bin/oracle": "an oracle binary", "data/table.json": "{}"},
		stage0: {"stage0.a": "an archive"},
	}
	runs := 0
	action := Action{Package: "github.com/system-inc/adamic/bridge/tsgo", Directory: "bridge/tsgo", Test: "TestProduct_HealthyRegion"}
	productKey := keyOf("productKey of the healthy region")
	builder := Builder{
		Store: serve(t, store, "workshop"), Scratch: t.TempDir(), Run: fakeRun(products, &runs),
		Key: func(Action) (string, error) { return productKey, nil },
	}
	results := builder.Build([]Action{action})
	if len(results) != 1 || results[0].Outcome != "built" || results[0].Key != productKey || results[0].Error != "" {
		t.Fatalf("first build: %+v", results)
	}
	// Three files in two products and their two .inputs files; no lock and no unfinished directory.
	if results[0].Outputs != 5 || runs != 1 {
		t.Fatalf("outputs %d, runs %d", results[0].Outputs, runs)
	}
	// Built once: the same action again, on any builder, builds nothing.
	again := Builder{Store: serve(t, store, "home"), Scratch: t.TempDir(), Run: fakeRun(products, &runs), Key: builder.Key}
	if results = again.Build([]Action{action}); results[0].Outcome != "stored" || runs != 1 {
		t.Fatalf("second build: %+v, runs %d", results, runs)
	}

	// The runner fetches into its cache directory, and buildcache would find each product there as a hit.
	runner := Store{Read: builder.Store.Read}
	cache := t.TempDir()
	if err := runner.Fetch(productKey, cache); err != nil {
		t.Fatal(err)
	}
	for product, files := range products {
		for name, content := range files {
			read, err := os.ReadFile(filepath.Join(cache, product, name))
			if err != nil || string(read) != content {
				t.Fatalf("%s/%s: %q %v", product, name, read, err)
			}
		}
		if inputs, err := os.ReadFile(filepath.Join(cache, product+".inputs")); err != nil || string(inputs) != "inputs of "+product {
			t.Fatalf("%s.inputs: %q %v", product, inputs, err)
		}
	}
	if info, err := os.Stat(filepath.Join(cache, oracle, "bin/oracle")); err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("the oracle binary isn't executable: %v %v", info.Mode(), err)
	}
	if info, err := os.Stat(filepath.Join(cache, oracle, "data/table.json")); err != nil || info.Mode()&0o111 != 0 {
		t.Fatalf("a data file came back executable: %v %v", info.Mode(), err)
	}
	entries, _ := os.ReadDir(cache)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("a fetch left %s behind", entry.Name())
		}
	}
	// A product directory already in the cache is left as it is.
	os.WriteFile(filepath.Join(cache, stage0, "stage0.a"), []byte("local"), 0o644)
	if err := runner.Fetch(productKey, cache); err != nil {
		t.Fatal(err)
	}
	if read, _ := os.ReadFile(filepath.Join(cache, stage0, "stage0.a")); string(read) != "local" {
		t.Fatalf("a fetch replaced a product already in the cache")
	}
	if err := runner.Fetch(keyOf("never built"), t.TempDir()); !errors.Is(err, ErrNotStored) {
		t.Fatalf("a missing action: %v", err)
	}
}

func TestASecondBuilderThatBuildsOtherBytesFailsNamingBothBuilders(t *testing.T) {
	store := newFakeStore()
	productKey := keyOf("productKey")
	product := keyOf("product")
	runs := 0
	workshop := Builder{
		Store: serve(t, store, "workshop"), Scratch: t.TempDir(), Key: func(Action) (string, error) { return productKey, nil },
		Run: fakeRun(map[string]map[string]string{product: {"bin/tool": "built on workshop"}}, &runs),
	}
	action := Action{Directory: "x", Test: "TestProduct_X"}
	if results := workshop.Build([]Action{action}); results[0].Outcome != "built" {
		t.Fatalf("%+v", results)
	}
	// The audit: a second builder rebuilds the same key. It writes past the store check, as an audit must.
	home := Store{Read: workshop.Store.Read, Write: workshop.Store.Write, Token: "build:home"}
	directory := t.TempDir()
	os.MkdirAll(filepath.Join(directory, product, "bin"), 0o755)
	os.WriteFile(filepath.Join(directory, product, "bin/tool"), []byte("built on home, differently"), 0o755)
	outputs, files, err := Outputs(directory)
	if err != nil {
		t.Fatal(err)
	}
	_, err = home.Upload(productKey, outputs, files)
	var conflict ConflictError
	if !errors.As(err, &conflict) || conflict.HeldBuilder != "workshop" || conflict.Builder != "home" {
		t.Fatalf("a different second build: %v", err)
	}
	if !strings.Contains(conflict.Error(), "workshop built") || !strings.Contains(conflict.Error(), "home built") {
		t.Fatalf("the conflict doesn't name both builders: %v", conflict)
	}
	// The same bytes from the second builder is no conflict.
	same := t.TempDir()
	os.MkdirAll(filepath.Join(same, product, "bin"), 0o755)
	os.WriteFile(filepath.Join(same, product, "bin/tool"), []byte("built on workshop"), 0o755)
	os.WriteFile(filepath.Join(same, product+".inputs"), []byte("inputs of "+product), 0o644)
	outputs, files, _ = Outputs(same)
	if _, err = home.Upload(productKey, outputs, files); err != nil {
		t.Fatalf("an identical second build: %v", err)
	}
}

func TestAFailedOrEmptyProductTestUploadsNothing(t *testing.T) {
	store := newFakeStore()
	builder := Builder{
		Store: serve(t, store, "workshop"), Scratch: t.TempDir(), Key: func(Action) (string, error) { return keyOf("k"), nil },
		Run: func(Action, []string) ([]byte, error) { return []byte("--- FAIL"), errors.New("exit status 1") },
	}
	if results := builder.Build([]Action{{Directory: "x", Test: "TestProduct_X"}}); results[0].Outcome != "failed" || !strings.Contains(results[0].Error, "FAIL") {
		t.Fatalf("%+v", results)
	}
	builder.Run = func(Action, []string) ([]byte, error) { return nil, nil }
	if results := builder.Build([]Action{{Directory: "x", Test: "TestProduct_X"}}); results[0].Outcome != "failed" || !strings.Contains(results[0].Error, "no product") {
		t.Fatalf("%+v", results)
	}
	if store.writes != 0 {
		t.Fatalf("a failed build wrote %d times", store.writes)
	}
}

func TestAFetchRefusesAPoisonedStore(t *testing.T) {
	product := keyOf("product")
	// built stores one honest action and returns its store, key and manifest.
	built := func(t *testing.T) (*fakeStore, Store, string, Manifest) {
		store := newFakeStore()
		productKey := keyOf("productKey")
		runs := 0
		builder := Builder{
			Store: serve(t, store, "workshop"), Scratch: t.TempDir(), Key: func(Action) (string, error) { return productKey, nil },
			Run: fakeRun(map[string]map[string]string{product: {"bin/tool": "honest"}}, &runs),
		}
		results := builder.Build([]Action{{Directory: "x", Test: "TestProduct_X"}})
		var held Manifest
		json.Unmarshal(store.blobs[results[0].Manifest], &held)
		return store, Store{Read: builder.Store.Read}, productKey, held
	}
	refused := func(t *testing.T, runner Store, key string) {
		t.Helper()
		cache := t.TempDir()
		if err := runner.Fetch(key, cache); err == nil || !strings.Contains(err.Error(), "poisoned") {
			t.Fatalf("a poisoned store: %v", err)
		}
		if entries, _ := os.ReadDir(cache); len(entries) != 0 {
			t.Fatalf("a poisoned fetch left %s in the cache", entries[0].Name())
		}
	}
	t.Run("a blob that doesn't hash to its name", func(t *testing.T) {
		store, runner, key, held := built(t)
		// Same size, other bytes: only the hash can tell. Outputs sort the .inputs file first, so poison the last.
		last := held.Outputs[len(held.Outputs)-1]
		store.blobs[last.Sha256] = []byte(strings.Repeat("x", int(last.Bytes)))
		refused(t, runner, key)
	})
	t.Run("a manifest whose size is wrong for an honest blob", func(t *testing.T) {
		store, runner, key, held := built(t)
		held.Outputs[len(held.Outputs)-1].Bytes++
		canonical, _ := held.Canonical()
		sum := sha256.Sum256(canonical)
		store.blobs[hex.EncodeToString(sum[:])] = canonical
		store.refs[key] = hex.EncodeToString(sum[:])
		refused(t, runner, key)
	})
	t.Run("a ref naming another key's manifest", func(t *testing.T) {
		store, runner, key, _ := built(t)
		other := keyOf("other")
		store.refs[other] = store.refs[key]
		refused(t, runner, other)
	})
}

func TestTheCanonicalManifestMatchesTheStores(t *testing.T) {
	// The bytes Actions.ts's canonicalManifest writes for the same manifest (test/pipeline/Actions.test.ts).
	manifest := Manifest{Key: "k", Outputs: []Output{
		{Path: "b/two", Sha256: "2", Bytes: 2, Executable: true},
		{Path: "a/<one>&", Sha256: "1", Bytes: 1},
	}}
	canonical, err := manifest.Canonical()
	want := `{"key":"k","outputs":[{"bytes":1,"executable":false,"path":"a/<one>&","sha256":"1"},{"bytes":2,"executable":true,"path":"b/two","sha256":"2"}]}`
	if err != nil || string(canonical) != want {
		t.Fatalf("canonical %s (%v)", canonical, err)
	}
}

func TestGoTestRunsOneProductTestInTheTreeWithTheBuildersEnvironment(t *testing.T) {
	tree := t.TempDir()
	os.WriteFile(filepath.Join(tree, "go.mod"), []byte("module example.com/products\n\ngo 1.22\n"), 0o644)
	os.MkdirAll(filepath.Join(tree, "pkg"), 0o755)
	os.WriteFile(filepath.Join(tree, "pkg", "product_test.go"), []byte(`package pkg

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProduct_Tool(t *testing.T) {
	if os.Getenv("ADAMIC_BUILD_STORE") != "off" {
		t.Fatal("floor1 must be off for a builder")
	}
	directory := filepath.Join(os.Getenv("ADAMIC_BUILD_CACHE_DIR"), "0123456789abcdef")
	os.MkdirAll(directory, 0o755)
	os.WriteFile(filepath.Join(directory, "tool"), []byte("#!/bin/sh\n"), 0o755)
}

func TestProduct_Other(t *testing.T) { t.Fatal("only the asked-for product test runs") }
`), 0o644)
	store := newFakeStore()
	builder := Builder{
		Tree: tree, Store: serve(t, store, "workshop"), Scratch: t.TempDir(), Run: GoTest(tree),
		Key: func(Action) (string, error) { return keyOf("tool"), nil },
	}
	results := builder.Build([]Action{{Package: "example.com/products/pkg", Directory: "pkg", Test: "TestProduct_Tool"}})
	if results[0].Outcome != "built" || results[0].Outputs != 1 {
		t.Fatalf("%+v", results)
	}
}
