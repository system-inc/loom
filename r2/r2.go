// Package r2 is Cloudflare R2's S3 interface, the one way loom writes the public store (loom-artifacts): every
// request goes to https://<account id>.r2.cloudflarestorage.com/<bucket>/<key>, signed with AWS Signature Version 4
// (service s3, region auto) from Go's standard library alone, so a builder needs no SDK and no Worker in its path.
// Reads by anyone go direct to the bucket's public domain instead, artifacts.loom.system.inc, never through here.
//
// The key pair comes from a key = value file, ~/.loom/r2-releases.conf by default, the same file updater/upload.sh
// reads: account_id, access_key_id and secret_access_key. That key can write anywhere in loom-artifacts, releases/
// included, so Put writes only the action store's prefixes and the gate inputs' (Writable).
package r2

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Credentials are an R2 key pair and the account it belongs to.
type Credentials struct {
	AccountId       string
	AccessKeyId     string
	SecretAccessKey string
}

// DefaultCredentialsPath is where a machine keeps its R2 key pair, as upload.sh reads it.
func DefaultCredentialsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".loom", "r2-releases.conf")
}

// ReadCredentials reads a key = value file as upload.sh does: a key's last line wins, blank lines and # comments are
// skipped, and all three of account_id, access_key_id and secret_access_key must be there.
func ReadCredentials(path string) (Credentials, error) {
	file, err := os.Open(path)
	if err != nil {
		return Credentials{}, err
	}
	defer file.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, found := strings.Cut(line, "=")
		if found {
			values[strings.TrimSpace(name)] = strings.TrimSpace(value)
		}
	}
	if err = scanner.Err(); err != nil {
		return Credentials{}, err
	}
	credentials := Credentials{AccountId: values["account_id"], AccessKeyId: values["access_key_id"], SecretAccessKey: values["secret_access_key"]}
	if credentials.AccountId == "" || credentials.AccessKeyId == "" || credentials.SecretAccessKey == "" {
		return Credentials{}, fmt.Errorf("%s needs account_id, access_key_id and secret_access_key", path)
	}
	return credentials, nil
}

// A Bucket is one R2 bucket through the S3 interface. Endpoint is https://<account id>.r2.cloudflarestorage.com
// (a test's fake otherwise), and Now signs each request (nil means time.Now).
type Bucket struct {
	Endpoint    string
	Name        string
	Credentials Credentials
	Client      *http.Client
	Now         func() time.Time
}

// accountIdPattern is a Cloudflare account id, the first label of the S3 endpoint's host.
var accountIdPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// bucketNamePattern is a plain R2 bucket name: lowercase letters, digits and hyphens, 3 to 63 of them, starting and
// ending with a letter or digit, and never a slash, which would make the rest of the name a key prefix.
var bucketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

// Open is the bucket named name on credentials' account, refusing an account id that isn't one, so nothing from the
// credentials file can steer the host a request is signed for, and a name that isn't a plain bucket name, so nothing
// from a flag can move every key under another prefix (loom-artifacts/releases would put the action store's writes
// under releases/, past Writable).
func Open(credentials Credentials, name string) (Bucket, error) {
	if !accountIdPattern.MatchString(credentials.AccountId) {
		return Bucket{}, fmt.Errorf("account_id %q isn't a Cloudflare account id, 32 lowercase hex digits", credentials.AccountId)
	}
	if !bucketNamePattern.MatchString(name) {
		return Bucket{}, fmt.Errorf("bucket %q isn't a plain bucket name: lowercase letters, digits and hyphens, no slash", name)
	}
	return Bucket{Endpoint: "https://" + credentials.AccountId + ".r2.cloudflarestorage.com", Name: name, Credentials: credentials}, nil
}

// ErrNotFound is a key the bucket doesn't hold.
var ErrNotFound = errors.New("not in the bucket")

// ErrExists is a put with IfNoneMatch refused because the key is already written.
var ErrExists = errors.New("already written")

// ErrChanged is a put with IfMatch refused because the key no longer holds that ETag.
var ErrChanged = errors.New("changed since it was read")

// Writable are the only key prefixes Put writes: the action store's (builder/store.go), and gate-inputs/, where the
// gate inputs' chunks and manifests live by sha256 (gateinputs/gateinputs.go) under no lifecycle rule. The key pair can
// write the whole bucket, releases/ included, whose current.txt every machine installs from, so a writer of the action
// store never touches anything else even through a bug.
var Writable = []string{"blobs/", "refs/action/", "trees/", "gate-inputs/"}

