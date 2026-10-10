package builder

import (
	"bytes"
	"compress/gzip"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/r2/r2test"
)

// pastFresh is an age a day past FreshFor and still within Lifecycle: a blob or ref this old is held, and must be
// written again before anything relies on it.
const pastFresh = FreshFor + 24*time.Hour

// clocked is a builder's store on a fake bucket whose clock and the builder's both read *now.
func clocked(t *testing.T, now *time.Time) (*r2test.Fake, Store) {
	fake, store := serve(t)
	fake.Now = func() time.Time { return *now }
	store.Now = func() time.Time { return *now }
	return fake, store
}

// A blob the bucket holds from within FreshFor isn't sent again; one older is copied onto itself in the bucket, which
// starts its Lifecycle over and sends no byte; one whose ETag isn't these bytes' MD5, or one gone, is sent.
func TestABlobIsSkippedOnlyWhileItIsFresh(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	fake, store := clocked(t, &now)
	content := []byte("a product's archive")
	sum, err := store.PutBlob(content)
	if err != nil || fake.Count("PUT", "blobs/") != 1 || !fake.Modified("blobs/"+sum).Equal(now) {
		t.Fatalf("a new blob: %v %v", err, fake.Requests())
	}
	now = now.Add(FreshFor - 24*time.Hour)
	if _, err = store.PutBlob(content); err != nil || fake.Count("PUT", "blobs/") != 1 || fake.Count("COPY", "blobs/") != 0 {
		t.Fatalf("a blob a day short of FreshFor was sent or copied again: %v %v", err, fake.Requests())
	}
	now = now.Add(26 * time.Hour)
	if _, err = store.PutBlob(content); err != nil || fake.Count("PUT", "blobs/") != 1 || fake.Count("COPY", "blobs/") != 1 || !fake.Modified("blobs/"+sum).Equal(now) {
		t.Fatalf("a blob 2 hours past FreshFor wasn't refreshed in the bucket: %v %v", err, fake.Requests())
	}
	// Held under its name but not these bytes, as a multipart upload's ETag would be: sent, never trusted.
	fake.Set("blobs/"+sum, []byte("other bytes"), now.Add(-pastFresh))
	if _, err = store.PutBlob(content); err != nil || fake.Count("PUT", "blobs/") != 2 || fake.Count("COPY", "blobs/") != 1 {
		t.Fatalf("a held blob of other bytes wasn't sent: %v %v", err, fake.Requests())
	}
	if held, _ := fake.Object("blobs/" + sum); string(held) != string(content) {
		t.Fatalf("the blob holds %q", held)
	}
	fake.Delete("blobs/" + sum)
	if _, err = store.PutBlob(content); err != nil || fake.Count("PUT", "blobs/") != 3 {
		t.Fatalf("a blob the lifecycle took wasn't sent again: %v %v", err, fake.Requests())
	}
	// A blob the lifecycle takes between the look and the refresh is sent.
	fake.Set("blobs/"+sum, content, now.Add(-pastFresh))
	fake.Before = func(method, key string) {
		if method == "COPY" {
			fake.Delete(key)
		}
	}
	if _, err = store.PutBlob(content); err != nil || fake.Count("PUT", "blobs/") != 4 || !fake.Modified("blobs/"+sum).Equal(now) {
		t.Fatalf("a blob gone under its refresh wasn't sent again: %v %v", err, fake.Requests())
	}
}

// A product held and asked for every day is copied once in each Lifecycle, in its last days, and never reaches its
// end: FreshFor takes most of the lifecycle, so a held object costs one copy a lifecycle, not one every few days.
// Mutants that each fail it: FreshFor at 5 days (a copy every fifth day); FreshFor at Lifecycle (expired first).
func TestAHeldProductIsCopiedOnceALifecycle(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	fake, store := clocked(t, &now)
	key := keyOf("p")
	archive := tarGzip(t, entry{name: key + "/tool", body: "tool"})
	if _, err := store.Publish(key, archive); err != nil {
		t.Fatal(err)
	}
	for end := now.Add(2 * Lifecycle); now.Before(end); now = now.Add(24 * time.Hour) {
		for _, object := range []string{"blobs/" + digest(archive), "refs/action/" + key} {
			if age := now.Sub(fake.Modified(object)); age >= Lifecycle {
				t.Fatalf("%s reached %v, past Lifecycle", object, age)
			}
		}
		if sum, stored, err := store.Stored(key); err != nil || !stored || sum != digest(archive) {
			t.Fatalf("a held product on %v: %s %v %v", now, sum, stored, err)
		}
	}
	if blobs, refs := fake.Count("COPY", "blobs/"), fake.Count("COPY", "refs/"); blobs > 2 || refs > 2 || fake.Count("PUT", "") != 2 {
		t.Fatalf("two lifecycles of daily use copied the blob %d times and the ref %d, and put %d", blobs, refs, fake.Count("PUT", ""))
	}
}

