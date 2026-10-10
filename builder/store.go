package builder

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/system-inc/loom/housecache"
	"github.com/system-inc/loom/r2"
)

// The action store is the public bucket, loom-artifacts (Kirk, Oct 10). A builder writes it straight through R2's S3
// interface with its own key (Store.Bucket), and anyone reads it straight from its public domain (Store.Read,
// https://artifacts.loom.system.inc): no Worker is in the data path. It holds three kinds of object:
//
//	blobs/<sha256>            gzipped bytes, named by their own sha256: a test binary, a product's archive, or a
//	                          chunk of a tree's source
//	refs/action/<productKey>  the sha256 of that product's archive, as text, never changed (only rewritten with its
//	                          own bytes, to keep it fresh)
//	trees/<treeKey>.json      a tree's index (TreeIndex): each package's binary, the products its tests read, the
//	                          source's chunks
//
// The bucket's lifecycle deletes every blob, ref and tree index 7 days after its upload (trees/ by Loom's own rule,
// Oct 10, so old indexes naming expired blobs don't pile up). So a blob or ref the store holds is relied on as it is
// only while it was uploaded within FreshFor; an older one a build relies on is written again, the same bytes, which
// starts its 7 days over (a held product is refreshed, never rebuilt), and a ref is only ever written after its blob
// is fresh, so no ref written today names a blob that vanishes tomorrow.

// FreshFor is how recently a blob or ref must have been uploaded for a builder to rely on it without writing it
// again, leaving two of the lifecycle's 7 days for the runners that read it.
const FreshFor = 5 * 24 * time.Hour

// PublicRead is the action store's public domain, which anyone reads with no credentials.
const PublicRead = "https://artifacts.loom.system.inc"

// ImmutableBlob is a blob's Cache-Control: named by its hash, its bytes never change, so the edge may keep it.
const ImmutableBlob = "public, max-age=31536000, immutable"