// An Object is one key the bucket holds: its size, its ETag, and when it was last written, which is when R2's
// lifecycle starts counting its days.
type Object struct {
	Key      string
	Size     int64
	ETag     string
	Modified time.Time
}

// PutOptions are a put's headers: IfNoneMatch writes only when the key holds nothing (If-None-Match: *), and
// IfMatch only when the key still holds that ETag (If-Match), which R2's PutObject honors.
type PutOptions struct {
	ContentType  string
	CacheControl string
	IfNoneMatch  bool
	IfMatch      string
}

// emptySha256 is the payload hash of a request with no body.
const emptySha256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func (bucket Bucket) client() *http.Client {
	if bucket.Client != nil {
		return bucket.Client
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

func (bucket Bucket) now() time.Time {
	if bucket.Now != nil {
		return bucket.Now()
	}
	return time.Now()
}

// address is the request's url for key (empty for the bucket itself) and query, escaped exactly as it is signed.
func (bucket Bucket) address(key string, query url.Values) (*url.URL, error) {
	address, err := url.Parse(strings.TrimSuffix(bucket.Endpoint, "/"))
	if err != nil {
		return nil, err
	}
	address.Path = "/" + bucket.Name
	if key != "" {
		address.Path += "/" + key
	}
	address.RawPath = escape(address.Path, false)
	address.RawQuery = canonicalQuery(query)
	return address, nil
}

// transient is an answer worth asking again: the service's own failure, or its asking us to slow down.
func transient(status int) bool {
	return status >= 500 || status == http.StatusTooManyRequests
}

// do sends one signed request, again after a network error, a 5xx or a 429, three tries in all, and returns the
// answer with its body read. Each try is signed afresh, so a retry never carries a stale date.
func (bucket Bucket) do(method, key string, query url.Values, body []byte, headers map[string]string) (*http.Response, []byte, error) {
	address, err := bucket.address(key, query)
	if err != nil {
		return nil, nil, err
	}
	payload := emptySha256
	if body != nil {
		sum := sha256.Sum256(body)
		payload = hex.EncodeToString(sum[:])
	}
	var lastError error
	for attempt := range 3 {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		request, err := http.NewRequest(method, address.String(), bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		request.ContentLength = int64(len(body))
		if body == nil {
			request.Body, request.ContentLength = nil, 0
		}
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		Sign(request, payload, bucket.Credentials, bucket.now())
		response, err := bucket.client().Do(request)
		if err != nil {
			lastError = err
			continue
		}
		answer, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			lastError = err
			continue
		}
		if transient(response.StatusCode) {
			lastError = fmt.Errorf("%s %s: %s: %s", method, key, response.Status, snippet(answer))
			continue
		}
		return response, answer, nil
	}
	return nil, nil, lastError
}

func snippet(answer []byte) string {
	return strings.TrimSpace(string(answer[:min(len(answer), 512)]))
}

// object is what an answer's headers say about key.
func object(key string, response *http.Response) (Object, error) {
	modified, err := http.ParseTime(response.Header.Get("Last-Modified"))
	if err != nil {
		return Object{}, fmt.Errorf("%s: Last-Modified %q: %w", key, response.Header.Get("Last-Modified"), err)
	}
	return Object{Key: key, Size: response.ContentLength, ETag: response.Header.Get("ETag"), Modified: modified}, nil
}

// Head is what the bucket holds at key, or ErrNotFound.
func (bucket Bucket) Head(key string) (Object, error) {
	response, answer, err := bucket.do(http.MethodHead, key, nil, nil, nil)
	if err != nil {
		return Object{}, err
	}
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return Object{}, ErrNotFound
	default:
		return Object{}, fmt.Errorf("HEAD %s: %s: %s", key, response.Status, snippet(answer))
	}
	return object(key, response)
}

// Get reads key's bytes, or ErrNotFound.
func (bucket Bucket) Get(key string) ([]byte, error) {
	content, _, err := bucket.GetObject(key)
	return content, err
}

// GetObject reads key's bytes and what the bucket says about them, or ErrNotFound.
func (bucket Bucket) GetObject(key string) ([]byte, Object, error) {
	response, answer, err := bucket.do(http.MethodGet, key, nil, nil, nil)
	if err != nil {
		return nil, Object{}, err
	}
	switch response.StatusCode {
	case http.StatusOK:
		held, err := object(key, response)
		held.Size = int64(len(answer))
		return answer, held, err
	case http.StatusNotFound:
		return nil, Object{}, ErrNotFound
	}
	return nil, Object{}, fmt.Errorf("GET %s: %s: %s", key, response.Status, snippet(answer))
}

// Put writes body at key, which must be under one of Writable. With IfNoneMatch, a key that already holds anything
// is ErrExists; with IfMatch, a key that no longer holds that ETag is ErrChanged; either way it is left as it is.
func (bucket Bucket) Put(key string, body []byte, options PutOptions) error {
	writable := false
	for _, prefix := range Writable {
		writable = writable || (strings.HasPrefix(key, prefix) && len(key) > len(prefix))
	}
	if !writable || strings.Contains(key, "..") {
		return fmt.Errorf("PUT %s: the action store writes only under %s", key, strings.Join(Writable, ", "))
	}
	headers := map[string]string{}
	if options.ContentType != "" {
		headers["Content-Type"] = options.ContentType
	}
	if options.CacheControl != "" {
		headers["Cache-Control"] = options.CacheControl
	}
	if options.IfNoneMatch {
		headers["If-None-Match"] = "*"
	}
	if options.IfMatch != "" {
		headers["If-Match"] = options.IfMatch
	}
	if body == nil {
		body = []byte{}
	}
	response, answer, err := bucket.do(http.MethodPut, key, nil, body, headers)
	if err != nil {
		return err
	}
	switch {
	case response.StatusCode == http.StatusOK || response.StatusCode == http.StatusCreated:
		return nil
	case response.StatusCode == http.StatusPreconditionFailed && options.IfNoneMatch:
		return ErrExists
	case response.StatusCode == http.StatusPreconditionFailed && options.IfMatch != "":
		return ErrChanged
	}
	return fmt.Errorf("PUT %s: %s: %s", key, response.Status, snippet(answer))
}

// listPage is one ListObjectsV2 answer.
type listPage struct {
	Contents []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

// List is every key under prefix, page by page (ListObjectsV2), in the bucket's order.
func (bucket Bucket) List(prefix string) ([]Object, error) {
	objects := []Object{}
	token := ""
	for {
		query := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			query.Set("continuation-token", token)
		}
		response, answer, err := bucket.do(http.MethodGet, "", query, nil, nil)
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("listing %s: %s: %s", prefix, response.Status, snippet(answer))
		}
		var page listPage
		if err = xml.Unmarshal(answer, &page); err != nil {
			return nil, fmt.Errorf("listing %s: %w", prefix, err)
		}
		for _, content := range page.Contents {
			objects = append(objects, Object{Key: content.Key, Size: content.Size, Modified: content.LastModified})
		}
		if !page.IsTruncated {
			return objects, nil
		}
		if page.NextContinuationToken == "" {
			return nil, fmt.Errorf("listing %s: a truncated page with no continuation token", prefix)
		}
		token = page.NextContinuationToken
	}
}