// A ref is only ever written after its blob is fresh: a product whose archive went up more than FreshFor ago (an
// earlier build's, the same bytes) gets its blob refreshed before the ref that names it is written.
func TestAFreshRefNeverNamesABlobAboutToExpire(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	fake, store := clocked(t, &now)
	archive := tarGzip(t, entry{name: keyOf("p") + "/tool", body: "tool"})
	key := keyOf("p")
	fake.Set("blobs/"+digest(archive), archive, now.Add(-pastFresh))
	if sum, err := store.Publish(key, archive); err != nil || sum != digest(archive) {
		t.Fatalf("%s %v", sum, err)
	}
	requests := fake.Requests()
	blob, ref := slices.Index(requests, "COPY blobs/"+digest(archive)), slices.Index(requests, "PUT refs/action/"+key)
	if blob < 0 || ref < 0 || blob > ref {
		t.Fatalf("the blob was refreshed after its ref was written, or not at all: %v", requests)
	}
	if !fake.Modified("blobs/" + digest(archive)).Equal(now) {
		t.Fatalf("the blob's clock wasn't reset: %v", fake.Modified("blobs/"+digest(archive)))
	}
	// A ref already held for the same archive is found, and it and its blob, both now past FreshFor, are refreshed in
	// the bucket, the ref only over the object read (its ETag), blob first.
	now = now.Add(FreshFor + time.Minute)
	fake.ResetRequests()
	if _, err := store.Publish(key, archive); err != nil {
		t.Fatal(err)
	}
	requests = fake.Requests()
	blob, ref = slices.Index(requests, "COPY blobs/"+digest(archive)), slices.Index(requests, "COPY refs/action/"+key)
	if blob < 0 || ref < 0 || blob > ref || fake.Count("PUT", "") != 0 || !fake.Modified("refs/action/"+key).Equal(now) {
		t.Fatalf("an unchanged product past FreshFor: %v", requests)
	}
	if held, _ := fake.Object("refs/action/" + key); string(held) != digest(archive) {
		t.Fatalf("the ref was rewritten as %q", held)
	}
	// A builder holding the action, its blob and ref gone stale, calls it stored and refreshes both, the blob once its
	// bytes are read and checked: a held product is refreshed, never rebuilt.
	now = now.Add(FreshFor + time.Minute)
	fake.ResetRequests()
	if sum, stored, err := store.Stored(key); err != nil || !stored || sum != digest(archive) {
		t.Fatalf("a held product past FreshFor: %s %v %v", sum, stored, err)
	}
	if fake.Count("PUT", "") != 0 || fake.Count("COPY", "blobs/") != 1 || fake.Count("COPY", "refs/") != 1 || !fake.Modified("blobs/"+digest(archive)).Equal(now) || !fake.Modified("refs/action/"+key).Equal(now) {
		t.Fatalf("a stale held product wasn't refreshed: %v", fake.Requests())
	}
	// A day later nothing is written.
	now = now.Add(24 * time.Hour)
	fake.ResetRequests()
	if _, stored, err := store.Stored(key); err != nil || !stored || fake.Count("PUT", "") != 0 || fake.Count("COPY", "") != 0 {
		t.Fatalf("a fresh held product: %v %v", err, fake.Requests())
	}
	// Only a ref whose blob is gone isn't stored, so its product is built again; the builder says why.
	fake.Delete("blobs/" + digest(archive))
	if sum, stored, err := store.Stored(key); err != nil || stored || sum != digest(archive) {
		t.Fatalf("a ref whose blob is gone: %s %v %v", sum, stored, err)
	}
	runs := 0
	builder := Builder{Store: store, Scratch: t.TempDir(), Cache: t.TempDir(), Key: func(Action) (string, error) { return key, nil },
		Run: func(Action, []string) ([]byte, error) { runs++; return nil, nil }}
	if results := builder.Build([]Action{{Directory: "x", Test: "TestProduct_X"}}); runs != 1 || !strings.Contains(results[0].Note, "no longer holds") {
		t.Fatalf("a rebuild of a product whose blob is gone: %+v", results)
	}
}

