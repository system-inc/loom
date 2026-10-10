package builder

import (
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
	// A ref already held for the same archive is found: only its blob is kept fresh, and the ref isn't written.
	now = now.Add(5*24*time.Hour + time.Minute)
	fake.ResetRequests()
	if _, err := store.Publish(key, archive); err != nil {
		t.Fatal(err)
	}
	if fake.Count("PUT", "refs/") != 0 || fake.Count("PUT", "blobs/") != 1 {
		t.Fatalf("an unchanged product: %v", fake.Requests())
	}
	// And a builder holding the action, with its blob gone stale, builds it again rather than calling it stored.
	now = now.Add(5*24*time.Hour + time.Minute)
	if _, stored, err := store.Stored(key); err != nil || stored {
		t.Fatalf("a ref whose blob is 5 days old is stored: %v", err)
	}
	now = now.Add(-4 * 24 * time.Hour)
	if _, stored, err := store.Stored(key); err != nil || !stored {
		t.Fatalf("a ref whose blob is a day old isn't stored: %v", err)
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