// A LifecycleRule is one rule of the bucket's lifecycle that deletes objects: the key prefix it applies to (empty is
// every key) and after how many days, or on what date.
type LifecycleRule struct {
	Id     string
	Prefix string
	Days   int
	Date   string
}

// lifecycleConfiguration is a GetBucketLifecycleConfiguration answer: a rule's prefix is its Filter's, its Filter's And's,
// or the older form's own.
type lifecycleConfiguration struct {
	Rules []struct {
		Id     string `xml:"ID"`
		Status string `xml:"Status"`
		Prefix string `xml:"Prefix"`
		Filter struct {
			Prefix string `xml:"Prefix"`
			And    struct {
				Prefix string `xml:"Prefix"`
			} `xml:"And"`
		} `xml:"Filter"`
		Expiration *struct {
			Days int    `xml:"Days"`
			Date string `xml:"Date"`
		} `xml:"Expiration"`
	} `xml:"Rule"`
}

// ErrLifecycleUnreadable is a lifecycle this key may not read (R2 answers 403 to a key scoped to objects).
var ErrLifecycleUnreadable = errors.New("the bucket's lifecycle isn't readable with this key")

// Lifecycle is every enabled rule of the bucket's lifecycle that deletes objects (GetBucketLifecycleConfiguration):
// none when the bucket has no lifecycle, and ErrLifecycleUnreadable when the key may not read it.
func (bucket Bucket) Lifecycle() ([]LifecycleRule, error) {
	response, answer, err := bucket.do(http.MethodGet, "", url.Values{"lifecycle": {""}}, nil, nil)
	if err != nil {
		return nil, err
	}
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		if strings.Contains(string(answer), "NoSuchLifecycleConfiguration") {
			return []LifecycleRule{}, nil
		}
		return nil, fmt.Errorf("the bucket's lifecycle: %s: %s", response.Status, snippet(answer))
	case http.StatusForbidden, http.StatusUnauthorized:
		return nil, fmt.Errorf("%w: %s", ErrLifecycleUnreadable, snippet(answer))
	default:
		return nil, fmt.Errorf("the bucket's lifecycle: %s: %s", response.Status, snippet(answer))
	}
	var configuration lifecycleConfiguration
	if err = xml.Unmarshal(answer, &configuration); err != nil {
		return nil, fmt.Errorf("the bucket's lifecycle: %w", err)
	}
	rules := []LifecycleRule{}
	for _, rule := range configuration.Rules {
		if !strings.EqualFold(rule.Status, "Enabled") || rule.Expiration == nil {
			continue
		}
		prefix := rule.Filter.Prefix
		if prefix == "" {
			prefix = rule.Filter.And.Prefix
		}
		if prefix == "" {
			prefix = rule.Prefix
		}
		rules = append(rules, LifecycleRule{Id: rule.Id, Prefix: prefix, Days: rule.Expiration.Days, Date: rule.Expiration.Date})
	}
	return rules, nil
}

