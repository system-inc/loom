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

// clocked is a builder's store on a fake bucket whose clock and the builder's both read *now.
func clocked(t *testing.T, now *time.Time) (*r2test.Fake, Store) {
	fake, store := serve(t)
	fake.Now = func() time.Time { return *now }
	store.Now = func() time.Time { return *now }
	return fake, store
}

// A blob the bucket holds from within FreshFor isn't sent again; one older is, which starts its 7 days over.
func TestABlobIsSkippedOnlyWhileItIsFresh(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	fake, store := clocked(t, &now)
	content := []byte("a product's archive")
	sum, err := store.PutBlob(content)
	if err != nil || fake.Count("PUT", "blobs/") != 1 || !fake.Modified("blobs/"+sum).Equal(now) {
		t.Fatalf("a new blob: %v %v", err, fake.Requests())
	}
	now = now.Add(4 * 24 * time.Hour)
	if _, err = store.PutBlob(content); err != nil || fake.Count("PUT", "blobs/") != 1 {
		t.Fatalf("a blob 4 days old was sent again: %v %v", err, fake.Requests())
	}
	now = now.Add(26 * time.Hour)
	if _, err = store.PutBlob(content); err != nil || fake.Count("PUT", "blobs/") != 2 || !fake.Modified("blobs/"+sum).Equal(now) {
		t.Fatalf("a blob 5 days and 2 hours old wasn't sent again: %v %v", err, fake.Requests())
	}
	fake.Delete("blobs/" + sum)
	if _, err = store.PutBlob(content); err != nil || fake.Count("PUT", "blobs/") != 3 {
		t.Fatalf("a blob the lifecycle took wasn't sent again: %v %v", err, fake.Requests())
	}
}

// A ref is only ever written after its blob is fresh: a product whose archive went up six days ago (an earlier
// build's, the same bytes) gets its blob sent again before the ref that names it.
func TestAFreshRefNeverNamesABlobAboutToExpire(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	fake, store := clocked(t, &now)
	archive := tarGzip(t, entry{name: keyOf("p") + "/tool", body: "tool"})
	key := keyOf("p")
	fake.Set("blobs/"+digest(archive), archive, now.Add(-6*24*time.Hour))
	if sum, err := store.Publish(key, archive); err != nil || sum != digest(archive) {
		t.Fatalf("%s %v", sum, err)
	}
	requests := fake.Requests()
	blob, ref := slices.Index(requests, "PUT blobs/"+digest(archive)), slices.Index(requests, "PUT refs/action/"+key)
	if blob < 0 || ref < 0 || blob > ref {
		t.Fatalf("the blob went up after its ref, or not at all: %v", requests)
	}
	if !fake.Modified("blobs/" + digest(archive)).Equal(now) {
		t.Fatalf("the blob's clock wasn't reset: %v", fake.Modified("blobs/"+digest(archive)))
	}
	// A ref already held for the same archive is found, and it and its blob, both now past FreshFor, are written
	// again with their own bytes, the ref only over the object read (If-Match), blob first.
	now = now.Add(5*24*time.Hour + time.Minute)
	fake.ResetRequests()
	if _, err := store.Publish(key, archive); err != nil {
		t.Fatal(err)
	}
	requests = fake.Requests()
	blob, ref = slices.Index(requests, "PUT blobs/"+digest(archive)), slices.Index(requests, "PUT refs/action/"+key)
	if blob < 0 || ref < 0 || blob > ref || !fake.Modified("refs/action/"+key).Equal(now) {
		t.Fatalf("an unchanged product past FreshFor: %v", requests)
	}
	if held, _ := fake.Object("refs/action/" + key); string(held) != digest(archive) {
		t.Fatalf("the ref was rewritten as %q", held)
	}
	// A builder holding the action, its blob and ref gone stale, calls it stored and refreshes both, the blob from its
	// own bytes: a held product is refreshed, never rebuilt.
	now = now.Add(5*24*time.Hour + time.Minute)
	fake.ResetRequests()
	if sum, stored, err := store.Stored(key); err != nil || !stored || sum != digest(archive) {
		t.Fatalf("a held product 5 days old: %s %v %v", sum, stored, err)
	}
	if fake.Count("PUT", "blobs/") != 1 || fake.Count("PUT", "refs/") != 1 || !fake.Modified("blobs/"+digest(archive)).Equal(now) || !fake.Modified("refs/action/"+key).Equal(now) {
		t.Fatalf("a stale held product wasn't refreshed: %v", fake.Requests())
	}
	// A day later nothing is written.
	now = now.Add(24 * time.Hour)
	fake.ResetRequests()
	if _, stored, err := store.Stored(key); err != nil || !stored || fake.Count("PUT", "") != 0 {
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
			fake.Set("refs/action/"+key, []byte(digest(archive)), now.Add(-6*24*time.Hour))
			fake.Before = func(method, path string) {
				if method == "PUT" && path == "refs/action/"+key {
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
	fake.Set("blobs/"+digest([]byte("archive b")), []byte("archive b"), now.Add(-6*24*time.Hour))
	fake.Set("refs/action/"+keyOf("c"), []byte(digest([]byte("archive c"))), now.Add(-6*24*time.Hour))
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
