package builder

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/system-inc/loom/r2"
)

// The action store is the public bucket, loom-artifacts (Kirk, Oct 10). A builder writes it straight through R2's S3
// interface with its own key (Store.Bucket), and anyone reads it straight from its public domain (Store.Read,
// https://artifacts.loom.system.inc): no Worker is in the data path. It holds three kinds of object:
//
//	blobs/<sha256>            gzipped bytes, named by their own sha256: a test binary, a product's archive, or a
//	                          tree's source archive
//	refs/action/<productKey>  the sha256 of that product's archive, as text, written once and never changed
//	trees/<treeKey>.json      a tree's index (TreeIndex): each package's binary, the products its tests read, the
//	                          source archive
//
// The bucket's lifecycle deletes every blob and ref 7 days after its upload. So a blob the store holds is skipped
// only while it was uploaded within FreshFor; an older one goes up again, which starts its 7 days over, and a ref is
// only ever written after its blob is fresh, so no ref written today names a blob that vanishes tomorrow.

// FreshFor is how recently a blob must have been uploaded for a builder to rely on it without sending it again,
// leaving two of the lifecycle's 7 days for the runners that read it.
const FreshFor = 5 * 24 * time.Hour

// ImmutableBlob is a blob's Cache-Control: named by its hash, its bytes never change, so the edge may keep it.
const ImmutableBlob = "public, max-age=31536000, immutable"

// A Store is the action store: Read is the public domain, read with no credentials; Bucket the S3 interface a
// builder writes (nil for a runner); Blobs, when set, a runner's local cache of blobs by sha256; and Now the clock a
// blob's age is read by (nil means time.Now).
type Store struct {
	Read   string
	Bucket *r2.Bucket
	Client *http.Client
	Blobs  string
	Now    func() time.Time
	// SkipNative leaves out of a fetch every product its buildcache description says clang built, so the runner
	// builds those itself (Judge's ruling until native products are reproducible, #tsn1wp8): Go products serve.
	SkipNative bool
	// Requests counts what this store was asked, for measuring a build's cost (#k62gwdt). Shared by copies of the Store.
	Requests *Requests
}

// Requests counts a store's calls: reads are GETs and HEADs, writes are PUTs.
type Requests struct {
	Reads  atomic.Int64
	Writes atomic.Int64
}

// ErrNotStored is an action, blob or tree the store doesn't hold.
var ErrNotStored = errors.New("not in the action store")

// ConflictError is a ref that already names another archive than the one this build made: one key, two products.
// The key isn't honest, or the build isn't reproducible, and since a ref never changes it always fails the build.
type ConflictError struct {
	Key   string
	Held  string
	Built string
}

func (conflict ConflictError) Error() string {
	return fmt.Sprintf("refs/action/%s holds %s and this build made %s: a ref never changes, so this key isn't honest or its build isn't reproducible", conflict.Key, conflict.Held, conflict.Built)
}

func (store Store) now() time.Time {
	if store.Now != nil {
		return store.Now()
	}
	return time.Now()
}

func (store Store) client() *http.Client {
	if store.Client != nil {
		return store.Client
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (store Store) read() {
	if store.Requests != nil {
		store.Requests.Reads.Add(1)
	}
}

func (store Store) wrote() {
	if store.Requests != nil {
		store.Requests.Writes.Add(1)
	}
}

func digest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// get reads one object from the public domain.
func (store Store) get(key string) ([]byte, error) {
	store.read()
	response, err := store.client().Get(store.Read + "/" + key)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s: %w", key, ErrNotStored)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s/%s answered %s", store.Read, key, response.Status)
	}
	return io.ReadAll(response.Body)
}

// blob reads one blob and checks it hashes to its name: from the runner's local cache when it holds it, otherwise
// from the public domain, kept in the local cache once it checks.
func (store Store) blob(sum string) ([]byte, error) {
	if !productKeyPattern.MatchString(sum) {
		return nil, fmt.Errorf("%q isn't a blob's sha256: the store is poisoned", sum)
	}
	if store.Blobs != "" {
		local := filepath.Join(store.Blobs, sum)
		if content, err := os.ReadFile(local); err == nil {
			if digest(content) == sum {
				return content, nil
			}
			os.Remove(local)
		}
	}
	content, err := store.get("blobs/" + sum)
	if err != nil {
		return nil, err
	}
	if actual := digest(content); actual != sum {
		return nil, fmt.Errorf("blob %s hashes to %s: the store is poisoned", sum, actual)
	}
	if store.Blobs != "" {
		if err = os.MkdirAll(store.Blobs, 0o755); err != nil {
			return nil, err
		}
		partial, err := os.CreateTemp(store.Blobs, "."+sum[:12]+"-")
		if err != nil {
			return nil, err
		}
		_, err = partial.Write(content)
		if closeErr := partial.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(partial.Name(), filepath.Join(store.Blobs, sum))
		}
		if err != nil {
			os.Remove(partial.Name())
			return nil, err
		}
	}
	return content, nil
}

// Ref is the archive refs/action/<key> names, read from the public domain.
func (store Store) Ref(key string) (string, error) {
	content, err := store.get("refs/action/" + key)
	if err != nil {
		return "", err
	}
	target := strings.TrimSpace(string(content))
	if !productKeyPattern.MatchString(target) {
		return "", fmt.Errorf("refs/action/%s holds %q, not an archive's sha256: the store is poisoned", key, target[:min(80, len(target))])
	}
	return target, nil
}