// Sign signs request for R2 (AWS Signature Version 4, region auto, service s3): it sets X-Amz-Date and
// X-Amz-Content-Sha256 (payload, the body's sha256 in hex) and then Authorization over the host and every header
// the request carries, so nothing it sends goes unsigned.
func Sign(request *http.Request, payload string, credentials Credentials, at time.Time) {
	sign(request, payload, credentials, at, "auto")
}

func sign(request *http.Request, payload string, credentials Credentials, at time.Time, region string) {
	stamp := at.UTC().Format("20060102T150405Z")
	day := stamp[:8]
	request.Header.Del("Authorization")
	request.Header.Set("X-Amz-Date", stamp)
	request.Header.Set("X-Amz-Content-Sha256", payload)
	host := request.Host
	if host == "" {
		host = request.URL.Host
	}
	headers := map[string]string{"host": host}
	for name, values := range request.Header {
		trimmed := make([]string, len(values))
		for index, value := range values {
			trimmed[index] = strings.Join(strings.Fields(value), " ")
		}
		headers[strings.ToLower(name)] = strings.Join(trimmed, ",")
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name + ":" + headers[name] + "\n")
	}
	signedHeaders := strings.Join(names, ";")
	canonicalRequest := strings.Join([]string{
		request.Method,
		escape(request.URL.Path, false),
		canonicalQuery(request.URL.Query()),
		canonicalHeaders.String(),
		signedHeaders,
		payload,
	}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	requestSum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hex.EncodeToString(requestSum[:])
	key := []byte("AWS4" + credentials.SecretAccessKey)
	for _, part := range []string{day, region, "s3", "aws4_request"} {
		key = mac(key, part)
	}
	signature := hex.EncodeToString(mac(key, stringToSign))
	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+credentials.AccessKeyId+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func mac(key []byte, text string) []byte {
	hash := hmac.New(sha256.New, key)
	hash.Write([]byte(text))
	return hash.Sum(nil)
}

// escape is Signature Version 4's encoding: every byte but A-Z, a-z, 0-9, -, ., _ and ~ as %XX, and / kept in a
// path (slash false) or encoded in a query (slash true).
func escape(text string, slash bool) string {
	var escaped strings.Builder
	for index := 0; index < len(text); index++ {
		character := text[index]
		switch {
		case 'A' <= character && character <= 'Z', 'a' <= character && character <= 'z', '0' <= character && character <= '9',
			character == '-', character == '.', character == '_', character == '~', character == '/' && !slash:
			escaped.WriteByte(character)
		default:
			fmt.Fprintf(&escaped, "%%%02X", character)
		}
	}
	return escaped.String()
}

// canonicalQuery is the query sorted by name, then value, each escaped, as both signed and sent.
func canonicalQuery(query url.Values) string {
	pairs := [][2]string{}
	for name, values := range query {
		for _, value := range values {
			pairs = append(pairs, [2]string{escape(name, true), escape(value, true)})
		}
	}
	sort.Slice(pairs, func(left, right int) bool {
		if pairs[left][0] != pairs[right][0] {
			return pairs[left][0] < pairs[right][0]
		}
		return pairs[left][1] < pairs[right][1]
	})
	joined := make([]string, len(pairs))
	for index, pair := range pairs {
		joined[index] = pair[0] + "=" + pair[1]
	}
	return strings.Join(joined, "&")
}
