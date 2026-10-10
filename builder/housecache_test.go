package builder

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// publicDomain is the store's public domain with one blob, counting each GET.
func publicDomain(t *testing.T, content []byte) (*httptest.Server, *atomic.Int64) {
	var gets atomic.Int64
	sum := digest(content)
	served := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gets.Add(1)
		if request.URL.Path != "/blobs/"+sum {
			http.NotFound(writer, request)
			return
		}
		writer.Write(content)
	}))
	t.Cleanup(served.Close)
	return served, &gets
}

// The store reader asks the house cache first, and uses what it gives only when it hashes to the blob's name: a house
// cache that lies, or is down, or lacks the blob, costs a read of the store, never a wrong byte.
func TestTheStoreReaderTriesTheHouseCacheAndChecksIt(t *testing.T) {
	content := []byte("a product's archive")
	sum := digest(content)
	store, storeGets := publicDomain(t, content)

	var houseGets atomic.Int64
	honest := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		houseGets.Add(1)
		if request.URL.Path == "/blobs/"+sum {
			writer.Write(content)
			return
		}
		http.NotFound(writer, request)
	}))
	defer honest.Close()
	liar := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		houseGets.Add(1)
		writer.Write([]byte("a product's archivX"))
	}))
	defer liar.Close()
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + listener.Addr().String()
	listener.Close()

	got, err := Store{Read: store.URL, House: honest.URL}.blob(sum)
	if err != nil || !bytes.Equal(got, content) || storeGets.Load() != 0 || houseGets.Load() != 1 {
		t.Fatalf("through an honest house cache: %q, %v, store %d", got, err, storeGets.Load())
	}
	for name, house := range map[string]string{"lying": liar.URL, "dead": dead} {
		before := storeGets.Load()
		got, err := Store{Read: store.URL, House: house}.blob(sum)
		if err != nil || !bytes.Equal(got, content) || storeGets.Load() != before+1 {
			t.Fatalf("through a %s house cache: %q, %v", name, got, err)
		}
	}
	missing := digest([]byte("never uploaded"))
	if _, err := (Store{Read: store.URL, House: honest.URL}).blob(missing); err == nil || !strings.Contains(err.Error(), ErrNotStored.Error()) {
		t.Fatalf("a blob neither holds: %v", err)
	}
	// Only blobs go through the house cache: a ref is mutable, so it is always the store's.
	before := houseGets.Load()
	(Store{Read: store.URL, House: honest.URL}).Ref(strings.Repeat("a", 64))
	if houseGets.Load() != before {
		t.Fatal("a ref was asked of the house cache")
	}
}
