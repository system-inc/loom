package builder

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/r2/r2test"
)

// sharedProduct is the buildcache key both trees' product test asks for.
var sharedProduct = strings.Repeat("5", 64)

// buildcacheTest is a product test that does what adamic's buildcache does for one product (internal/buildcache,
// get and fetch): a hit when the cache holds it, else a fetch from ADAMIC_BUILD_STORE (refs/build/<key>, a version 1
// manifest, each file's blob, every hash checked), else a build, whose bytes differ every time as a native product's
// do (#tsn1wp8). It logs its census line and refuses an environment that would audit or publish.
const buildcacheTest = `package p

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const key = "` + "5555555555555555555555555555555555555555555555555555555555555555" + `"

func get(t *testing.T, address string) ([]byte, bool) {
	response, err := http.Get(address)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, false
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s answered %s", address, response.Status)
	}
	content, _ := io.ReadAll(response.Body)
	return content, true
}

func blob(t *testing.T, store, sum string) []byte {
	content, found := get(t, store+"/blobs/"+sum)
	if actual := sha256.Sum256(content); !found || hex.EncodeToString(actual[:]) != sum {
		t.Fatalf("blob %s: found %v, hashes to %x", sum, found, actual)
	}
	return content
}

func TestProduct_Shared(t *testing.T) {
	if os.Getenv("ADAMIC_BUILD_AUDIT") != "0" || os.Getenv("ADAMIC_BUILD_STORE_TOKEN") != os.DevNull {
		t.Fatal("a tree's product tests must neither audit nor publish")
	}
	cache := os.Getenv("ADAMIC_BUILD_CACHE_DIR")
	product := filepath.Join(cache, key)
	outcome := "hit"
	if _, err := os.Stat(product); err != nil {
		outcome = "miss"
		scratch, _ := os.MkdirTemp(cache, ".building-")
		if store := os.Getenv("ADAMIC_BUILD_STORE"); store != "" && store != "off" {
			if reference, found := get(t, store+"/refs/build/"+key); found {
				var manifest struct {
					Version int
					Key     string
					Files   []struct {
						Path, SHA256 string
						Size         int64
						Mode         uint32
					}
				}
				if err := json.Unmarshal(blob(t, store, string(reference)), &manifest); err != nil || manifest.Version != 1 || manifest.Key != key {
					t.Fatalf("manifest %+v %v", manifest, err)
				}
				for _, file := range manifest.Files {
					target := filepath.Join(scratch, filepath.FromSlash(file.Path))
					os.MkdirAll(filepath.Dir(target), 0o755)
					os.WriteFile(target, blob(t, store, file.SHA256), os.FileMode(file.Mode&0o755))
				}
				outcome = "fetched"
			}
		}
		if outcome == "miss" {
			os.WriteFile(filepath.Join(scratch, "tool"), []byte(time.Now().Format(time.RFC3339Nano)), 0o755)
		}
		os.WriteFile(product+".inputs", []byte("name shared\ntool clang --version: clang version 20.1.8\n"), 0o644)
		if err := os.Rename(scratch, product); err != nil {
			t.Fatal(err)
		}
	}
	log, _ := os.OpenFile(os.Getenv("ADAMIC_BUILD_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	fmt.Fprintf(log, "build shared %s %s 0.01\n", key[:12], outcome)
	log.Close()
	if info, err := os.Stat(filepath.Join(product, "tool")); err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("the product's tool: %v %v", info, err)
	}
}
`