// A ref another builder rewrote between our read and our refresh is read again: the same archive is fine, another
// is refused, and the If-Match keeps us from writing over it either way.
func TestARefRefreshIsWrittenOnlyOverTheRefRead(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	key := keyOf("p")
	archive := tarGzip(t, entry{name: key + "/tool", body: "tool"})
	for name, theirs := range map[string]string{"the same archive": digest(archive), "another archive": keyOf("another")} {
		t.Run(name, func(t *testing.T) {
			fake, store := clocked(t, &now)
			fake.Set("blobs/"+digest(archive), archive, now)
			fake.Set("refs/action/"+key, []byte(digest(archive)), now.Add(-pastFresh))
			fake.Before = func(method, path string) {
				if method == "COPY" && path == "refs/action/"+key {
					fake.Set(path, []byte(theirs+"\n"), now)
				}
			}
			_, _, err := store.Stored(key)
			if held, _ := fake.Object("refs/action/" + key); string(held) != theirs+"\n" {
				t.Fatalf("the other builder's ref was overwritten with %q", held)
			}
			if same := theirs == digest(archive); same != (err == nil) {
				t.Fatalf("%s: %v", name, err)
			}
		})
	}
}

// A ref never changes. One naming another archive refuses the build, naming both, and nothing goes up; a ref another
// builder writes between our read and our write is read back and held to the same rule.
func TestARefNamingAnotherArchiveIsRefusedAndNeverOverwritten(t *testing.T) {
	fake, store := serve(t)
	key := keyOf("p")
	first := tarGzip(t, entry{name: key + "/tool", body: "built on workshop"})
	second := tarGzip(t, entry{name: key + "/tool", body: "built on another day, differently"})
	if _, err := store.Publish(key, first); err != nil {
		t.Fatal(err)
	}
	fake.ResetRequests()
	_, err := store.Publish(key, second)
	var conflict ConflictError
	if !errors.As(err, &conflict) || conflict.Held != digest(first) || conflict.Built != digest(second) {
		t.Fatalf("a second archive: %v", err)
	}
	if !strings.Contains(err.Error(), digest(first)) || !strings.Contains(err.Error(), digest(second)) {
		t.Fatalf("the refusal doesn't name both: %v", err)
	}
	if ref, _ := fake.Object("refs/action/" + key); string(ref) != digest(first) || fake.Count("PUT", "") != 0 {
		t.Fatalf("a refused build wrote: %q %v", ref, fake.Requests())
	}
	// The race: the ref is empty when read, and another builder writes it before our conditional put lands.
	for name, theirs := range map[string][]byte{"another archive": first, "the same archive": second} {
		t.Run(name, func(t *testing.T) {
			fake, store := serve(t)
			fake.Before = func(method, path string) {
				if method == "PUT" && path == "refs/action/"+key {
					// Another builder puts its blob, then its ref.
					fake.Set("blobs/"+digest(theirs), theirs, time.Now())
					fake.Set(path, []byte(digest(theirs)), time.Now())
				}
			}
			_, err := store.Publish(key, second)
			if ref, _ := fake.Object("refs/action/" + key); string(ref) != digest(theirs) {
				t.Fatalf("the other builder's ref was overwritten with %q", ref)
			}
			if same := digest(theirs) == digest(second); same != (err == nil) {
				t.Fatalf("racing %s: %v", name, err)
			}
		})
	}
}

