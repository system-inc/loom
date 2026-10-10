package fleet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// stubPipeline answers the fleet routes as the shape on #83911s7 says, from memory.
type stubPipeline struct {
	mutex   sync.Mutex
	sources map[string]*Source
	seq     int
	server  *httptest.Server
}

func newStubPipeline(t *testing.T) *stubPipeline {
	stub := &stubPipeline{sources: map[string]*Source{}}
	stub.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		stub.mutex.Lock()
		defer stub.mutex.Unlock()
		if request.Header.Get("Authorization") == "" {
			http.Error(writer, "no token", http.StatusUnauthorized)
			return
		}
		path := request.URL.Path
		switch {
		case path == "/fleet" && request.Method == http.MethodGet:
			list := []Source{}
			for _, source := range stub.sources {
				list = append(list, *source)
			}
			json.NewEncoder(writer).Encode(map[string][]Source{"sources": list})
		case strings.HasSuffix(path, "/counts") && request.Method == http.MethodPost:
			name := strings.TrimSuffix(strings.TrimPrefix(path, "/fleet/sources/"), "/counts")
			var counts Counts
			json.NewDecoder(request.Body).Decode(&counts)
			stub.sources[name].Counts = &counts
			writer.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(path, "/fleet/sources/") && request.Method == http.MethodPost:
			name := strings.TrimPrefix(path, "/fleet/sources/")
			var change Change
			json.NewDecoder(request.Body).Decode(&change)
			source := stub.sources[name]
			if source == nil {
				source = &Source{Name: name, On: true}
				stub.sources[name] = source
			}
			if change.Kind != nil {
				source.Kind = *change.Kind
			}
			if change.Cap != nil {
				source.Cap = *change.Cap
			}
			if change.On != nil {
				source.On = *change.On
			}
			stub.seq++
			source.Changed = Changed{Seq: stub.seq, At: "2026-10-10T00:10:00Z", By: change.By}
			json.NewEncoder(writer).Encode(map[string]Source{"source": *source})
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (stub *stubPipeline) client() *Client {
	return &Client{Pipeline: stub.server.URL, HTTP: http.DefaultClient, Token: func() string { return "t" }}
}

func number(value int) *int     { return &value }
func word(value string) *string { return &value }
func truth(value bool) *bool    { return &value }

// A cap set through loom is the one an arm honors on its next pass, and the log line can say whose it was.
func TestTheCapAnArmHonorsIsTheFleetsAndAnOffSourceGetsNone(t *testing.T) {
	stub := newStubPipeline(t)
	client := stub.client()
	if _, err := client.Set(context.Background(), "codex", Change{Kind: word("codex"), Cap: number(80), By: "kirk"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Set(context.Background(), "codex", Change{Cap: number(25), By: "board"}); err != nil {
		t.Fatal(err)
	}
	fallback := filepath.Join(t.TempDir(), "codex-ceiling")
	os.WriteFile(fallback, []byte("80\n"), 0o644)
	cap, from := client.CapOf(context.Background(), "codex", fallback, DefaultCodexCap)
	if cap != 25 || !strings.Contains(from, "seq 2 by board") {
		t.Fatalf("cap %d from %q, not 25 from loom's seq 2 by board", cap, from)
	}
	// A cap of 0 is honored as 0, and an off source's cap is 0 whatever it holds: an arm sends no turns.
	if _, err := client.Set(context.Background(), "codex", Change{Cap: number(0), By: "board"}); err != nil {
		t.Fatal(err)
	}
	if cap, _ := client.CapOf(context.Background(), "codex", fallback, DefaultCodexCap); cap != 0 {
		t.Fatalf("a cap of 0 read as %d", cap)
	}
	client.Set(context.Background(), "codex", Change{Cap: number(40), On: truth(false), By: "board"})
	if cap, from := client.CapOf(context.Background(), "codex", fallback, DefaultCodexCap); cap != 0 || !strings.Contains(from, "off") {
		t.Fatalf("an off source's cap read as %d from %q", cap, from)
	}
	if err := client.PostCounts(context.Background(), "codex", Counts{Running: 12, Asking: 3}); err != nil {
		t.Fatal(err)
	}
	sources, _ := client.List(context.Background())
	if len(sources) != 1 || sources[0].Counts == nil || sources[0].Counts.Running != 12 {
		t.Fatalf("sources %+v", sources)
	}
}

// Without loom, an arm falls back to the file it read before the fleet, then to the default.
func TestAnArmFallsBackToTheFileThenTheDefault(t *testing.T) {
	gone := &Client{Pipeline: "http://127.0.0.1:1", HTTP: http.DefaultClient, Token: func() string { return "t" }}
	fallback := filepath.Join(t.TempDir(), "codex-ceiling")
	os.WriteFile(fallback, []byte("64\n"), 0o644)
	if cap, from := gone.CapOf(context.Background(), "codex", fallback, DefaultCodexCap); cap != 64 || from != fallback {
		t.Fatalf("cap %d from %q, not 64 from the file", cap, from)
	}
	os.WriteFile(fallback, []byte("lots\n"), 0o644)
	if cap, _ := gone.CapOf(context.Background(), "codex", fallback, DefaultCodexCap); cap != DefaultCodexCap {
		t.Fatalf("an unreadable file gave cap %d, not the default", cap)
	}
}

// Kirk's own Mac is never a source, and a change no source could carry never leaves the Mac.
func TestABadSourceOrChangeIsRefusedBeforeItIsSent(t *testing.T) {
	stub := newStubPipeline(t)
	client := stub.client()
	cases := map[string]struct {
		name   string
		change Change
	}{
		"Kirk's own Mac": {"kirk-mac", Change{Kind: word("mac"), Cap: number(1), By: "x"}},
		"a bad name":     {"Sun 1", Change{Cap: number(1), By: "x"}},
		"a cap over":     {"codex", Change{Cap: number(1001), By: "x"}},
		"a cap under":    {"codex", Change{Cap: number(-1), By: "x"}},
		"a bad kind":     {"codex", Change{Kind: word("laptop"), By: "x"}},
		"no one":         {"codex", Change{Cap: number(5)}},
	}
	for label, test := range cases {
		if _, err := client.Set(context.Background(), test.name, test.change); err == nil {
			t.Errorf("%s: accepted", label)
		}
	}
	if len(stub.sources) != 0 {
		t.Fatalf("a refused change reached loom: %v", stub.sources)
	}
}
