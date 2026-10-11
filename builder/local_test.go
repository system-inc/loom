package builder

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// testdata/adamiclocal is a cache adamic's own internal/buildcache made, at adamic 6972ea7c97, through
// testdata/adamicprobe's make (untraced, as every product of a tree's build is): two products in its local directory,
// inner and outer, outer's bytes naming inner only as <build cache>/<inner's name key>, each product's .inputs and
// lock, a pointer per name key, and the resolved copy of outer with its lock, as Workshop's tree caches hold them
// (landable-4's, Oct 10). builds.log is the census buildcache wrote while it built them. The resolved copy's text was
// rewritten to name a Workshop path rather than the machine that made the fixture.
const (
	adamicInner          = "0a4dfe24d8e76fa776272933c1bc368530717e08e058593a2f7b7adf8a7d7cac"
	adamicOuter          = "41f812e23bf5d8b05c8905224bb8c3162e0fb5cceef3c9115eea169b9bfc7cae"
	adamicInnerNameKey   = "c3f2d13bee92d2362034aaf96a341af4c346f8810dfb8abca4f91c16c1d2f5c7"
	adamicOuterNameKey   = "8a97deebfa2fca6a9cd4d97b1793c205a71f521fbeb45b25a0ed38e9291e12d2"
	adamicLocalFixture   = "testdata/adamiclocal"
	adamicBuildingLeft   = ".building-0a4dfe24d8e7-1277429600"
	adamicPointerLeft    = ".pointer-1277429600"
	adamicSourceVariable = "LOOM_ADAMIC_SOURCE"
)

// adamicLocalCache copies the fixture's cache into a fresh directory, with what a killed build leaves beside it on
// Workshop (a .building- scratch directory and a .pointer- temporary file), and returns it and its build log.
func adamicLocalCache(t *testing.T) (string, string) {
	t.Helper()
	cache := filepath.Join(t.TempDir(), "cache")
	if err := os.CopyFS(cache, os.DirFS(filepath.Join(adamicLocalFixture, "cache"))); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(cache, LocalDirectory, adamicBuildingLeft), 0o755)
	os.WriteFile(filepath.Join(cache, LocalDirectory, adamicBuildingLeft, "partial"), []byte("half a product"), 0o644)
	os.WriteFile(filepath.Join(cache, LocalDirectory, adamicPointerLeft), []byte("{}"), 0o644)
	return cache, filepath.Join(adamicLocalFixture, "builds.log")
}