// The audit lists the bucket and reads each fresh ref: one naming a blob the store lacks, or a blob uploaded more
// than FreshFor before it, is drift; a ref near its own end is only counted.
func TestTheAuditFindsAFreshRefWhoseBlobIsGoneOrAboutToBe(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	fake, store := clocked(t, &now)
	fake.PageSize = 2
	for _, name := range []string{"a", "b", "c"} {
		if _, err := store.Publish(keyOf(name), []byte("archive "+name)); err != nil {
			t.Fatal(err)
		}
	}
	store.PutBlob([]byte("a judge's tests list"))
	if report, err := Audit(store); err != nil || report.Drift() || report.Refs != 3 || report.Blobs != 4 {
		t.Fatalf("an honest store: %+v %v", report, err)
	}
	fake.Delete("blobs/" + digest([]byte("archive a")))
	fake.Set("blobs/"+digest([]byte("archive b")), []byte("archive b"), now.Add(-pastFresh))
	fake.Set("refs/action/"+keyOf("c"), []byte(digest([]byte("archive c"))), now.Add(-pastFresh))
	fake.Delete("blobs/" + digest([]byte("archive c")))
	report, err := Audit(store)
	if err != nil || !report.Drift() || !slices.Equal(report.Dangling, []string{keyOf("a")}) || !slices.Equal(report.Stale, []string{keyOf("b")}) || report.Expiring != 1 {
		t.Fatalf("a store with drift: %+v %v", report, err)
	}
	if fake.Count("LIST", "blobs/") != 3 {
		t.Fatalf("the audit didn't walk the listing's pages: %v", fake.Requests())
	}
}

// regzip is archive's tar gzipped again at another level, as another Go's compress/flate might write it.
func regzip(t *testing.T, archive []byte, level int) []byte {
	t.Helper()
	tar, err := gunzipped(archive)
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	compressor, _ := gzip.NewWriterLevel(&buffer, level)
	compressor.Write(tar)
	compressor.Close()
	return buffer.Bytes()
}

// A conflict is other files, not other gzip bytes: an archive of the same tar under another sha256 is the held one,
// and nothing of it goes up; an archive of other files is refused.
func TestAnArchiveOfTheSameFilesGzippedOtherwiseIsTheHeldOne(t *testing.T) {
	fake, store := serve(t)
	key := keyOf("p")
	archive := tarGzip(t, entry{name: key + "/tool", body: strings.Repeat("the same tool ", 200)})
	if _, err := store.Publish(key, archive); err != nil {
		t.Fatal(err)
	}
	other := regzip(t, archive, gzip.BestSpeed)
	if digest(other) == digest(archive) {
		t.Fatal("the two gzips came out the same; the test needs them to differ")
	}
	fake.ResetRequests()
	sum, err := store.Publish(key, other)
	if err != nil || sum != digest(archive) {
		t.Fatalf("the same files gzipped otherwise: %s %v", sum, err)
	}
	if _, held := fake.Object("blobs/" + digest(other)); held || fake.Count("PUT", "") != 0 {
		t.Fatalf("the other gzip went up: %v", fake.Requests())
	}
	different := tarGzip(t, entry{name: key + "/tool", body: "another tool"})
	if _, err = store.Publish(key, different); !errors.As(err, &ConflictError{}) {
		t.Fatalf("other files: %v", err)
	}
}