// heldRef is what the bucket itself holds at refs/action/<key>, empty when nothing: a builder asks the bucket, never
// the public domain, whose edge could answer from before the ref was written.
func (store Store) heldRef(key string) (string, error) {
	store.read()
	content, err := store.Bucket.Get("refs/action/" + key)
	if errors.Is(err, r2.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	target := strings.TrimSpace(string(content))
	if !productKeyPattern.MatchString(target) {
		return "", fmt.Errorf("refs/action/%s holds %q, not an archive's sha256: the store is poisoned", key, target[:min(80, len(target))])
	}
	return target, nil
}

// fresh reports whether the bucket holds blob sum, uploaded within FreshFor.
func (store Store) fresh(sum string) (bool, error) {
	store.read()
	object, err := store.Bucket.Head("blobs/" + sum)
	if errors.Is(err, r2.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return store.now().Sub(object.Modified) < FreshFor, nil
}

// PutBlob makes the bucket hold content, fresh, at blobs/<its sha256>, and returns that sha256. A blob the bucket
// holds from within FreshFor isn't sent again; one older, or missing, goes up, which starts its 7 days over.
func (store Store) PutBlob(content []byte) (string, error) {
	sum := digest(content)
	held, err := store.fresh(sum)
	if err != nil || held {
		return sum, err
	}
	store.wrote()
	return sum, store.Bucket.Put("blobs/"+sum, content, r2.PutOptions{ContentType: "application/octet-stream", CacheControl: ImmutableBlob})
}

// Publish stores a product's archive under its key and returns the archive's sha256. A ref naming the same archive
// is found, and only its blob is kept fresh; a ref naming another archive is a ConflictError, and nothing goes up.
// Otherwise the blob goes up first, fresh, then the ref, written only if nothing is there (If-None-Match: *): of two
// builders racing, one writes and the other reads what it wrote, the same archive or a ConflictError. A ref is never
// overwritten.
func (store Store) Publish(key string, archive []byte) (string, error) {
	sum := digest(archive)
	held, err := store.heldRef(key)
	if err != nil {
		return "", err
	}
	if held != "" && held != sum {
		return "", ConflictError{Key: key, Held: held, Built: sum}
	}
	if _, err = store.PutBlob(archive); err != nil {
		return "", err
	}
	if held == sum {
		return sum, nil
	}
	store.wrote()
	err = store.Bucket.Put("refs/action/"+key, []byte(sum), r2.PutOptions{ContentType: "text/plain", CacheControl: "no-cache", IfNoneMatch: true})
	if !errors.Is(err, r2.ErrExists) {
		return sum, err
	}
	if held, err = store.heldRef(key); err != nil {
		return "", err
	}
	if held != sum {
		return "", ConflictError{Key: key, Held: held, Built: sum}
	}
	return sum, nil
}

// Stored reports whether the bucket holds key's product whole and fresh: its ref, and the blob it names uploaded
// within FreshFor. A ref whose blob is older (or gone) isn't stored, so the product is built again and its blob
// sent again, the same bytes, before anything relies on it.
func (store Store) Stored(key string) (string, bool, error) {
	held, err := store.heldRef(key)
	if err != nil || held == "" {
		return "", false, err
	}
	fresh, err := store.fresh(held)
	return held, fresh, err
}

// FetchProduct writes a product's files under directory (a runner's ADAMIC_BUILD_CACHE_DIR): its ref, then its
// archive, checked against its hash and unpacked into a scratch directory, where every entry must be a buildcache
// path (<key>/<file> or <key>.inputs). Only when the whole archive checks is each product renamed into place whole,
// as buildcache itself publishes one, so a poisoned store leaves nothing behind. A product already in the cache is
// left as it is, since its key says what it holds.
func (store Store) FetchProduct(key, directory string) error {
	sum, err := store.Ref(key)
	if err != nil {
		return err
	}
	archive, err := store.blob(sum)
	if err != nil {
		return fmt.Errorf("action %s: %w", key, err)
	}
	if err = os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	scratch, err := os.MkdirTemp(directory, ".fetching-"+key[:min(12, len(key))]+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	if err = Unpack(archive, scratch, buildcachePath); err != nil {
		return fmt.Errorf("action %s: %w: the store is poisoned", key, err)
	}
	return store.place(scratch, directory)
}

// place renames each product unpacked in scratch into directory, leaving out one already there and, with SkipNative,
// one clang built.
func (store Store) place(scratch, directory string) error {
	entries, err := os.ReadDir(scratch)
	if err != nil {
		return err
	}
	skipped := map[string]bool{}
	if store.SkipNative {
		for _, entry := range entries {
			product, isInputs := strings.CutSuffix(entry.Name(), ".inputs")
			if !isInputs {
				continue
			}
			content, err := os.ReadFile(filepath.Join(scratch, entry.Name()))
			if err != nil {
				return err
			}
			if native(content) {
				skipped[product] = true
			}
		}
	}
	for _, entry := range entries {
		if skipped[strings.TrimSuffix(entry.Name(), ".inputs")] {
			continue
		}
		target := filepath.Join(directory, entry.Name())
		if _, err := os.Lstat(target); err == nil {
			continue
		}
		if err = os.Rename(filepath.Join(scratch, entry.Name()), target); err != nil {
			return err
		}
	}
	return nil
}