// smallSource is a one-file tree's source, as chunks.
func smallSource(t *testing.T) *Source {
	t.Helper()
	source, err := SourceChunks(gitTree(t, map[string]string{"go.mod": "module example.com/tree\n"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	return &source
}

// archiveNames lists a gzipped tar's entry names, in order.
func archiveNames(t *testing.T, archive []byte) []string {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	entries := tar.NewReader(reader)
	for {
		header, err := entries.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
}

// A tree's build keeps its products where adamic's buildcache builds them untraced, in its cache's local directory
// (landable-4: 1,141 products built and none uploaded, Touched finding none of them at the top). Touched finds each by
// the prefix its census line names; its archive holds it at local/<key> with its .inputs, and nothing else; the tree's
// pointers go in an archive of their own, local/<name key>.json and nothing else; and unpacking both into a unit's cache
// puts each where buildcache reads it.
//
// Mutants: Touched reading only the cache's top (the build log names a product the cache holds 0 of); OutputsOf naming
// a product's entries <key>/... instead of local/<key>/... (the archive's names); LocalPointersArchive shipping no
// pointers (the pointer archive's names); ProductEntries allowing every name under local/ (inner's entries in outer's
// archive are allowed).
func TestAProductInAdamicsLocalLayoutIsFoundArchivedAndPlacedWithItsPointers(t *testing.T) {
	cache, log := adamicLocalCache(t)
	touched, err := Touched(log, cache)
	if err != nil || !slices.Equal(touched, []string{adamicInner, adamicOuter}) {
		t.Fatalf("touched %v %v", touched, err)
	}
	for _, key := range []string{adamicInner, adamicOuter} {
		if place, err := ProductPath(cache, key); err != nil || place != LocalDirectory+"/"+key {
			t.Fatalf("product %s is at %q %v", key, place, err)
		}
	}

	archive, files, err := ProductArchive(cache, []string{adamicOuter})
	if err != nil || files != 2 {
		t.Fatal(files, err)
	}
	if names := archiveNames(t, archive); !slices.Equal(names, []string{"local/" + adamicOuter + ".inputs", "local/" + adamicOuter + "/names"}) {
		t.Fatalf("outer's archive holds %v", names)
	}
	pointers, count, err := LocalPointersArchive(cache)
	if err != nil || count != 2 {
		t.Fatal(count, err)
	}
	if names := archiveNames(t, pointers); !slices.Equal(names, []string{"local/" + adamicOuterNameKey + ".json", "local/" + adamicInnerNameKey + ".json"}) {
		t.Fatalf("the pointer archive holds %v", names)
	}

	// Each product's archive holds that product, at either place, and nothing of another's; the pointer archive holds
	// pointers and nothing else.
	outer := ProductEntries(adamicOuter)
	for name, allowed := range map[string]bool{
		"local/" + adamicOuter + "/names": true, "local/" + adamicOuter + ".inputs": true, adamicOuter + "/names": true, adamicOuter + ".inputs": true,
		"local/" + adamicInner + "/inner.txt": false, "local/" + adamicInner + ".inputs": false, "local/" + adamicOuterNameKey + ".json": false,
		"local/" + adamicOuter: false, "local/" + adamicOuter + "/": false, "local/" + adamicOuter + ".lock": false, "local/x/" + adamicOuter + "/names": false,
		"local/." + adamicOuter + ".resolved-7edfde8ac783/names": false, "other/" + adamicOuter + "/names": false,
	} {
		if outer(name) != allowed {
			t.Errorf("outer's archive may hold %s: %v", name, outer(name))
		}
	}
	for name, allowed := range map[string]bool{
		"local/" + adamicInnerNameKey + ".json": true, adamicInnerNameKey + ".json": false, "local/" + adamicPointerLeft: false,
		"local/" + adamicInner + "/inner.txt": false, "local/" + adamicInner + ".inputs": false, "local/" + adamicInnerNameKey + ".json/x": false,
		"local/" + strings.ToUpper(adamicInnerNameKey) + ".json": false,
	} {
		if LocalPointerEntry(name) != allowed {
			t.Errorf("the pointer archive may hold %s: %v", name, LocalPointerEntry(name))
		}
	}

	unit := filepath.Join(t.TempDir(), "adamic-build")
	for _, key := range []string{adamicInner, adamicOuter} {
		product, _, err := ProductArchive(cache, []string{key})
		if err != nil {
			t.Fatal(err)
		}
		if err = Unpack(bytes.NewReader(product), unit, ProductEntries(key)); err != nil {
			t.Fatal(err)
		}
	}
	if err = Unpack(bytes.NewReader(pointers), unit, LocalPointerEntry); err != nil {
		t.Fatal(err)
	}
	checkAdamicLayout(t, cache, unit)
	// A product whose archive holds another's entries is refused.
	mixed, _, err := ProductArchive(cache, []string{adamicInner, adamicOuter})
	if err != nil {
		t.Fatal(err)
	}
	if err = Unpack(bytes.NewReader(mixed), t.TempDir(), ProductEntries(adamicOuter)); err == nil {
		t.Fatal("outer's archive held inner's entries, and they were unpacked")
	}
}

// checkAdamicLayout holds a unit's cache to the layout adamic's buildcache reads: each product's files and .inputs
// under local/, as the tree's cache holds them, every pointer, and none of buildcache's own bookkeeping.
func checkAdamicLayout(t *testing.T, cache, unit string) {
	t.Helper()
	want := []string{
		"local/" + adamicInner + "/inner.txt", "local/" + adamicInner + ".inputs",
		"local/" + adamicOuter + "/names", "local/" + adamicOuter + ".inputs",
		"local/" + adamicInnerNameKey + ".json", "local/" + adamicOuterNameKey + ".json",
	}
	got := []string{}
	err := filepath.WalkDir(unit, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, err := filepath.Rel(unit, path)
		got = append(got, filepath.ToSlash(relative))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("the unit's cache holds\n%v\nand adamic reads\n%v", got, want)
	}
	for _, name := range want {
		built, _ := os.ReadFile(filepath.Join(cache, filepath.FromSlash(name)))
		unpacked, err := os.ReadFile(filepath.Join(unit, filepath.FromSlash(name)))
		if err != nil || !bytes.Equal(built, unpacked) {
			t.Fatalf("%s: %q, built as %q: %v", name, unpacked, built, err)
		}
	}
}

// A tree whose products are in the local layout goes up whole and comes down whole: PublishTree uploads each product's
// archive under its key and the tree's pointers as one blob the index names, ParseTree accepts it, and FetchPackage
// places the products and the pointers in the unit's cache as adamic reads them. A held product's archive in that
// layout is served to buildcache from its own directory.
//
// Mutants: PublishTree not uploading the pointers (the index names none, and the unit's cache lacks them); FetchPackage
// not unpacking them (the unit's cache lacks them); place renaming a unit's local directory only when it is missing
// (the second package's products never join the first's); HeldProducts walking <key> at the top only (its manifest
// lists no file); ParseTree accepting any pointers' name (the poisoned index parses).
func TestATreeInAdamicsLocalLayoutGoesUpAndComesDownWhole(t *testing.T) {
	cache, _ := adamicLocalCache(t)
	binaries := t.TempDir()
	index := TreeIndex{Tree: strings.Repeat("6", 40), Go: "go1.27.0", Goos: "linux", Goarch: "amd64", Packages: map[string]TreePackage{
		"example.com/outer": {Package: "example.com/outer", Directory: "outer", Products: []string{adamicOuter}},
		"example.com/both":  {Package: "example.com/both", Directory: "both", Products: []string{adamicInner, adamicOuter}},
	}}
	for name := range index.Packages {
		os.WriteFile(filepath.Join(binaries, strings.ReplaceAll(name, "/", "_")+".test"), []byte("a test binary of "+name), 0o755)
	}
	fake, store := serve(t)
	treeKey, written, err := PublishTree(store, &index, binaries, cache, smallSource(t), nil)
	if err != nil || !written {
		t.Fatal(written, err)
	}
	content, _ := fake.Object("trees/" + treeKey + ".json")
	parsed, err := ParseTree(treeKey, content)
	if err != nil || parsed.LocalPointers == "" || !strings.Contains(string(content), `"localPointers":"`+parsed.LocalPointers+`"`) {
		t.Fatalf("the index %s: %v", content, err)
	}
	if blob, found := fake.Object("blobs/" + parsed.LocalPointers); !found || !slices.Equal(archiveNames(t, blob),
		[]string{"local/" + adamicOuterNameKey + ".json", "local/" + adamicInnerNameKey + ".json"}) {
		t.Fatalf("the pointers' blob: %v", found)
	}
	for _, key := range []string{adamicInner, adamicOuter} {
		if ref, _ := fake.Object("refs/action/" + key); string(ref) != parsed.Products[key] {
			t.Fatalf("product %s: the ref %q, the index %q", key, ref, parsed.Products[key])
		}
	}
	runner := Store{Read: fake.Public()}
	unit := t.TempDir()
	if _, err = runner.FetchPackage(context.Background(), treeKey, "example.com/both", unit); err != nil {
		t.Fatal(err)
	}
	checkAdamicLayout(t, cache, filepath.Join(unit, "cache"))
	// A unit whose cache already holds some of the local layout gets the rest beside it.
	partial := t.TempDir()
	os.MkdirAll(filepath.Join(partial, "cache", LocalDirectory, adamicInner), 0o755)
	os.WriteFile(filepath.Join(partial, "cache", LocalDirectory, adamicInner, "inner.txt"), []byte("the inner product\n"), 0o644)
	os.WriteFile(filepath.Join(partial, "cache", LocalDirectory, adamicInner+".inputs"), []byte("name loom probe inner\nfile inner.txt\n"), 0o644)
	if _, err = runner.FetchPackage(context.Background(), treeKey, "example.com/outer", partial); err != nil {
		t.Fatal(err)
	}
	checkAdamicLayout(t, cache, filepath.Join(partial, "cache"))
	// An index naming pointers that aren't a sha256 is refused.
	poisoned := strings.Replace(string(content), parsed.LocalPointers, "../../etc", 1)
	if _, err = ParseTree(treeKey, []byte(poisoned)); err == nil || !strings.Contains(err.Error(), "local pointers") {
		t.Fatalf("an index naming pointers %q: %v", "../../etc", err)
	}

	// The held product, served from its archive's local directory.
	held, err := ServeHeldProducts(store, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	response, err := store.client().Get(held.Address + "/refs/build/" + adamicOuter)
	if err != nil {
		t.Fatal(err)
	}
	manifestSum, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("the held product: %s %v", response.Status, held.Err())
	}
	response, err = store.client().Get(held.Address + "/blobs/" + string(manifestSum))
	if err != nil {
		t.Fatal(err)
	}
	var manifest heldManifest
	err = json.NewDecoder(response.Body).Decode(&manifest)
	response.Body.Close()
	if err != nil || len(manifest.Files) != 1 || manifest.Files[0].Path != "names" {
		t.Fatalf("the held product's manifest %+v %v", manifest, err)
	}
}

// Touched is still an error, never a guess, when a prefix names a product at the top and one in the local directory,
// and ProductPath when a key is at both or neither; a cache with no local directory reads as main's layout did.
//
// Mutant: Touched taking the first of two products a prefix names (a product at both places resolves).
func TestAProductAtBothPlacesOrNeitherIsAnError(t *testing.T) {
	cache, logs := t.TempDir(), t.TempDir()
	key := strings.Repeat("d", 64)
	log := filepath.Join(logs, "builds.log")
	os.WriteFile(log, []byte("build p dddddddddddd hit 0.01\n"), 0o644)
	os.MkdirAll(filepath.Join(cache, key), 0o755)
	if touched, err := Touched(log, cache); err != nil || !slices.Equal(touched, []string{key}) {
		t.Fatalf("main's layout: %v %v", touched, err)
	}
	os.MkdirAll(filepath.Join(cache, LocalDirectory, key), 0o755)
	if touched, err := Touched(log, cache); err == nil {
		t.Fatalf("a key at both places resolved to %v", touched)
	}
	if _, err := ProductPath(cache, key); err == nil {
		t.Fatal("a key at both places has a path")
	}
	if _, err := ProductPath(cache, strings.Repeat("e", 64)); err == nil {
		t.Fatal("a key at neither place has a path")
	}
	if pointers, count, err := LocalPointersArchive(t.TempDir()); err != nil || count != 0 || pointers != nil {
		t.Fatalf("a cache with no local directory has pointers: %d %v", count, err)
	}
}

// adamic's own buildcache reads a nested name in a unit's cache that loom unpacked: testdata/adamicprobe, built
// against adamic's internal/buildcache (LOOM_ADAMIC_SOURCE: a directory holding adamic's go.mod and
// internal/buildcache, at 6972ea7c97 or later), makes inner and outer on "Workshop", the tree goes up through
// PublishTree and comes down through FetchPackage into another checkout's unit cache, and there buildcache.Absolute
// reads outer's name for inner back to inner's file through inner's pointer. The same unit without the pointers reads
// nothing: the failure a test would report as the change's red. It is skipped without LOOM_ADAMIC_SOURCE, since loom
// holds no copy of adamic's code; testdata/adamiclocal is what it makes.
//
// Mutants: PublishTree not uploading the pointers, or FetchPackage not unpacking them (Absolute reads
// <cache>/<name key>, which holds nothing).
func TestAdamicReadsANestedNameInAUnitsCache(t *testing.T) {
	adamic := os.Getenv(adamicSourceVariable)
	if adamic == "" {
		t.Skipf("%s names no adamic source to build the probe against", adamicSourceVariable)
	}
	work := t.TempDir()
	module := filepath.Join(work, "workshop")
	probeSource, err := os.ReadFile("testdata/adamicprobe/main.go")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"go.mod": []byte("module github.com/system-inc/adamic\n\ngo 1.27\n"), "inner.txt": []byte("inner\n"), "outer.txt": []byte("outer\n"),
		"internal/buildcache/cmd/adamicprobe/main.go": probeSource}
	sources, err := filepath.Glob(filepath.Join(adamic, "internal", "buildcache", "*.go"))
	if err != nil || len(sources) == 0 {
		t.Fatalf("%s holds no internal/buildcache: %v", adamic, err)
	}
	for _, file := range sources {
		if !strings.HasSuffix(file, "_test.go") {
			content, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			files["internal/buildcache/"+filepath.Base(file)] = content
		}
	}
	for name, content := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(module, name)), 0o755)
		if err = os.WriteFile(filepath.Join(module, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	probe := filepath.Join(work, "adamicprobe")
	command := exec.Command("go", "build", "-o", probe, "./internal/buildcache/cmd/adamicprobe")
	command.Dir = module
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("building the probe: %v\n%s", err, output)
	}
	run := func(checkout, cache, log, mode string) (string, error) {
		command := exec.Command(probe, mode)
		command.Dir = checkout
		command.Env = append(os.Environ(), "ADAMIC_BUILD_CACHE_DIR="+cache, "ADAMIC_BUILD_LOG="+log, "ADAMIC_BUILD_CACHE=on", "ADAMIC_BUILD_STORE=off")
		output, err := command.CombinedOutput()
		return string(output), err
	}
	cache, log := filepath.Join(work, "cache"), filepath.Join(work, "builds.log")
	if output, err := run(module, cache, log, "make"); err != nil {
		t.Fatalf("make: %v\n%s", err, output)
	}
	touched, err := Touched(log, cache)
	if err != nil || len(touched) != 2 {
		t.Fatalf("touched %v %v", touched, err)
	}
	binaries := t.TempDir()
	os.WriteFile(filepath.Join(binaries, "example.com_probe.test"), []byte("a test binary"), 0o755)
	index := TreeIndex{Tree: strings.Repeat("5", 40), Go: "go1.27.0", Goos: "linux", Goarch: "amd64",
		Packages: map[string]TreePackage{"example.com/probe": {Package: "example.com/probe", Directory: "probe", Products: touched}}}
	fake, store := serve(t)
	treeKey, _, err := PublishTree(store, &index, binaries, cache, smallSource(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The runner's checkout of the same tree, at another path.
	checkout := filepath.Join(work, "runner")
	if err = os.CopyFS(checkout, os.DirFS(module)); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(work, "unit")
	if _, err = (Store{Read: fake.Public()}).FetchPackage(context.Background(), treeKey, "example.com/probe", unit); err != nil {
		t.Fatal(err)
	}
	read := func(cache string) (string, error) {
		return run(checkout, cache, filepath.Join(work, "unit.log"), "read")
	}
	output, err := read(filepath.Join(unit, "cache"))
	if err != nil || !strings.HasPrefix(output, filepath.Join(unit, "cache", LocalDirectory)) || !strings.HasSuffix(output, "the inner product\n") {
		t.Fatalf("adamic read the unit's cache: %v\n%s", err, output)
	}
	t.Logf("adamic read outer's name for inner as %s", output)
	// The same products without the pointers: the nested name reads nothing.
	bare := filepath.Join(work, "bare")
	for _, key := range touched {
		product, _, err := ProductArchive(cache, []string{key})
		if err == nil {
			err = Unpack(bytes.NewReader(product), bare, ProductEntries(key))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if output, err = read(bare); err == nil || !strings.Contains(output, "no such file or directory") {
		t.Fatalf("adamic read a nested name with no pointers: %v\n%s", err, output)
	}
}
