// Package r2test is R2 in memory for tests, never the real one: its S3 interface at /<bucket>/<key> honors HEAD,
// GET (each with an ETag, the body's MD5 quoted), PUT with If-None-Match: * or If-Match: <ETag>, ListObjectsV2
// and GetBucketLifecycleConfiguration, stamps every write's Last-Modified from its own clock, and refuses
// (and fails the test on) any request whose Signature Version 4 Authorization isn't well formed, for the right key
// id, over the body it carries. The bucket's public domain is /public/<key>, read (GET or HEAD) with no signature.
package r2test

import (
	"compress/gzip"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/system-inc/loom/r2"
)

// A Fake is one bucket. Now is its clock (nil means time.Now), PageSize how many keys a listing page holds,
// Refused hears each badly signed request (nil means the test fails), and Before, when set, runs before each signed
// request on a key is answered, so a test can race another writer in. Answer, when set and it returns a status, is
// what the fake answers instead, for a test that needs the service to fail or slow it down. Lifecycle, when set, is the
// bucket's lifecycle configuration as GetBucketLifecycleConfiguration answers it (empty: the bucket has none), and
// LifecycleStatus, when set, the status that call answers instead (a key that may not read it gets 403).
type Fake struct {
	Server      *httptest.Server
	Credentials r2.Credentials
	Name        string
	Now         func() time.Time
	PageSize    int
	Refused     func(problem string)
	Before      func(method, key string)
	Answer      func(method, key string) int

	Lifecycle       string
	LifecycleStatus int

	t        testing.TB
	mutex    sync.Mutex
	objects  map[string]object
	requests []string
}

type object struct {
	body         []byte
	modified     time.Time
	cacheControl string
}

// New starts a fake bucket, loom-artifacts, closed when the test ends.
func New(t testing.TB) *Fake {
	fake := &Fake{
		Credentials: r2.Credentials{AccountId: "0123456789abcdef0123456789abcdef", AccessKeyId: "fake-key-id", SecretAccessKey: "fake-secret"},
		Name:        "loom-artifacts",
		PageSize:    1000,
		t:           t,
		objects:     map[string]object{},
	}
	fake.Server = httptest.NewServer(fake)
	t.Cleanup(fake.Server.Close)
	return fake
}

// Bucket is a client for the fake, signing with its key pair.
func (fake *Fake) Bucket() r2.Bucket {
	return r2.Bucket{Endpoint: fake.Server.URL, Name: fake.Name, Credentials: fake.Credentials, Client: fake.Server.Client()}
}

// Public is the fake's public domain, what artifacts.loom.system.inc is to the real bucket.
func (fake *Fake) Public() string {
	return fake.Server.URL + "/public"
}

func (fake *Fake) now() time.Time {
	if fake.Now != nil {
		return fake.Now()
	}
	return time.Now()
}

// Object is what the bucket holds at key.
func (fake *Fake) Object(key string) ([]byte, bool) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	held, found := fake.objects[key]
	return held.body, found
}

// Modified is when key was last written.
func (fake *Fake) Modified(key string) time.Time {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return fake.objects[key].modified
}

// CacheControl is the Cache-Control key was written with.
func (fake *Fake) CacheControl(key string) string {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return fake.objects[key].cacheControl
}

// Set writes key directly, as of modified, for a test that plants, poisons or ages what the bucket holds.
func (fake *Fake) Set(key string, body []byte, modified time.Time) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.objects[key] = object{body: body, modified: modified}
}

// Delete removes key, as the lifecycle would.
func (fake *Fake) Delete(key string) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	delete(fake.objects, key)
}

