package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/system-inc/loom/protocol"
)

// The whole hand-off with real binaries: this serve takes a unit whose job names another loom-runner, fetches that one
// from the release store by its sha256, and runs it with serve's own settings. The unit's events on the wire are that
// runner's (its sha256 in started), and it applies serve's --strict, refusing a test job that brings an environment,
// so the unit is broken there, never run here.
func TestServeHandsAUnitToTheRunnerItNames(t *testing.T) {
	if testing.Short() {
		t.Skip("builds loom-runner")
	}
	binary := filepath.Join(t.TempDir(), "loom-runner")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("building loom-runner: %v %s", err, output)
	}
	content, _ := os.ReadFile(binary)
	sum := sha256.Sum256(content)
	named := hex.EncodeToString(sum[:])
	var mutex sync.Mutex
	var events []protocol.Event
	handed := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/releases/blobs/"+named:
			writer.Write(content)
		case request.URL.Path == "/pools/box/next":
			mutex.Lock()
			defer mutex.Unlock()
			if handed {
				writer.WriteHeader(http.StatusNoContent)
				return
			}
			handed = true
			unit := protocol.Unit{Run: "r-named", Unit: "phase", TimeoutSeconds: 60, Environment: map[string]string{"FROM_THE_SERVER": "1"}, Token: "run-token", Wire: &protocol.Endpoint{Url: "http://" + request.Host + "/runs/r-named/events"},
				Test: &protocol.TestJob{Repository: protocol.AdamicRepository, Sha: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), Tools: strings.Repeat("d", 40),
					Phase: "wasi fixture-07", Runner: named}}
			json.NewEncoder(writer).Encode(unit)
		case request.URL.Path == "/runs/r-named/events":
			body, _ := io.ReadAll(request.Body)
			mutex.Lock()
			defer mutex.Unlock()
			for _, line := range bytes.Split(bytes.TrimSpace(body), []byte("\n")) {
				var event protocol.Event
				if json.Unmarshal(line, &event) == nil {
					events = append(events, event)
				}
			}
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	tokenFile := filepath.Join(t.TempDir(), "pool-token")
	os.WriteFile(tokenFile, []byte("pool-token\n"), 0o600)
	var stdout, stderr bytes.Buffer
	code := run([]string{"serve", "--strict", "--pool", server.URL + "/pools/box", "--token-file", tokenFile, "--worker", "box-1", "--until", "62s",
		"--root", t.TempDir(), "--workspace", t.TempDir(), "--releases", server.URL + "/releases/blobs/"}, &stdout, &stderr)
	if code != 0 || !strings.HasPrefix(stdout.String(), "loom-runner serve: 1 units, 0 passed, 0 failed, 1 broken in ") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	mutex.Lock()
	defer mutex.Unlock()
	var messages []string
	for _, event := range events {
		messages = append(messages, event.Message)
	}
	if len(events) == 0 || events[0].Type != "started" || events[0].RunnerSha256 != named || events[len(events)-1].Status != protocol.StatusBroken ||
		!strings.Contains(strings.Join(messages, "\n"), "refused: a test job's environment is the runner's to make") {
		t.Fatalf("the wire got %+v", events)
	}
}
