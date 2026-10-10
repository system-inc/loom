package r2

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The signer gives the signatures of Amazon's own worked examples for S3 (Signature Version 4, "Examples:
// Signature Calculations"): a GET with a Range header, a PUT of a key holding a $, and two bucket queries.
func TestSignMatchesAmazonsWorkedExamples(t *testing.T) {
	credentials := Credentials{AccessKeyId: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name, method, address, payload string
		headers                        map[string]string
		signedHeaders, signature       string
	}{
		{"get object", "GET", "https://examplebucket.s3.amazonaws.com/test.txt", emptySha256, map[string]string{"Range": "bytes=0-9"},
			"host;range;x-amz-content-sha256;x-amz-date", "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"},
		{"put object", "PUT", "https://examplebucket.s3.amazonaws.com/test$file.text", "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
			map[string]string{"Date": "Fri, 24 May 2013 00:00:00 GMT", "X-Amz-Storage-Class": "REDUCED_REDUNDANCY"},
			"date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class", "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"},
		{"get lifecycle", "GET", "https://examplebucket.s3.amazonaws.com/?lifecycle", emptySha256, nil,
			"host;x-amz-content-sha256;x-amz-date", "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543"},
		{"list objects", "GET", "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J", emptySha256, nil,
			"host;x-amz-content-sha256;x-amz-date", "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"},
	}
	for _, example := range cases {
		request, err := http.NewRequest(example.method, example.address, nil)
		if err != nil {
			t.Fatal(err)
		}
		for name, value := range example.headers {
			request.Header.Set(name, value)
		}
		sign(request, example.payload, credentials, at, "us-east-1")
		want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=" + example.signedHeaders + ", Signature=" + example.signature
		if got := request.Header.Get("Authorization"); got != want {
			t.Errorf("%s:\n got %s\nwant %s", example.name, got, want)
		}
	}
	// R2's own scope is region auto.
	request, _ := http.NewRequest("GET", "https://account.r2.cloudflarestorage.com/loom-artifacts/blobs/x", nil)
	Sign(request, emptySha256, credentials, at)
	if !strings.Contains(request.Header.Get("Authorization"), "/20130524/auto/s3/aws4_request, ") {
		t.Fatalf("R2's scope: %s", request.Header.Get("Authorization"))
	}
}

// The credentials file is upload.sh's: key = value lines, the last of a key winning, comments skipped, and all three
// keys required.
func TestReadCredentialsReadsUploadShsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r2-releases.conf")
	os.WriteFile(path, []byte("# loom-artifacts only\naccount_id = old\naccount_id=acct0123\n  access_key_id =  key0123  \nsecret_access_key = s3cr=t\n"), 0o600)
	credentials, err := ReadCredentials(path)
	if err != nil || credentials != (Credentials{AccountId: "acct0123", AccessKeyId: "key0123", SecretAccessKey: "s3cr=t"}) {
		t.Fatalf("%+v %v", credentials, err)
	}
	if _, err = Open(credentials, "loom-artifacts"); err == nil || !strings.Contains(err.Error(), "account id") {
		t.Fatalf("an account id that isn't one: %v", err)
	}
	// Anything but 32 lowercase hex digits could steer the host a request is signed for.
	for _, account := range []string{"evil.example.com/x", strings.Repeat("A", 32), strings.Repeat("a", 31), strings.Repeat("a", 32) + ".attacker"} {
		if _, err = Open(Credentials{AccountId: account}, "loom-artifacts"); err == nil {
			t.Errorf("account id %q opened", account)
		}
	}
	credentials.AccountId = "0123456789abcdef0123456789abcdef"
	// A name that isn't a plain bucket name would move every key under another prefix, past Put's guard.
	for _, name := range []string{"loom-artifacts/releases", "loom-artifacts/", "Loom-Artifacts", "loom_artifacts", "-loom", "lo", "", "loom-artifacts?x=1", "../loom"} {
		if _, err = Open(credentials, name); err == nil {
			t.Errorf("bucket %q opened", name)
		}
	}
	if bucket, err := Open(credentials, "loom-artifacts"); err != nil || bucket.Endpoint != "https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com" || bucket.Name != "loom-artifacts" {
		t.Fatalf("%+v %v", bucket, err)
	}
	os.WriteFile(path, []byte("account_id = acct0123\naccess_key_id = key0123\n"), 0o600)
	if _, err = ReadCredentials(path); err == nil || !strings.Contains(err.Error(), "secret_access_key") {
		t.Fatalf("a file with no secret: %v", err)
	}
}

// Put writes only the action store's prefixes, so this key can never write releases/, and sends nothing for another.
func TestPutWritesOnlyTheActionStore(t *testing.T) {
	sent := 0
	bucket := Bucket{Endpoint: "http://127.0.0.1:1", Name: "loom-artifacts", Client: &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		sent++
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
	})}}
	for _, key := range []string{"releases/current.txt", "releases/blobs/abc", "blobs/", "refs/build/abc", "refs/actionx/abc", "trees", "blobs/../releases/current.txt", "manifests/x.txt"} {
		if err := bucket.Put(key, []byte("x"), PutOptions{}); err == nil {
			t.Errorf("%s was written", key)
		}
	}
	if sent != 0 {
		t.Fatalf("%d refused puts were sent", sent)
	}
	for _, key := range []string{"blobs/abc", "refs/action/abc", "trees/abc.json"} {
		if err := bucket.Put(key, []byte("x"), PutOptions{}); err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (trip roundTrip) RoundTrip(request *http.Request) (*http.Response, error) { return trip(request) }

// A 429, like a 5xx, is asked again; a 4xx that isn't is answered at once.
func TestTooManyRequestsIsAskedAgain(t *testing.T) {
	answers := []int{429, 200}
	asked := 0
	bucket := Bucket{Endpoint: "http://127.0.0.1:1", Name: "loom-artifacts", Client: &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		status := answers[min(asked, len(answers)-1)]
		asked++
		return &http.Response{StatusCode: status, Body: http.NoBody, Header: http.Header{"Last-Modified": {"Sat, 10 Oct 2026 12:00:00 GMT"}}}, nil
	})}}
	if _, err := bucket.Get("blobs/abc"); err != nil || asked != 2 {
		t.Fatalf("a 429 then a 200: %v after %d asks", err, asked)
	}
	answers, asked = []int{403}, 0
	if _, err := bucket.Get("blobs/abc"); err == nil || asked != 1 {
		t.Fatalf("a 403: %v after %d asks", err, asked)
	}
}