// A ref whose archive the lifecycle already took names nothing, so a rebuild with other bytes (a product that isn't
// reproducible yet) takes its place: the blob first, then the ref, only over the ref read. A ref another builder
// rewrote meanwhile holds this build to it.
func TestARefWhoseBlobIsGoneIsPointedAtTheRebuild(t *testing.T) {
	key := keyOf("p")
	gone := keyOf("an archive the lifecycle took")
	rebuilt := tarGzip(t, entry{name: key + "/tool", body: "built again, other bytes"})
	fake, store := serve(t)
	fake.Set("refs/action/"+key, []byte(gone), time.Now().Add(-pastFresh))
	if sum, err := store.Publish(key, rebuilt); err != nil || sum != digest(rebuilt) {
		t.Fatalf("a rebuild over a ref whose blob is gone: %s %v", sum, err)
	}
	requests := fake.Requests()
	blob, ref := slices.Index(requests, "PUT blobs/"+digest(rebuilt)), slices.Index(requests, "PUT refs/action/"+key)
	if blob < 0 || ref < 0 || blob > ref {
		t.Fatalf("the blob went up after its ref, or not at all: %v", requests)
	}
	if held, _ := fake.Object("refs/action/" + key); string(held) != digest(rebuilt) {
		t.Fatalf("the ref holds %q", held)
	}
	for name, theirs := range map[string]string{"another archive": keyOf("a third build"), "this archive": digest(rebuilt)} {
		t.Run(name, func(t *testing.T) {
			fake, store := serve(t)
			fake.Set("refs/action/"+key, []byte(gone), time.Now())
			fake.Before = func(method, path string) {
				if method == "PUT" && path == "refs/action/"+key {
					fake.Set(path, []byte(theirs), time.Now())
				}
			}
			_, err := store.Publish(key, rebuilt)
			if held, _ := fake.Object("refs/action/" + key); string(held) != theirs {
				t.Fatalf("the other builder's ref was overwritten with %q", held)
			}
			if same := theirs == digest(rebuilt); same != (err == nil) || (!same && !errors.As(err, &ConflictError{})) {
				t.Fatalf("racing %s: %v", name, err)
			}
		})
	}
}

// A worse build that keeps a better build's index keeps it runnable: every blob it names that is past FreshFor is
// read, checked and refreshed in the bucket, and so is the index, over its ETag, none of them sent again. An index naming a blob that is gone can't be kept,
// and the worse but whole one takes its place.
func TestAKeptIndexAndEverythingItNamesAreKeptFresh(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	old := now.Add(-pastFresh)
	source, product, binary := []byte("a source chunk"), []byte("a product's archive"), []byte("a test binary")
	chunks := []SourceChunk{{Blob: digest(source), First: "a", Last: "a", Files: 1, Bytes: int64(len(source))}}
	better := TreeIndex{Format: TreeIndexFormat, Tree: "t", Source: chunks, Products: map[string]string{keyOf("p"): digest(product)},
		Packages: map[string]TreePackage{"a": {Package: "a", Binary: digest(binary), Products: []string{keyOf("p")}}, "b": {Package: "b", Binary: digest(binary)}}}
	worse := TreeIndex{Format: TreeIndexFormat, Tree: "t", Source: chunks, Products: map[string]string{},
		Packages: map[string]TreePackage{"a": {Package: "a", Error: "a conflict"}, "b": {Package: "b", Binary: digest(binary)}}}
	plant := func(t *testing.T) (*r2test.Fake, Store) {
		fake, store := clocked(t, &now)
		for _, blob := range [][]byte{source, product, binary} {
			fake.Set("blobs/"+digest(blob), blob, old)
		}
		encoded, _ := better.encode()
		fake.Set("trees/k.json", encoded, old)
		return fake, store
	}
	fake, store := plant(t)
	if written, err := store.writeIndex("k", &worse); err != nil || written {
		t.Fatalf("a worse build: written %v, %v", written, err)
	}
	for _, key := range []string{"blobs/" + digest(source), "blobs/" + digest(product), "blobs/" + digest(binary), "trees/k.json"} {
		if !fake.Modified(key).Equal(now) {
			t.Errorf("%s wasn't kept fresh: %v", key, fake.Modified(key))
		}
	}
	if content, _ := fake.Object("trees/k.json"); !strings.Contains(string(content), digest(product)) || fake.Count("PUT", "") != 0 {
		t.Fatalf("the kept index changed, or something was sent: %s %v", content, fake.Requests())
	}
	// A day later nothing needs writing.
	now = now.Add(24 * time.Hour)
	fake.ResetRequests()
	if written, err := store.writeIndex("k", &worse); err != nil || written || fake.Count("PUT", "") != 0 || fake.Count("COPY", "") != 0 {
		t.Fatalf("a fresh kept index: %v %v %v", written, err, fake.Requests())
	}
	// With the product's blob gone, the better index can't be kept.
	fake, store = plant(t)
	fake.Delete("blobs/" + digest(product))
	if written, err := store.writeIndex("k", &worse); err != nil || !written {
		t.Fatalf("an index naming a gone blob was kept: %v %v", written, err)
	}
}
