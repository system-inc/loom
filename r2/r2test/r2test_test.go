package r2test

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/system-inc/loom/r2"
)

// A bucket reads what it wrote, a conditional put never replaces, a listing walks its pages, and every request
// the fake takes is signed for its key.
func TestABucketWritesReadsAndListsThroughTheS3Interface(t *testing.T) {
	fake := New(t)
	fake.PageSize = 2
	written := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	fake.Now = func() time.Time { return written }
	bucket := fake.Bucket()
	if _, err := bucket.Head("blobs/a"); !errors.Is(err, r2.ErrNotFound) {
		t.Fatalf("a missing key: %v", err)
	}
	if _, err := bucket.Get("blobs/a"); !errors.Is(err, r2.ErrNotFound) {
		t.Fatalf("a missing key: %v", err)
	}
	if err := bucket.Put("blobs/a", []byte("first"), r2.PutOptions{CacheControl: "public, max-age=31536000, immutable", ContentType: "application/octet-stream"}); err != nil {
		t.Fatal(err)
	}
	if object, err := bucket.Head("blobs/a"); err != nil || !object.Modified.Equal(written) || object.Size != 5 {
		t.Fatalf("%+v %v", object, err)
	}
	if content, err := bucket.Get("blobs/a"); err != nil || string(content) != "first" {
		t.Fatalf("%q %v", content, err)
	}
	if fake.CacheControl("blobs/a") != "public, max-age=31536000, immutable" {
		t.Fatalf("cache control %q", fake.CacheControl("blobs/a"))
	}
	if err := bucket.Put("refs/action/k", []byte("one"), r2.PutOptions{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
	if err := bucket.Put("refs/action/k", []byte("two"), r2.PutOptions{IfNoneMatch: true}); !errors.Is(err, r2.ErrExists) {
		t.Fatalf("a second conditional put: %v", err)
	}
	if content, _ := fake.Object("refs/action/k"); string(content) != "one" {
		t.Fatalf("a conditional put replaced %q", content)
	}
	// If-Match writes only over the ETag it read.
	_, read, err := bucket.GetObject("refs/action/k")
	if err != nil || read.ETag == "" {
		t.Fatalf("%+v %v", read, err)
	}
	if err = bucket.Put("refs/action/k", []byte("one"), r2.PutOptions{IfMatch: read.ETag}); err != nil {
		t.Fatalf("a put over the ETag read: %v", err)
	}
	fake.Set("refs/action/k", []byte("someone else's"), written)
	if err = bucket.Put("refs/action/k", []byte("one"), r2.PutOptions{IfMatch: read.ETag}); !errors.Is(err, r2.ErrChanged) {
		t.Fatalf("a put over another ETag: %v", err)
	}
	if err = bucket.Put("refs/action/gone", []byte("one"), r2.PutOptions{IfMatch: read.ETag}); !errors.Is(err, r2.ErrChanged) {
		t.Fatalf("a put over nothing: %v", err)
	}
	// A large object is read whole, under its strong ETag, so a put over it matches: R2 compresses a GET that takes
	// gzip and weakens the ETag, which once failed every replacement of a tree's index. Mutant: no Accept-Encoding.
	large := bytes.Repeat([]byte("index "), CompressedFrom)
	fake.Set("trees/large.json", large, written)
	content, read, err := bucket.GetObject("trees/large.json")
	if err != nil || !bytes.Equal(content, large) || strings.HasPrefix(read.ETag, "W/") {
		t.Fatalf("a large object read as %d bytes under %q: %v", len(content), read.ETag, err)
	}
	if err = bucket.Put("trees/large.json", large, r2.PutOptions{IfMatch: read.ETag}); err != nil {
		t.Fatalf("a put over a large object's ETag: %v", err)
	}
	for index := range 5 {
		bucket.Put(fmt.Sprintf("blobs/%c", 'b'+index), []byte("x"), r2.PutOptions{})
	}
	objects, err := bucket.List("blobs/")
	if err != nil || len(objects) != 6 || objects[0].Key != "blobs/a" || objects[5].Key != "blobs/f" || !objects[0].Modified.Equal(written) {
		t.Fatalf("%+v %v", objects, err)
	}
	if fake.Count("LIST", "blobs/") != 3 {
		t.Fatalf("a listing of 6 in pages of 2 took %v", fake.Requests())
	}
}

// The fake refuses a request signed by another key, with another secret, or over other bytes than it carries.
func TestTheFakeRefusesWhatIsntSignedForItsKey(t *testing.T) {
	fake := New(t)
	var mutex sync.Mutex
	refused := []string{}
	fake.Refused = func(problem string) {
		mutex.Lock()
		defer mutex.Unlock()
		refused = append(refused, problem)
	}
	other := fake.Bucket()
	other.Credentials.AccessKeyId = "someone-else"
	if err := other.Put("blobs/a", []byte("x"), r2.PutOptions{}); err == nil {
		t.Fatal("another key id wrote")
	}
	wrong := fake.Bucket()
	wrong.Credentials.SecretAccessKey = "guessed"
	if err := wrong.Put("blobs/a", []byte("x"), r2.PutOptions{}); err == nil {
		t.Fatal("another secret wrote")
	}
	// A header added after signing, and a body other than the one signed.
	request, _ := http.NewRequest(http.MethodPut, fake.Server.URL+"/loom-artifacts/blobs/a", strings.NewReader("other"))
	r2.Sign(request, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", fake.Credentials, time.Now())
	if response, err := fake.Server.Client().Do(request); err != nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("a body other than the signed one: %v %v", response, err)
	}
	request, _ = http.NewRequest(http.MethodPut, fake.Server.URL+"/loom-artifacts/blobs/a", nil)
	r2.Sign(request, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", fake.Credentials, time.Now())
	request.Header.Set("If-None-Match", "*")
	if response, err := fake.Server.Client().Do(request); err != nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("an unsigned If-None-Match: %v %v", response, err)
	}
	if _, held := fake.Object("blobs/a"); held || len(refused) != 4 {
		t.Fatalf("refused %v", refused)
	}
}

// Lifecycle reads every enabled rule that expires keys, with its prefix in each of the forms S3 writes it: none for a
// bucket with no lifecycle, and ErrLifecycleUnreadable for a key that may not read it. A gate inputs publisher refuses
// a bucket on its answer (gateinputs.HomeExpires), so a rule read wrong is a home that silently expires.
func TestLifecycleReadsEveryExpiringRuleAndItsPrefix(t *testing.T) {
	fake := New(t)
	bucket := fake.Bucket()
	if rules, err := bucket.Lifecycle(); err != nil || len(rules) != 0 {
		t.Fatalf("no lifecycle: %v, %v", rules, err)
	}
	fake.Lifecycle = `<LifecycleConfiguration>` +
		`<Rule><ID>blobs</ID><Status>Enabled</Status><Filter><Prefix>blobs/</Prefix></Filter><Expiration><Days>7</Days></Expiration></Rule>` +
		`<Rule><ID>trees</ID><Status>Enabled</Status><Filter><And><Prefix>trees/</Prefix></And></Filter><Expiration><Days>7</Days></Expiration></Rule>` +
		`<Rule><ID>refs</ID><Status>Enabled</Status><Prefix>refs/</Prefix><Expiration><Days>7</Days></Expiration></Rule>` +
		`<Rule><ID>everything</ID><Status>Enabled</Status><Filter></Filter><Expiration><Date>2027-01-01T00:00:00Z</Date></Expiration></Rule>` +
		`<Rule><ID>off</ID><Status>Disabled</Status><Filter><Prefix>gate-inputs/</Prefix></Filter><Expiration><Days>1</Days></Expiration></Rule>` +
		`<Rule><ID>uploads</ID><Status>Enabled</Status><Filter><Prefix>gate-inputs/</Prefix></Filter><AbortIncompleteMultipartUpload><DaysAfterInitiation>1</DaysAfterInitiation></AbortIncompleteMultipartUpload></Rule>` +
		`</LifecycleConfiguration>`
	rules, err := bucket.Lifecycle()
	if err != nil {
		t.Fatal(err)
	}
	read := []string{}
	for _, rule := range rules {
		read = append(read, fmt.Sprintf("%s:%q:%d:%s", rule.Id, rule.Prefix, rule.Days, rule.Date))
	}
	want := `blobs:"blobs/":7: trees:"trees/":7: refs:"refs/":7: everything:"":0:2027-01-01T00:00:00Z`
	if strings.Join(read, " ") != want {
		t.Fatalf("read %s, want %s", strings.Join(read, " "), want)
	}
	fake.LifecycleStatus = http.StatusForbidden
	if _, err := bucket.Lifecycle(); !errors.Is(err, r2.ErrLifecycleUnreadable) {
		t.Fatalf("a key that may not read it: %v", err)
	}
}
