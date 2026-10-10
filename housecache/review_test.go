package housecache

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The review of the house cache's paths (#ktkm6fr): a raw request line, exactly as a client could send it, reaches the
// store only for a by-hash path, and a HEAD never fetches.

func reviewServer(t *testing.T) (*httptest.Server, *atomic.Int64, string) {
	content := []byte("blob bytes")
	sum := sha256.Sum256(content)
	name := hex.EncodeToString(sum[:])
	var asked atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if r.URL.Path == "/blobs/"+name {
			w.Write(content)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(upstream.Close)
	server := &Server{Directory: t.TempDir(), Upstream: upstream.URL, Limit: 1 << 30, Free: func(string) (uint64, error) { return 1 << 40, nil }}
	if err := server.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	served := httptest.NewServer(server)
	t.Cleanup(served.Close)
	return served, &asked, name
}

// raw sends one request line and returns its status code.
func raw(t *testing.T, address, line string) string {
	connection, err := net.Dial("tcp", strings.TrimPrefix(address, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	fmt.Fprintf(connection, "%s HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n", line)
	status, _ := bufio.NewReader(connection).ReadString('\n')
	fields := strings.Fields(status)
	if len(fields) < 2 {
		return status
	}
	return fields[1]
}

func TestReviewPaths(t *testing.T) {
	served, asked, name := reviewServer(t)
	cases := []struct {
		path   string
		status string
		asks   int64
	}{
		{"/blobs/" + name, "200", 1},
		// The path is decoded before it is matched: the same object, the same bytes, checked the same.
		{"/blobs%2F" + name, "200", 0},
		{"http://evil.example/blobs/" + name, "200", 0},
		{"/releases/blobs/" + name, "200", 0},
		{"/gate-inputs/" + name, "200", 0},
		{"/blobs/../blobs/" + name, "404", 0},
		{"/x/../blobs/" + name, "404", 0},
		{"/blobs/" + strings.ToUpper(name), "404", 0},
		{"/blobs/" + name + "%0A", "404", 0},
		{"/blobs/" + name + "%00", "404", 0},
		{"/blobs/" + name + "/", "404", 0},
		{"//blobs/" + name, "404", 0},
		{"/releases/../blobs/" + name, "404", 0},
		{"/blobs/" + name + "?x=1", "404", 0},
	}
	for _, check := range cases {
		before := asked.Load()
		if got := raw(t, served.URL, "GET "+check.path); got != check.status || asked.Load()-before != check.asks {
			t.Errorf("GET %q answered %s and asked the store %d times, not %s and %d", check.path, got, asked.Load()-before, check.status, check.asks)
		}
	}
	// A HEAD says what is held, and never fetches the whole blob to say it.
	fresh, freshAsked, freshName := reviewServer(t)
	if got := raw(t, fresh.URL, "HEAD /blobs/"+freshName); got != "404" || freshAsked.Load() != 0 {
		t.Fatalf("HEAD of a miss answered %s and asked the store %d times", got, freshAsked.Load())
	}
	if raw(t, fresh.URL, "GET /blobs/"+freshName); raw(t, fresh.URL, "HEAD /blobs/"+freshName) != "200" || freshAsked.Load() != 1 {
		t.Fatalf("HEAD of a held blob, or the store asked %d times", freshAsked.Load())
	}
}