// Keys are every key the bucket holds under prefix, sorted.
func (fake *Fake) Keys(prefix string) []string {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	keys := []string{}
	for key := range fake.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// Requests are the requests the fake took, "<method> <key>", in order: a listing as "LIST <prefix>", and a read
// of the public domain as "PUBLIC <key>".
func (fake *Fake) Requests() []string {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return append([]string{}, fake.requests...)
}

// Count is how many of the S3 requests were method on a key under prefix.
func (fake *Fake) Count(method, prefix string) int {
	count := 0
	for _, request := range fake.Requests() {
		if strings.HasPrefix(request, method+" "+prefix) {
			count++
		}
	}
	return count
}

// ResetRequests forgets the requests taken so far.
func (fake *Fake) ResetRequests() {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.requests = nil
}

var authorizationPattern = regexp.MustCompile(`^AWS4-HMAC-SHA256 Credential=([^/ ]+)/([0-9]{8})/auto/s3/aws4_request, SignedHeaders=([a-z0-9;-]+), Signature=([0-9a-f]{64})$`)

// check is the signature's verdict on a request: empty when it is well formed, for this key id, over this body,
// signing every header that changes what the request does, and the signature is the one the key pair gives.
func (fake *Fake) check(request *http.Request, body []byte) string {
	match := authorizationPattern.FindStringSubmatch(request.Header.Get("Authorization"))
	if match == nil {
		return fmt.Sprintf("Authorization %q isn't Signature Version 4 for region auto and service s3", request.Header.Get("Authorization"))
	}
	if match[1] != fake.Credentials.AccessKeyId {
		return fmt.Sprintf("signed by key %q, not %q", match[1], fake.Credentials.AccessKeyId)
	}
	stamp := request.Header.Get("X-Amz-Date")
	at, err := time.Parse("20060102T150405Z", stamp)
	if err != nil || stamp[:8] != match[2] {
		return fmt.Sprintf("X-Amz-Date %q doesn't match the scope's day %s", stamp, match[2])
	}
	sum := sha256.Sum256(body)
	if request.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) {
		return fmt.Sprintf("X-Amz-Content-Sha256 %q isn't the body's sha256", request.Header.Get("X-Amz-Content-Sha256"))
	}
	signed := map[string]bool{}
	for _, name := range strings.Split(match[3], ";") {
		signed[name] = true
	}
	for _, name := range []string{"host", "x-amz-date", "x-amz-content-sha256", "if-none-match", "content-type", "cache-control"} {
		if (name == "host" || request.Header.Get(name) != "") && !signed[name] {
			return fmt.Sprintf("%s isn't signed (SignedHeaders=%s)", name, match[3])
		}
	}
	again, _ := http.NewRequest(request.Method, "http://"+request.Host+request.URL.RequestURI(), nil)
	for name := range signed {
		if name != "host" {
			again.Header[http.CanonicalHeaderKey(name)] = request.Header.Values(name)
		}
	}
	r2.Sign(again, request.Header.Get("X-Amz-Content-Sha256"), fake.Credentials, at)
	if again.Header.Get("Authorization") != request.Header.Get("Authorization") {
		return "the signature isn't the key pair's for this request"
	}
	return ""
}

