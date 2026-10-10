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
	if bucket := Open(credentials, "loom-artifacts"); bucket.Endpoint != "https://acct0123.r2.cloudflarestorage.com" || bucket.Name != "loom-artifacts" {
		t.Fatalf("%+v", bucket)
	}
	os.WriteFile(path, []byte("account_id = acct0123\naccess_key_id = key0123\n"), 0o600)
	if _, err = ReadCredentials(path); err == nil || !strings.Contains(err.Error(), "secret_access_key") {
		t.Fatalf("a file with no secret: %v", err)
	}
}
