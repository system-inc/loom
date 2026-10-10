package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// SIGHUP is loom-serve's reload, which the updater's hook asks for after a release: serve takes nothing more, lets the
// unit in hand finish, and exits 0 so systemd starts the new runner. The hangup here arrives as the pool hands out the
// unit, so it is the unit in hand. A serve that asks again didn't drain, and is stopped with SIGTERM so the test ends.
func TestAHangupDrainsServe(t *testing.T) {
	var once sync.Once
	var mutex sync.Mutex
	askedAgain := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/pools/box/next" {
			return
		}
		served := false
		once.Do(func() {
			served = true
			syscall.Kill(os.Getpid(), syscall.SIGHUP)
			fmt.Fprintf(writer, `{"run":"r-cli","unit":"in-hand","argv":["sh","-c","sleep 1"],"timeoutSeconds":30,"wire":{"url":"http://%s/runs/r-cli/events"},"token":"run-token"}`, request.Host)
		})
		if !served {
			mutex.Lock()
			askedAgain = true
			mutex.Unlock()
			syscall.Kill(os.Getpid(), syscall.SIGTERM)
			writer.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	tokenFile := filepath.Join(t.TempDir(), "pool-token")
	os.WriteFile(tokenFile, []byte("pool-token\n"), 0o600)
	var stdout, stderr bytes.Buffer
	started := time.Now()
	code := run([]string{"serve", "--pool", server.URL + "/pools/box", "--token-file", tokenFile, "--worker", "box-1", "--until", "1h",
		"--workspace", t.TempDir()}, &stdout, &stderr)
	mutex.Lock()
	defer mutex.Unlock()
	if askedAgain {
		t.Fatalf("serve asked for another unit after the hangup: stdout %q", stdout.String())
	}
	if code != 0 || !strings.HasPrefix(stdout.String(), "loom-runner serve: 1 units, 1 passed, 0 failed, 0 broken in ") ||
		!strings.Contains(stdout.String(), "stopped on a drain") || time.Since(started) > 30*time.Second {
		t.Fatalf("exit %d after %v, stdout %q, stderr %q", code, time.Since(started), stdout.String(), stderr.String())
	}
}

// A hangup that arrives before serve's drain handler is in place is ignored, never the default's exit: serve ignores
// it first thing, so even one refused at its flags leaves it ignored.
func TestServeIgnoresAHangupFromItsFirstMoment(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"serve"}, &stdout, &stderr); code != 2 || !signal.Ignored(syscall.SIGHUP) {
		t.Fatalf("exit %d, SIGHUP ignored %v", code, signal.Ignored(syscall.SIGHUP))
	}
}

// install-serve is the hook's whole work, and only a Linux box serves through systemd. The hook knows a release has it
// by this exact usage line.
func TestInstallServeTakesNoArgumentsAndOnlyLinux(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "\n  loom-runner install-serve\n") {
		t.Fatalf("the usage the hook reads: exit %d, %q", code, stderr.String())
	}
	if code := run([]string{"install-serve", "now"}, &stdout, &stderr); code != 2 {
		t.Fatalf("install-serve with an argument: exit %d", code)
	}
	if runtime.GOOS != "linux" {
		stderr.Reset()
		if code := run([]string{"install-serve"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "readies a systemd user unit, and this is "+runtime.GOOS) {
			t.Fatalf("install-serve on %s: exit %d, %q", runtime.GOOS, code, stderr.String())
		}
	}
}
