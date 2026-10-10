package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// A bridge machine: its queue-bridge.conf, token secret and state, and a stand-in queue that records every request.
type bridgeFixture struct {
	config, state string
	mutex         sync.Mutex
	requests      []string
}

func newBridgeFixture(t *testing.T) *bridgeFixture {
	root := t.TempDir()
	made := &bridgeFixture{config: filepath.Join(root, "queue-bridge.conf"), state: filepath.Join(root, "state")}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		made.mutex.Lock()
		made.requests = append(made.requests, request.Method+" "+request.URL.RequestURI())
		made.mutex.Unlock()
		if !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ") {
			writer.WriteHeader(401)
			return
		}
		writer.Write([]byte(`{"changes":[],"futures":[]}`))
	}))
	t.Cleanup(server.Close)
	secret := filepath.Join(root, "token-secret")
	os.WriteFile(secret, []byte(strings.Repeat("s", 64)+"\n"), 0o600)
	os.WriteFile(made.config, []byte("queue = "+server.URL+"\nstate = "+made.state+"\nsecret = "+secret+"\nrepository = "+root+"\n"), 0o644)
	return made
}

func (made *bridgeFixture) pass() (int, string) {
	made.mutex.Lock()
	made.requests = nil
	made.mutex.Unlock()
	var stdout, stderr bytes.Buffer
	code := run([]string{"queue-bridge", "--config", made.config}, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

// A pass reads the queue with a token and carries facts only, never asking for futures or landings; one pass at a time.
func TestQueueBridgeOnePassAtATime(t *testing.T) {
	made := newBridgeFixture(t)
	os.MkdirAll(made.state, 0o755)
	lock, err := os.OpenFile(filepath.Join(made.state, "lock"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if code, output := made.pass(); code != 0 || len(made.requests) != 0 {
		t.Fatalf("while another pass holds the lock: exit %d, asked %q: %s", code, made.requests, output)
	}
	syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if code, output := made.pass(); code != 0 || strings.Join(made.requests, ",") != "GET /submissions?state=unchecked" {
		t.Fatalf("once free: exit %d, asked %q: %s", code, made.requests, output)
	}
}

// Without queue-bridge.conf this machine isn't the bridge, and the updater's hook finds `loom queue-bridge install` in
// loom's usage.
func TestLoomQueueBridgeNeedsItsConfAndItsInstallIsInTheUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"queue-bridge", "--config", filepath.Join(t.TempDir(), "queue-bridge.conf")}, &stdout, &stderr); code != 3 || !strings.Contains(stderr.String(), "makes it the bridge") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	stderr.Reset()
	run(nil, &stdout, &stderr)
	if !strings.Contains(stderr.String(), "\n  loom queue-bridge install\n") {
		t.Fatalf("the usage:\n%s", stderr.String())
	}
}