func (fake *Fake) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	body, _ := io.ReadAll(request.Body)
	if key, public := strings.CutPrefix(request.URL.Path, "/public/"); public {
		fake.mutex.Lock()
		fake.requests = append(fake.requests, "PUBLIC "+key)
		held, found := fake.objects[key]
		fake.mutex.Unlock()
		if (request.Method != http.MethodGet && request.Method != http.MethodHead) || !found {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Length", strconv.Itoa(len(held.body)))
		if request.Method == http.MethodGet {
			writer.Write(held.body)
		}
		return
	}
	if problem := fake.check(request, body); problem != "" {
		if fake.Refused != nil {
			fake.Refused(problem)
		} else {
			fake.t.Errorf("r2test: %s %s: %s", request.Method, request.URL.Path, problem)
		}
		fail(writer, http.StatusForbidden, "SignatureDoesNotMatch", problem)
		return
	}
	path := strings.TrimPrefix(request.URL.Path, "/")
	if path == fake.Name && request.Method == http.MethodGet && request.URL.Query().Get("list-type") == "2" {
		fake.list(writer, request)
		return
	}
	if path == fake.Name && request.Method == http.MethodGet && request.URL.Query().Has("lifecycle") {
		fake.mutex.Lock()
		fake.requests = append(fake.requests, "LIFECYCLE")
		fake.mutex.Unlock()
		switch {
		case fake.LifecycleStatus != 0:
			fail(writer, fake.LifecycleStatus, "AccessDenied", "lifecycle")
		case fake.Lifecycle == "":
			fail(writer, http.StatusNotFound, "NoSuchLifecycleConfiguration", "lifecycle")
		default:
			writer.Header().Set("Content-Type", "application/xml")
			io.WriteString(writer, fake.Lifecycle)
		}
		return
	}
	key, inBucket := strings.CutPrefix(path, fake.Name+"/")
	if !inBucket || key == "" {
		fail(writer, http.StatusNotFound, "NoSuchBucket", path)
		return
	}
	if fake.Before != nil {
		fake.Before(request.Method, key)
	}
	if fake.Answer != nil {
		if status := fake.Answer(request.Method, key); status != 0 {
			fake.mutex.Lock()
			fake.requests = append(fake.requests, request.Method+" "+key)
			fake.mutex.Unlock()
			fail(writer, status, "Answered", key)
			return
		}
	}
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.requests = append(fake.requests, request.Method+" "+key)
	held, found := fake.objects[key]
	switch request.Method {
	case http.MethodHead, http.MethodGet:
		if !found {
			fail(writer, http.StatusNotFound, "NoSuchKey", key)
			return
		}
		writer.Header().Set("Last-Modified", held.modified.UTC().Format(http.TimeFormat))
		// As R2 does, a GET that takes gzip gets a large object compressed, under a weak ETag.
		if request.Method == http.MethodGet && len(held.body) >= CompressedFrom && strings.Contains(request.Header.Get("Accept-Encoding"), "gzip") {
			writer.Header().Set("Content-Encoding", "gzip")
			writer.Header().Set("ETag", "W/"+etag(held.body))
			compressor := gzip.NewWriter(writer)
			compressor.Write(held.body)
			compressor.Close()
			return
		}
		writer.Header().Set("Content-Length", strconv.Itoa(len(held.body)))
		writer.Header().Set("ETag", etag(held.body))
		if request.Method == http.MethodGet {
			writer.Write(held.body)
		}
	case http.MethodPut:
		switch match := request.Header.Get("If-None-Match"); {
		case match == "*" && found:
			fail(writer, http.StatusPreconditionFailed, "PreconditionFailed", key)
			return
		case match != "" && match != "*":
			fail(writer, http.StatusNotImplemented, "NotImplemented", "If-None-Match "+match)
			return
		}
		if match := request.Header.Get("If-Match"); match != "" && (!found || (match != "*" && match != etag(held.body))) {
			fail(writer, http.StatusPreconditionFailed, "PreconditionFailed", key)
			return
		}
		fake.objects[key] = object{body: body, modified: fake.now(), cacheControl: request.Header.Get("Cache-Control")}
		writer.WriteHeader(http.StatusOK)
	default:
		fail(writer, http.StatusMethodNotAllowed, "MethodNotAllowed", request.Method)
	}
}

// CompressedFrom is the size from which a GET that takes gzip is answered compressed, as R2 answered a 148 KB tree
// index on Oct 10 (a 3-byte object came back whole).
const CompressedFrom = 1024

// etag is an object's ETag as R2 gives one for a single put: its MD5, quoted.
func etag(body []byte) string {
	sum := md5.Sum(body)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// list answers one ListObjectsV2 page, keys in order, PageSize at most, its continuation token the last key given.
func (fake *Fake) list(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	prefix, after := query.Get("prefix"), query.Get("continuation-token")
	fake.mutex.Lock()
	fake.requests = append(fake.requests, "LIST "+prefix)
	fake.mutex.Unlock()
	type content struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		Size         int    `xml:"Size"`
	}
	page := struct {
		XMLName               xml.Name  `xml:"ListBucketResult"`
		Contents              []content `xml:"Contents"`
		IsTruncated           bool      `xml:"IsTruncated"`
		NextContinuationToken string    `xml:"NextContinuationToken,omitempty"`
	}{}
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	keys := []string{}
	for key := range fake.objects {
		if strings.HasPrefix(key, prefix) && key > after {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) > fake.PageSize {
		keys, page.IsTruncated = keys[:fake.PageSize], true
		page.NextContinuationToken = keys[len(keys)-1]
	}
	for _, key := range keys {
		held := fake.objects[key]
		page.Contents = append(page.Contents, content{Key: key, LastModified: held.modified.UTC().Format("2006-01-02T15:04:05.000Z"), Size: len(held.body)})
	}
	writer.Header().Set("Content-Type", "application/xml")
	encoded, _ := xml.Marshal(page)
	writer.Write(encoded)
}

func fail(writer http.ResponseWriter, status int, code, message string) {
	writer.Header().Set("Content-Type", "application/xml")
	writer.WriteHeader(status)
	fmt.Fprintf(writer, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, message)
}
