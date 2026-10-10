package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheIndexDecidesWithNoReadsAndSendsEachBlobOnce(t *testing.T) {
	store := newFakeStore()
	stage0, oracleA, oracleB := keyOf("stage0"), keyOf("oracle a"), keyOf("oracle b")
	cache, indexDirectory := t.TempDir(), t.TempDir()
	runs := 0
	run := func(action Action, environment []string) ([]byte, error) {
		runs++
		log := ""
		for _, entry := range environment {
			if value, ok := strings.CutPrefix(entry, "ADAMIC_BUILD_LOG="); ok {
				log = value
			}
		}
		own := map[string]string{"TestProduct_A": oracleA, "TestProduct_B": oracleB}[action.Test]
		lines := ""
		for _, product := range []string{stage0, own} {
			os.MkdirAll(filepath.Join(cache, product), 0o755)
			os.WriteFile(filepath.Join(cache, product, "out"), []byte(product), 0o644)
			lines += "build p " + product[:12] + " hit 1.00\n"
		}
		os.WriteFile(log, []byte(lines), 0o644)
		return nil, nil
	}
	keys := map[string]string{"TestProduct_A": keyOf("A"), "TestProduct_B": keyOf("B")}
	actions := []Action{{Directory: "x", Test: "TestProduct_A"}, {Directory: "x", Test: "TestProduct_B"}}
	build := func() (Store, []Result) {
		index, err := OpenIndex(indexDirectory)
		if err != nil {
			t.Fatal(err)
		}
		defer index.Close()
		served := serve(t, store, "workshop")
		served.Requests = &Requests{}
		builder := Builder{Store: served, Scratch: t.TempDir(), Cache: cache, Run: run, Index: index,
			Key: func(action Action) (string, error) { return keys[action.Test], nil }}
		return served, builder.Build(actions)
	}

	first, results := build()
	for _, result := range results {
		if result.Outcome != "built" {
			t.Fatalf("%+v", result)
		}
	}
	// The first build asks the store once per action (an empty index knows nothing), and sends stage0's blob once:
	// A's three blobs (stage0, its oracle, its manifest) and ref, then B's oracle, manifest and ref.
	if reads, writes := first.Requests.Reads.Load(), first.Requests.Writes.Load(); reads != 2 || writes != 7 {
		t.Fatalf("first build: %d reads, %d writes", reads, writes)
	}
	if store.writes != 7 {
		t.Fatalf("the store saw %d writes", store.writes)
	}

	// The same actions again: decided from the index alone, nothing read, nothing written, nothing run.
	second, results := build()
	for _, result := range results {
		if result.Outcome != "stored" || result.Manifest == "" {
			t.Fatalf("%+v", result)
		}
	}
	if reads, writes := second.Requests.Reads.Load(), second.Requests.Writes.Load(); reads != 0 || writes != 0 || runs != 2 {
		t.Fatalf("second build: %d reads, %d writes, %d runs", reads, writes, runs)
	}
}

func TestAnEmptyIndexSeedsItselfFromTheStoreOnce(t *testing.T) {
	store := newFakeStore()
	product := keyOf("product")
	productKey := keyOf("productKey")
	runs := 0
	// Built before the index existed, with no index.
	without := Builder{Store: serve(t, store, "workshop"), Scratch: t.TempDir(), Cache: t.TempDir(),
		Run: fakeRun(map[string]map[string]string{product: {"bin/tool": "t"}}, &runs), Key: func(Action) (string, error) { return productKey, nil }}
	without.Build([]Action{{Directory: "x", Test: "TestProduct_X"}})
	indexDirectory := t.TempDir()
	for pass, wantReads := range []int64{2, 0} {
		index, err := OpenIndex(indexDirectory)
		if err != nil {
			t.Fatal(err)
		}
		served := serve(t, store, "workshop")
		served.Requests = &Requests{}
		builder := without
		builder.Store, builder.Index = served, index
		results := builder.Build([]Action{{Directory: "x", Test: "TestProduct_X"}})
		index.Close()
		// The seeding pass reads the ref and its manifest; the next reads nothing.
		if results[0].Outcome != "stored" || served.Requests.Reads.Load() != wantReads || runs != 1 {
			t.Fatalf("pass %d: %+v, %d reads, %d runs", pass, results, served.Requests.Reads.Load(), runs)
		}
	}
	index, _ := OpenIndex(indexDirectory)
	defer index.Close()
	// The ref, the manifest, the tool and the .inputs file.
	if refs, blobs := index.Counts(); refs != 1 || blobs != 3 {
		t.Fatalf("the seeded index holds %d refs and %d blobs", refs, blobs)
	}
}

func TestTheIndexIsOneBuildersAndRefusesWhatItCantRead(t *testing.T) {
	directory := t.TempDir()
	index, err := OpenIndex(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = OpenIndex(directory); err == nil || !strings.Contains(err.Error(), "another builder") {
		t.Fatalf("a second builder opened the index: %v", err)
	}
	key, manifest := keyOf("key"), keyOf("manifest")
	if err = index.AddRef(key, manifest); err != nil {
		t.Fatal(err)
	}
	if err = index.AddRef(key, keyOf("other")); err == nil {
		t.Fatal("the index took a second manifest for one key")
	}
	index.Close()
	reopened, err := OpenIndex(directory)
	if err != nil {
		t.Fatal(err)
	}
	if held, known := reopened.Ref(key); !known || held != manifest {
		t.Fatalf("after reopening: %q %v", held, known)
	}
	reopened.Close()
	os.WriteFile(filepath.Join(directory, "index"), []byte("ref "+key+" not-a-sha\n"), 0o644)
	if _, err = OpenIndex(directory); err == nil {
		t.Fatal("an index with a bad line opened")
	}
}

func TestATrustedIndexDecidesAMissWithNoReadAndTheStoreStillCatchesADifferentBuild(t *testing.T) {
	store := newFakeStore()
	product := keyOf("product")
	productKey := keyOf("productKey")
	runs := 0
	// Stored before this index knew it.
	honest := map[string]map[string]string{product: {"bin/tool": "built once"}}
	Builder{Store: serve(t, store, "workshop"), Scratch: t.TempDir(), Cache: t.TempDir(), Run: fakeRun(honest, &runs),
		Key: func(Action) (string, error) { return productKey, nil }}.Build([]Action{{Directory: "x", Test: "TestProduct_X"}})
	trusted := func(products map[string]map[string]string) (Result, *Requests) {
		index, err := OpenIndex(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer index.Close()
		served := serve(t, store, "workshop")
		served.Requests = &Requests{}
		results := Builder{Store: served, Scratch: t.TempDir(), Cache: t.TempDir(), Run: fakeRun(products, &runs), Index: index, TrustIndex: true,
			Key: func(Action) (string, error) { return productKey, nil }}.Build([]Action{{Directory: "x", Test: "TestProduct_X"}})
		return results[0], served.Requests
	}
	// The same bytes: built again with no read, and the store answers 200.
	result, requests := trusted(honest)
	if result.Outcome != "built" || requests.Reads.Load() != 0 {
		t.Fatalf("a trusted miss: %+v, %d reads", result, requests.Reads.Load())
	}
	// Other bytes for the same key: the store's 409 names both builders, so trusting the index hid nothing.
	result, _ = trusted(map[string]map[string]string{product: {"bin/tool": "built differently"}})
	if result.Outcome != "failed" || !strings.Contains(result.Error, "workshop built") {
		t.Fatalf("a different rebuild: %+v", result)
	}
}