// heldBuild builds one tree with the store's products offered to it, publishes it, and returns its index and the
// product test's build log.
func heldBuild(t *testing.T, store Store, version string) (TreeIndex, string) {
	t.Helper()
	tree := gitTree(t, map[string]string{"go.mod": "module example.com/held\n\ngo 1.22\n", "p/p_test.go": buildcacheTest, "p/version.txt": version})
	work := t.TempDir()
	build := TreeBuild{Tree: tree, Cache: filepath.Join(work, "cache"), Out: filepath.Join(work, "out"), Environment: planner.GateEnvironmentList(), Jobs: 1}
	os.MkdirAll(build.Cache, 0o755)
	os.MkdirAll(build.Out, 0o755)
	held, err := ServeHeldProducts(store, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	build.Held = held.Address
	logs := t.TempDir()
	products, failed := build.Products([]planner.ProductTest{{Package: "example.com/held/p", Directory: "p", Test: "TestProduct_Shared"}}, logs)
	if len(failed) != 0 || held.Err() != nil {
		t.Fatalf("the product test: %v %v", failed, held.Err())
	}
	build.Held = ""
	packages, err := TestPackages(tree)
	if err != nil {
		t.Fatal(err)
	}
	index := TreeIndex{Tree: strings.Repeat(version, 40)[:40], Go: "go1.27.1", Packages: map[string]TreePackage{}}
	for _, result := range build.Binaries(packages) {
		result.Products = products[result.Package]
		index.Packages[result.Package] = result
	}
	source, err := SourceChunks(tree, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = PublishTree(store, &index, build.Out, build.Cache, &source, held.Held()); err != nil {
		t.Fatal(err)
	}
	if broke := index.Packages["example.com/held/p"].Error; broke != "" {
		t.Fatalf("package p: %s", broke)
	}
	log, _ := os.ReadFile(filepath.Join(logs, "product-0.log"))
	return index, string(log)
}

// Two trees that share a product build it once: the second tree's buildcache fetches it from the store, its log
// says so, and the store sees one put of that product's blob and one of its ref, though a rebuild would have made
// other bytes. A held blob uploaded more than FreshFor ago is fetched, used, and refreshed in the bucket, not sent.
func TestTwoTreesSharingAProductBuildItOnce(t *testing.T) {
	fake, store := serve(t)
	first, log := heldBuild(t, store, "1")
	if !strings.Contains(log, "build shared "+sharedProduct[:12]+" miss") {
		t.Fatalf("the first tree's log: %q", log)
	}
	archive := first.Products[sharedProduct]
	if archive == "" {
		t.Fatalf("the first tree's index names no archive for the product: %+v", first.Products)
	}
	second, log := heldBuild(t, store, "2")
	if !strings.Contains(log, "build shared "+sharedProduct[:12]+" fetched") {
		t.Fatalf("the second tree's log: %q", log)
	}
	if second.Products[sharedProduct] != archive {
		t.Fatalf("the second tree names %s, not the held %s", second.Products[sharedProduct], archive)
	}
	if blobs, refs := fake.Count("PUT", "blobs/"+archive), fake.Count("PUT", "refs/action/"+sharedProduct); blobs != 1 || refs != 1 {
		t.Fatalf("the product went up %d times and its ref %d: %v", blobs, refs, fake.Requests())
	}

	// A day past FreshFor, the blob and its ref are in their last days: the third tree fetches the blob, uses it, and
	// refreshes both in the bucket.
	fake.Set("blobs/"+archive, mustObject(t, fake, "blobs/"+archive), time.Now().Add(-pastFresh))
	fake.Set("refs/action/"+sharedProduct, mustObject(t, fake, "refs/action/"+sharedProduct), time.Now().Add(-pastFresh))
	third, log := heldBuild(t, store, "3")
	if !strings.Contains(log, " fetched ") || third.Products[sharedProduct] != archive {
		t.Fatalf("the third tree: %q, %s", log, third.Products[sharedProduct])
	}
	if fake.Count("PUT", "blobs/"+archive) != 1 || fake.Count("PUT", "refs/action/"+sharedProduct) != 1 || fake.Count("COPY", "blobs/"+archive) != 1 || fake.Count("COPY", "refs/action/"+sharedProduct) != 1 || time.Since(fake.Modified("blobs/"+archive)) > time.Minute || time.Since(fake.Modified("refs/action/"+sharedProduct)) > time.Minute {
		t.Fatalf("a stale held blob: %v, modified %v", fake.Requests(), fake.Modified("blobs/"+archive))
	}
}

func mustObject(t *testing.T, fake *r2test.Fake, key string) []byte {
	t.Helper()
	content, found := fake.Object(key)
	if !found {
		t.Fatalf("the bucket holds no %s", key)
	}
	return content
}

// The server refuses a held archive that doesn't check, and says so, so the build fails rather than quietly building.
func TestHeldProductsRefusesAPoisonedArchive(t *testing.T) {
	honest := tarGzip(t, entry{name: sharedProduct + "/tool", body: "the tool"})
	other := tarGzip(t, entry{name: strings.Repeat("6", 64) + "/tool", body: "another product's tool"})
	for name, plant := range map[string]func(fake *r2test.Fake){
		"another product's files": func(fake *r2test.Fake) {
			fake.Set("blobs/"+digest(other), other, time.Now())
			fake.Set("refs/action/"+sharedProduct, []byte(digest(other)), time.Now())
		},
		"the same product, other bytes than its hash": func(fake *r2test.Fake) {
			fake.Set("blobs/"+digest(honest), tarGzip(t, entry{name: sharedProduct + "/tool", body: "a tool someone swapped in"}), time.Now())
			fake.Set("refs/action/"+sharedProduct, []byte(digest(honest)), time.Now())
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake, store := serve(t)
			plant(fake)
			held, err := ServeHeldProducts(store, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			response, err := store.client().Get(held.Address + "/refs/build/" + sharedProduct)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode == 200 || held.Err() == nil || !strings.Contains(held.Err().Error(), "poisoned") || len(held.Held()) != 0 {
				t.Fatalf("a poisoned archive: %s, %v", response.Status, held.Err())
			}
		})
	}
	// A key the store doesn't hold is a miss, and no failure.
	_, store := serve(t)
	held, err := ServeHeldProducts(store, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	response, err := store.client().Get(held.Address + "/refs/build/" + strings.Repeat("7", 64))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 404 || held.Err() != nil {
		t.Fatalf("an unheld key: %s %v", response.Status, held.Err())
	}
}

// An ask that failed after the archive unpacked (here, the store refusing the ref's refresh) leaves nothing that
// trips the next ask for the same key: each ask unpacks into its own directory.
func TestHeldProductsAsksAgainAfterAFailure(t *testing.T) {
	fake, store := serve(t)
	archive := tarGzip(t, entry{name: sharedProduct + "/tool", body: "the tool"}, entry{name: sharedProduct + ".inputs", body: "name shared"})
	fake.Set("blobs/"+digest(archive), archive, time.Now())
	fake.Set("refs/action/"+sharedProduct, []byte(digest(archive)), time.Now().Add(-pastFresh))
	var refuse atomic.Bool
	refuse.Store(true)
	fake.Answer = func(method, key string) int {
		if refuse.Load() && method == "COPY" && key == "refs/action/"+sharedProduct {
			return 403
		}
		return 0
	}
	held, err := ServeHeldProducts(store, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	ask := func() int {
		response, err := store.client().Get(held.Address + "/refs/build/" + sharedProduct)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	if status := ask(); status == 200 || held.Err() == nil {
		t.Fatalf("a refused refresh: %d %v", status, held.Err())
	}
	refuse.Store(false)
	if status := ask(); status != 200 || held.Held()[sharedProduct] != digest(archive) {
		t.Fatalf("the second ask: %d %v", status, held.Err())
	}
}