// A Store is the action store: Read is the public domain, read with no credentials; Bucket the S3 interface a
// builder writes (nil for a runner); Blobs, when set, a runner's local cache of blobs by sha256; House, when set, the
// house cache (docs/house-cache.md), asked first for every blob; and Now the clock a blob's age is read by (nil means
// time.Now).
type Store struct {
	Read   string
	Bucket *r2.Bucket
	Client *http.Client
	Blobs  string
	House  string
	// HouseClient asks the house cache; nil means housecache.Client.
	HouseClient *http.Client
	Now         func() time.Time
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

// ConflictError is a ref that already names an archive of other files than the one this build made: one key, two
// products. The key isn't honest, or the build isn't reproducible, and since a ref never changes it always fails the
// build.
type ConflictError struct {
	Key   string
	Held  string
	Built string
}

func (conflict ConflictError) Error() string {
	return fmt.Sprintf("refs/action/%s holds %s and this build made %s, other files: a ref never changes, so this key isn't honest or its build isn't reproducible", conflict.Key, conflict.Held, conflict.Built)
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

// get reads one object from the public domain, within readContext.
func (store Store) get(readContext context.Context, key string) ([]byte, error) {
	store.read()
	request, err := http.NewRequestWithContext(readContext, http.MethodGet, store.Read+"/"+key, nil)
	if err != nil {
		return nil, err
	}
	response, err := store.client().Do(request)
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
func (store Store) blob(readContext context.Context, sum string) ([]byte, error) {
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
	// The house cache's copy is used only when it hashes to its name; anything else it gives costs a read of the store.
	content, err := store.housed(readContext, sum)
	if err != nil || digest(content) != sum {
		if content, err = store.get(readContext, "blobs/"+sum); err != nil {
			return nil, err
		}
		if actual := digest(content); actual != sum {
			return nil, fmt.Errorf("blob %s hashes to %s: the store is poisoned", sum, actual)
		}
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

// houseClient asks the house cache when a Store sets no HouseClient.
var houseClient = sync.OnceValue(housecache.Client)

// housed reads blob sum from the house cache, unchecked; an error when there is none, or it doesn't answer whole. Its
// ask has its own context within readContext, cut once its answer stalls or trickles (housecache.Watch), so the house
// cache never spends the caller's time; one that didn't answer is judged (housecache.Unanswered).
func (store Store) housed(readContext context.Context, sum string) ([]byte, error) {
	through := housecache.Through(store.House, store.Read+"/blobs/"+sum)
	if through == "" {
		return nil, errors.New("no house cache")
	}
	client := store.HouseClient
	if client == nil {
		client = houseClient()
	}
	houseContext, cancel := context.WithCancel(readContext)
	defer cancel()
	request, err := http.NewRequestWithContext(houseContext, http.MethodGet, through, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		if readContext.Err() == nil {
			housecache.Unanswered(store.House)
		}
		return nil, err
	}
	body := housecache.Watch(response.Body, cancel)
	defer body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", through, response.Status)
	}
	content, err := io.ReadAll(body)
	if err != nil && readContext.Err() == nil {
		housecache.Unanswered(store.House)
	}
	return content, err
}

// Ref is the archive refs/action/<key> names, read from the public domain.
func (store Store) Ref(readContext context.Context, key string) (string, error) {
	content, err := store.get(readContext, "refs/action/"+key)
	if err != nil {
		return "", err
	}
	target := strings.TrimSpace(string(content))
	if !productKeyPattern.MatchString(target) {
		return "", fmt.Errorf("refs/action/%s holds %q, not an archive's sha256: the store is poisoned", key, target[:min(80, len(target))])
	}
	return target, nil
}

// A heldRef is what the bucket holds at refs/action/<key>: the archive it names, and the object itself, whose age
// and ETag a refresh reads.
type heldRef struct {
	Sum    string
	Object r2.Object
}

// heldRef is what the bucket itself holds at refs/action/<key>, Sum empty when nothing: a builder asks the bucket,
// never the public domain, whose edge could answer from before the ref was written.
func (store Store) heldRef(key string) (heldRef, error) {
	store.read()
	content, object, err := store.Bucket.GetObject("refs/action/" + key)
	if errors.Is(err, r2.ErrNotFound) {
		return heldRef{}, nil
	}
	if err != nil {
		return heldRef{}, err
	}
	target := strings.TrimSpace(string(content))
	if !productKeyPattern.MatchString(target) {
		return heldRef{}, fmt.Errorf("refs/action/%s holds %q, not an archive's sha256: the store is poisoned", key, target[:min(80, len(target))])
	}
	return heldRef{Sum: target, Object: object}, nil
}

// refreshRef keeps a held ref fresh: one written more than FreshFor ago is written again, the same bytes, only over
// the very object read (If-Match on its ETag), so a runner that reads it has two days, as a blob's reader does. A ref
// another builder rewrote first is read again and must still name sum.
func (store Store) refreshRef(key string, held heldRef) error {
	if store.now().Sub(held.Object.Modified) < FreshFor {
		return nil
	}
	store.wrote()
	err := store.Bucket.Put("refs/action/"+key, []byte(held.Sum), r2.PutOptions{ContentType: "text/plain", CacheControl: "no-cache", IfMatch: held.Object.ETag})
	if !errors.Is(err, r2.ErrChanged) {
		return err
	}
	again, err := store.heldRef(key)
	if err != nil {
		return err
	}
	if again.Sum != held.Sum {
		return fmt.Errorf("refs/action/%s changed from %s to %s while it was refreshed: a ref never changes", key, held.Sum, again.Sum)
	}
	return nil
}

// stale reports whether a blob or ref written at modified must be written again before anything relies on it.
func (store Store) stale(modified time.Time) bool {
	return store.now().Sub(modified) >= FreshFor
}

// putBlob sends content to blobs/<sum>.
func (store Store) putBlob(sum string, content []byte) error {
	store.wrote()
	return store.Bucket.Put("blobs/"+sum, content, r2.PutOptions{ContentType: "application/octet-stream", CacheControl: ImmutableBlob})
}

// PutBlob makes the bucket hold content, fresh, at blobs/<its sha256>, and returns that sha256. A blob the bucket
// holds from within FreshFor isn't sent again; one older, or missing, goes up, which starts its 7 days over.
func (store Store) PutBlob(content []byte) (string, error) {
	_, err := store.sendBlob(content)
	return digest(content), err
}

// sendBlob is PutBlob, reporting whether content went up.
func (store Store) sendBlob(content []byte) (bool, error) {
	sum := digest(content)
	store.read()
	object, err := store.Bucket.Head("blobs/" + sum)
	switch {
	case err == nil && !store.stale(object.Modified):
		return false, nil
	case err != nil && !errors.Is(err, r2.ErrNotFound):
		return false, err
	}
	return true, store.putBlob(sum, content)
}

// heldBlob reads blob sum from the bucket, checked against its hash, and sends it again, the same bytes, when it was
// uploaded more than FreshFor ago: a blob the store holds is refreshed, never rebuilt. A blob the bucket lacks is
// ErrNotStored.
func (store Store) heldBlob(sum string) ([]byte, error) {
	store.read()
	content, object, err := store.Bucket.GetObject("blobs/" + sum)
	if errors.Is(err, r2.ErrNotFound) {
		return nil, fmt.Errorf("blobs/%s: %w", sum, ErrNotStored)
	}
	if err != nil {
		return nil, err
	}
	if actual := digest(content); actual != sum {
		return nil, fmt.Errorf("blob %s hashes to %s: the store is poisoned", sum, actual)
	}
	if store.stale(object.Modified) {
		if err = store.putBlob(sum, content); err != nil {
			return nil, err
		}
	}
	return content, nil
}

// sameContent reports whether two archives hold the same tar, whatever gzip made of it: a Go whose compress/flate
// writes other bytes for the same files makes another sha256, never another product.
func sameContent(left, right []byte) (bool, error) {
	leftTar, err := gunzipped(left)
	if err != nil {
		return false, err
	}
	rightTar, err := gunzipped(right)
	if err != nil {
		return false, err
	}
	return digest(leftTar) == digest(rightTar), nil
}

// Publish stores a product's archive under its key and returns the sha256 of the archive the store now names for
// it. A ref naming the same archive is found, and it and its blob are kept fresh. A ref naming another archive holding
// the same tar (gzip's bytes changed, the files didn't) is that archive, kept fresh; one holding other files is a
// ConflictError, and nothing goes up. A ref whose archive the lifecycle already took names nothing, and is pointed at
// this build's (replaceGone). Otherwise the blob goes up first, fresh, then the ref, written only if nothing is there
// (If-None-Match: *): of two builders racing, one writes and the other is held to what it wrote. A ref naming an
// archive the store holds is never overwritten with anything but its own bytes.
func (store Store) Publish(key string, archive []byte) (string, error) {
	sum := digest(archive)
	held, err := store.heldRef(key)
	if err != nil {
		return "", err
	}
	if held.Sum == "" {
		if _, err = store.PutBlob(archive); err != nil {
			return "", err
		}
		store.wrote()
		err = store.Bucket.Put("refs/action/"+key, []byte(sum), r2.PutOptions{ContentType: "text/plain", CacheControl: "no-cache", IfNoneMatch: true})
		if !errors.Is(err, r2.ErrExists) {
			return sum, err
		}
		if held, err = store.heldRef(key); err != nil {
			return "", err
		}
	}
	return store.agree(key, held, archive)
}

// agree holds this build's archive to the ref the store holds, and keeps what the ref names fresh.
func (store Store) agree(key string, held heldRef, archive []byte) (string, error) {
	sum := digest(archive)
	if held.Sum == sum {
		if _, err := store.PutBlob(archive); err != nil {
			return "", err
		}
		return sum, store.refreshRef(key, held)
	}
	theirs, err := store.heldBlob(held.Sum)
	if errors.Is(err, ErrNotStored) {
		return store.replaceGone(key, held, archive)
	}
	if err != nil {
		return "", err
	}
	same, err := sameContent(theirs, archive)
	if err != nil {
		return "", err
	}
	if !same {
		return "", ConflictError{Key: key, Held: held.Sum, Built: sum}
	}
	return held.Sum, store.refreshRef(key, held)
}

// replaceGone points a ref whose archive the lifecycle already took at this build's: the old archive names nothing a
// runner can read, so there is nothing to compare or to keep. The new blob goes up first, then the ref is rewritten
// only over the very object read (If-Match on its ETag); a ref another builder wrote meanwhile is read again and must
// name this archive, or this build is refused.
func (store Store) replaceGone(key string, held heldRef, archive []byte) (string, error) {
	sum, err := store.PutBlob(archive)
	if err != nil {
		return "", err
	}
	store.wrote()
	err = store.Bucket.Put("refs/action/"+key, []byte(sum), r2.PutOptions{ContentType: "text/plain", CacheControl: "no-cache", IfMatch: held.Object.ETag})
	if !errors.Is(err, r2.ErrChanged) {
		return sum, err
	}
	again, err := store.heldRef(key)
	if err != nil {
		return "", err
	}
	if again.Sum != sum {
		return "", ConflictError{Key: key, Held: again.Sum, Built: sum}
	}
	return sum, nil
}

// Stored reports whether the bucket holds key's product, the sha256 of the archive its ref names, and whether that
// archive is there. A held product is kept fresh, never rebuilt: its blob, uploaded more than FreshFor ago, is read
// and sent again, the same bytes, and so is its ref. Only a ref whose blob the store no longer holds isn't stored, and
// the caller says so when it builds again.
func (store Store) Stored(key string) (string, bool, error) {
	held, err := store.heldRef(key)
	if err != nil || held.Sum == "" {
		return "", false, err
	}
	store.read()
	object, err := store.Bucket.Head("blobs/" + held.Sum)
	if errors.Is(err, r2.ErrNotFound) {
		return held.Sum, false, nil
	}
	if err != nil {
		return "", false, err
	}
	if store.stale(object.Modified) {
		if _, err = store.heldBlob(held.Sum); err != nil {
			return "", false, err
		}
	}
	return held.Sum, true, store.refreshRef(key, held)
}

// FetchProduct writes a product's files under directory (a runner's ADAMIC_BUILD_CACHE_DIR): its ref, then its
// archive, checked against its hash and unpacked into a scratch directory, where every entry must be a buildcache
// path (<key>/<file> or <key>.inputs). Only when the whole archive checks is each product renamed into place whole,
// as buildcache itself publishes one, so a poisoned store leaves nothing behind. A product already in the cache is
// left as it is, since its key says what it holds.
func (store Store) FetchProduct(fetchContext context.Context, key, directory string) error {
	sum, err := store.Ref(fetchContext, key)
	if err != nil {
		return err
	}
	archive, err := store.blob(fetchContext, sum)
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
	if err = Unpack(bytes.NewReader(archive), scratch, buildcachePath); err != nil {
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
