package builder

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/system-inc/loom/r2/r2test"
)

// serve is a builder's store on a fresh fake bucket: writes signed through its S3 interface, reads from its public
// domain.
func serve(t *testing.T) (*r2test.Fake, Store) {
	fake := r2test.New(t)
	bucket := fake.Bucket()
	return fake, Store{Read: fake.Public(), Bucket: &bucket}
}

func keyOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// fakeRun does what a product test's buildcache would: for each product, a hit when its directory is already in the
// cache, otherwise a build (an executable and a data file, buildcache's .inputs file and lock beside it), and a
// record line in ADAMIC_BUILD_LOG either way.
func fakeRun(products map[string]map[string]string, runs *int) func(Action, []string) ([]byte, error) {
	var mutex sync.Mutex
	return func(action Action, environment []string) ([]byte, error) {
		mutex.Lock()
		defer mutex.Unlock()
		*runs++
		cache, log := "", ""
		for _, entry := range environment {
			if value, ok := strings.CutPrefix(entry, "ADAMIC_BUILD_CACHE_DIR="); ok {
				cache = value
			}
			if value, ok := strings.CutPrefix(entry, "ADAMIC_BUILD_LOG="); ok {
				log = value
			}
		}
		lines := ""
		for product, files := range products {
			outcome := "hit"
			if _, err := os.Stat(filepath.Join(cache, product)); err != nil {
				outcome = "miss"
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
			lines += "build product_" + product[:4] + " " + product[:12] + " " + outcome + " 1.00\n"
		}
		os.WriteFile(log, []byte("census line, not a build\n"+lines), 0o644)
		return []byte("ok"), nil
	}
}

func TestWorkshopBuildsAMissingActionOnceAndARunnerFetchesItIntoItsCache(t *testing.T) {
	fake, store := serve(t)
	oracle, stage0 := keyOf("oracle"), keyOf("stage0")
	products := map[string]map[string]string{
		oracle: {"bin/oracle": "an oracle binary", "data/table.json": "{}"},
		stage0: {"stage0.a": "an archive"},
	}
	runs := 0
	action := Action{Package: "github.com/system-inc/adamic/bridge/tsgo", Directory: "bridge/tsgo", Test: "TestProduct_HealthyRegion"}
	productKey := keyOf("productKey of the healthy region")
	builder := Builder{
		Store: store, Scratch: t.TempDir(), Cache: t.TempDir(), Run: fakeRun(products, &runs),
		Key: func(Action) (string, error) { return productKey, nil },
	}
	results := builder.Build([]Action{action})
	if len(results) != 1 || results[0].Outcome != "built" || results[0].Key != productKey || results[0].Error != "" {
		t.Fatalf("first build: %+v", results)
	}
	// Three files in two products and their two .inputs files, one archive; no lock and no unfinished directory.
	if results[0].Outputs != 5 || runs != 1 {
		t.Fatalf("outputs %d, runs %d", results[0].Outputs, runs)
	}
	// One blob and one ref, the ref naming the blob.
	if blobs, refs := fake.Keys("blobs/"), fake.Keys("refs/"); len(blobs) != 1 || len(refs) != 1 || blobs[0] != "blobs/"+results[0].Archive {
		t.Fatalf("the store holds %v and %v", blobs, refs)
	}
	if ref, _ := fake.Object("refs/action/" + productKey); string(ref) != results[0].Archive {
		t.Fatalf("the ref holds %q", ref)
	}
	// Built once: the same action again, from another cache, builds nothing.
	again := Builder{Store: store, Scratch: t.TempDir(), Cache: t.TempDir(), Run: fakeRun(products, &runs), Key: builder.Key}
	if results = again.Build([]Action{action}); results[0].Outcome != "stored" || runs != 1 {
		t.Fatalf("second build: %+v, runs %d", results, runs)
	}

	// The runner fetches into its cache directory, and buildcache would find each product there as a hit.
	runner := Store{Read: fake.Public()}
	cache := t.TempDir()
	if err := runner.FetchProduct(productKey, cache); err != nil {
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
	if err := runner.FetchProduct(productKey, cache); err != nil {
		t.Fatal(err)
	}
	if read, _ := os.ReadFile(filepath.Join(cache, stage0, "stage0.a")); string(read) != "local" {
		t.Fatalf("a fetch replaced a product already in the cache")
	}
	if err := runner.FetchProduct(keyOf("never built"), t.TempDir()); !errors.Is(err, ErrNotStored) {
		t.Fatalf("a missing action: %v", err)
	}
}

func TestAFailedProductTestUploadsNothingAndAnEmptyOneIsStoredAsEmpty(t *testing.T) {
	fake, store := serve(t)
	builder := Builder{
		Store: store, Scratch: t.TempDir(), Cache: t.TempDir(), Key: func(Action) (string, error) { return keyOf("k"), nil },
		Run: func(Action, []string) ([]byte, error) { return []byte("--- FAIL"), errors.New("exit status 1") },
	}
	if results := builder.Build([]Action{{Directory: "x", Test: "TestProduct_X"}}); results[0].Outcome != "failed" || !strings.Contains(results[0].Error, "FAIL") {
		t.Fatalf("%+v", results)
	}
	if fake.Count("PUT", "") != 0 {
		t.Fatalf("a failed build wrote %v", fake.Requests())
	}
	// A product test that passed and used no buildcache product is stored as an empty archive, and not run again.
	runs := 0
	builder.Run = func(Action, []string) ([]byte, error) { runs++; return nil, nil }
	if results := builder.Build([]Action{{Directory: "x", Test: "TestProduct_X"}}); results[0].Outcome != "built" || results[0].Outputs != 0 {
		t.Fatalf("%+v", results)
	}
	if results := builder.Build([]Action{{Directory: "x", Test: "TestProduct_X"}}); results[0].Outcome != "stored" || runs != 1 {
		t.Fatalf("%+v, runs %d", results, runs)
	}
	if err := (Store{Read: fake.Public()}).FetchProduct(keyOf("k"), t.TempDir()); err != nil {
		t.Fatalf("an empty action: %v", err)
	}
}

func TestAnUpstreamSharedByTwoActionsBuildsOnceAndGoesUpWithEach(t *testing.T) {
	fake, store := serve(t)
	stage0, oracleA, oracleB := keyOf("stage0"), keyOf("oracle a"), keyOf("oracle b")
	builds := map[string]int{}
	var mutex sync.Mutex
	cache := t.TempDir()
	// Each action needs stage0 and its own oracle; stage0 is built by whichever runs first.
	run := func(action Action, environment []string) ([]byte, error) {
		mutex.Lock()
		defer mutex.Unlock()
		log := ""
		for _, entry := range environment {
			if value, ok := strings.CutPrefix(entry, "ADAMIC_BUILD_LOG="); ok {
				log = value
			}
		}
		own := map[string]string{"TestProduct_A": oracleA, "TestProduct_B": oracleB}[action.Test]
		lines := ""
		for _, product := range []string{stage0, own} {
			outcome := "hit"
			if _, err := os.Stat(filepath.Join(cache, product)); err != nil {
				outcome = "miss"
				builds[product]++
				os.MkdirAll(filepath.Join(cache, product), 0o755)
				os.WriteFile(filepath.Join(cache, product, "out"), []byte(product), 0o644)
			}
			lines += "build p " + product[:12] + " " + outcome + " 1.00\n"
		}
		os.WriteFile(log, []byte(lines), 0o644)
		return nil, nil
	}
	keys := map[string]string{"TestProduct_A": keyOf("A"), "TestProduct_B": keyOf("B")}
	reported := []string{}
	builder := Builder{
		Store: store, Scratch: t.TempDir(), Cache: cache, Jobs: 2, Run: run,
		Key:    func(action Action) (string, error) { return keys[action.Test], nil },
		Report: func(result Result) { reported = append(reported, result.Action.Test) },
	}
	results := builder.Build([]Action{{Directory: "x", Test: "TestProduct_A"}, {Directory: "x", Test: "TestProduct_B"}})
	if len(reported) != 2 {
		t.Fatalf("reported %v", reported)
	}
	for _, result := range results {
		if result.Outcome != "built" || result.Products != 2 {
			t.Fatalf("%+v", result)
		}
	}
	if builds[stage0] != 1 || builds[oracleA] != 1 || builds[oracleB] != 1 {
		t.Fatalf("builds %v", builds)
	}
	// Each action's archive carries stage0, so a runner fetching only B still gets it.
	runnerCache := t.TempDir()
	if err := (Store{Read: fake.Public()}).FetchProduct(keys["TestProduct_B"], runnerCache); err != nil {
		t.Fatal(err)
	}
	for _, product := range []string{stage0, oracleB} {
		if _, err := os.Stat(filepath.Join(runnerCache, product, "out")); err != nil {
			t.Fatalf("B's fetch lacks %s: %v", product[:12], err)
		}
	}
	if _, err := os.Stat(filepath.Join(runnerCache, oracleA)); err == nil {
		t.Fatal("B's fetch brought A's own product")
	}
}

func TestTouchedRefusesAPrefixThatNamesNoProductOrTwo(t *testing.T) {
	cache, logs := t.TempDir(), t.TempDir()
	one := "aaaaaaaaaaaa" + strings.Repeat("1", 52)
	two := "aaaaaaaaaaaa" + strings.Repeat("2", 52)
	os.MkdirAll(filepath.Join(cache, one), 0o755)
	log := filepath.Join(logs, "builds.log")
	os.WriteFile(log, []byte("build p aaaaaaaaaaaa hit 0.01\n"), 0o644)
	if touched, err := Touched(log, cache); err != nil || len(touched) != 1 || touched[0] != one {
		t.Fatalf("one product: %v %v", touched, err)
	}
	os.MkdirAll(filepath.Join(cache, two), 0o755)
	if _, err := Touched(log, cache); err == nil {
		t.Fatal("an ambiguous prefix was resolved")
	}
	os.WriteFile(log, []byte("build p bbbbbbbbbbbb miss 0.01\n"), 0o644)
	if _, err := Touched(log, cache); err == nil {
		t.Fatal("a prefix with no product was resolved")
	}
	// Uncached builds and other lines name no product in the cache.
	os.WriteFile(log, []byte("build p uncached off 0.01\nstore p aaaaaaaaaaaa: not in the store\n"), 0o644)
	if touched, err := Touched(log, cache); err != nil || len(touched) != 0 {
		t.Fatalf("no build lines: %v %v", touched, err)
	}
}

func TestAFetchRefusesAPoisonedStore(t *testing.T) {
	product := keyOf("product")
	// built stores one honest action and returns the fake bucket, the action's key and its archive's sha256.
	built := func(t *testing.T) (*r2test.Fake, Store, string, string) {
		fake, store := serve(t)
		productKey := keyOf("productKey")
		runs := 0
		builder := Builder{
			Store: store, Scratch: t.TempDir(), Cache: t.TempDir(), Key: func(Action) (string, error) { return productKey, nil },
			Run: fakeRun(map[string]map[string]string{product: {"bin/tool": "honest"}}, &runs),
		}
		results := builder.Build([]Action{{Directory: "x", Test: "TestProduct_X"}})
		return fake, Store{Read: fake.Public()}, productKey, results[0].Archive
	}
	refused := func(t *testing.T, runner Store, key, says string) {
		t.Helper()
		cache := t.TempDir()
		if err := runner.FetchProduct(key, cache); err == nil || !strings.Contains(err.Error(), says) {
			t.Fatalf("a poisoned store: %v", err)
		}
		if entries, _ := os.ReadDir(cache); len(entries) != 0 {
			t.Fatalf("a poisoned fetch left %s in the cache", entries[0].Name())
		}
	}
	// plant puts an archive in the bucket under its own hash and points the action's ref at it.
	plant := func(fake *r2test.Fake, key string, archive []byte) {
		fake.Set("blobs/"+digest(archive), archive, fake.Modified("refs/action/"+key))
		fake.Set("refs/action/"+key, []byte(digest(archive)), fake.Modified("refs/action/"+key))
	}
	t.Run("an archive that doesn't hash to its name", func(t *testing.T) {
		fake, runner, key, archive := built(t)
		held, _ := fake.Object("blobs/" + archive)
		fake.Set("blobs/"+archive, append(held[:len(held)-1:len(held)-1], held[len(held)-1]^1), fake.Modified("blobs/"+archive))
		refused(t, runner, key, "poisoned")
	})
	t.Run("a corrupted archive under its own hash", func(t *testing.T) {
		fake, runner, key, archive := built(t)
		held, _ := fake.Object("blobs/" + archive)
		plant(fake, key, held[:len(held)/2])
		refused(t, runner, key, "poisoned")
	})
	t.Run("an entry outside the cache", func(t *testing.T) {
		fake, runner, key, _ := built(t)
		plant(fake, key, tarGzip(t, entry{name: product + "/../../escaped", body: "x"}))
		refused(t, runner, key, "poisoned")
	})
	t.Run("an entry under no buildcache key", func(t *testing.T) {
		fake, runner, key, _ := built(t)
		plant(fake, key, tarGzip(t, entry{name: "loose-file", body: "x"}))
		refused(t, runner, key, "poisoned")
	})
	t.Run("a link out of the cache", func(t *testing.T) {
		fake, runner, key, _ := built(t)
		plant(fake, key, tarGzip(t, entry{name: product + "/tool", link: "../../../../etc/passwd"}))
		refused(t, runner, key, "poisoned")
	})
	t.Run("a ref that isn't a sha256", func(t *testing.T) {
		fake, runner, key, _ := built(t)
		fake.Set("refs/action/"+key, []byte("latest"), fake.Modified("refs/action/"+key))
		refused(t, runner, key, "poisoned")
	})
}

func TestAFetchCanLeaveOutWhatClangBuilt(t *testing.T) {
	fake, store := serve(t)
	goProduct, nativeProduct := keyOf("go oracle"), keyOf("native release")
	cache := t.TempDir()
	productKey := keyOf("productKey")
	run := func(action Action, environment []string) ([]byte, error) {
		log := ""
		for _, entry := range environment {
			if value, ok := strings.CutPrefix(entry, "ADAMIC_BUILD_LOG="); ok {
				log = value
			}
		}
		for product, tool := range map[string]string{goProduct: "tool go version: go version go1.27.1 linux/amd64", nativeProduct: "tool clang --version: clang version 20.1.8"} {
			os.MkdirAll(filepath.Join(cache, product), 0o755)
			os.WriteFile(filepath.Join(cache, product, "out"), []byte(product), 0o755)
			os.WriteFile(filepath.Join(cache, product+".inputs"), []byte("name "+product[:4]+"\nfile a.go\n"+tool+"\n"), 0o644)
		}
		os.WriteFile(log, []byte("build g "+goProduct[:12]+" miss 1.00\nbuild n "+nativeProduct[:12]+" miss 1.00\n"), 0o644)
		return nil, nil
	}
	builder := Builder{Store: store, Scratch: t.TempDir(), Cache: cache, Run: run, Key: func(Action) (string, error) { return productKey, nil }}
	if results := builder.Build([]Action{{Directory: "x", Test: "TestProduct_X"}}); results[0].Outcome != "built" || results[0].Products != 2 {
		t.Fatalf("%+v", results)
	}
	runner := t.TempDir()
	if err := (Store{Read: fake.Public(), SkipNative: true}).FetchProduct(productKey, runner); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(runner, goProduct, "out")); err != nil {
		t.Fatalf("the Go product wasn't fetched: %v", err)
	}
	for _, left := range []string{nativeProduct, nativeProduct + ".inputs"} {
		if _, err := os.Stat(filepath.Join(runner, left)); err == nil {
			t.Fatalf("a native product was fetched: %s", left)
		}
	}
	everything := t.TempDir()
	if err := (Store{Read: fake.Public()}).FetchProduct(productKey, everything); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(everything, nativeProduct, "out")); err != nil {
		t.Fatalf("without SkipNative the native product wasn't fetched: %v", err)
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
	key := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	directory := filepath.Join(os.Getenv("ADAMIC_BUILD_CACHE_DIR"), key)
	os.MkdirAll(directory, 0o755)
	os.WriteFile(filepath.Join(directory, "tool"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(os.Getenv("ADAMIC_BUILD_LOG"), []byte("build tool "+key[:12]+" miss 0.10\n"), 0o644)
}

func TestProduct_Other(t *testing.T) { t.Fatal("only the asked-for product test runs") }
`), 0o644)
	_, store := serve(t)
	builder := Builder{
		Tree: tree, Store: store, Scratch: t.TempDir(), Cache: t.TempDir(), Run: GoTest(tree),
		Key: func(Action) (string, error) { return keyOf("tool"), nil },
	}
	results := builder.Build([]Action{{Package: "example.com/products/pkg", Directory: "pkg", Test: "TestProduct_Tool"}})
	if results[0].Outcome != "built" || results[0].Outputs != 1 {
		t.Fatalf("%+v", results)
	}
}
