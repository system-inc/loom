package main

import (
	"bytes"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/coordinator"
	"github.com/system-inc/loom/protocol"
)

func TestThePoolFlagTakesNameEqualsSlotsAndRepeats(t *testing.T) {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var pools poolSlotsFlag
	flags.Var(&pools, "pool", "")
	if err := flags.Parse([]string{"--pool", "codex=4", "--pool", "spare=1"}); err != nil || pools.String() != "codex=4 spare=1" {
		t.Fatalf("parsed %q, %v", pools.String(), err)
	}
	for _, bad := range []string{"codex", "codex=0", "codex=x", "a/b=2", "=3"} {
		if err := pools.Set(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPoolStatusReadsThePoolWithABoardToken(t *testing.T) {
	secret := []byte("loom-test-secret")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		claims, err := protocol.VerifyToken(secret, strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "), time.Now())
		if err != nil || claims.Scope != protocol.ScopeBoard || request.URL.Path != "/pools/codex" {
			http.Error(writer, "no", http.StatusForbidden)
			return
		}
		io.WriteString(writer, `{"queued":2,"workers":[{"worker":"codex-1","cpus":8,"seenAt":"2026-10-08T20:00:00Z","took":"tests[shard=1]"},{"worker":"codex-2","cpus":4,"seenAt":"2026-10-08T20:00:30Z","took":null}]}`)
	}))
	defer server.Close()
	status, err := readPoolStatus(server.Client(), server.URL, secret, "codex")
	if err != nil {
		t.Fatal(err)
	}
	var printed bytes.Buffer
	writePoolStatus(&printed, "codex", status, time.Date(2026, 10, 8, 20, 1, 0, 0, time.UTC))
	want := "pool codex: 2 queued, 2 workers\n  codex-1  8 cpus  asked 60 s ago  took tests[shard=1]\n  codex-2  4 cpus  asked 30 s ago  took nothing yet\n"
	if printed.String() != want {
		t.Fatalf("printed\n%s\nwant\n%s", printed.String(), want)
	}
	if _, err := readPoolStatus(server.Client(), server.URL, []byte("other secret"), "codex"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a refused token: %v", err)
	}
}

func TestTheGatesTableNeverLimitsAPool(t *testing.T) {
	state := t.TempDir()
	os.WriteFile(filepath.Join(state, "slots"), []byte("server S\n"), 0o644)
	pool := &coordinator.PoolMachine{Pool: "codex"}
	slots := []coordinator.Machine{coordinator.SSHMachine{Box: "server", Class: "S"}, pool, pool}
	limit := yieldLimit(slots, filepath.Join(state, "slots"))
	if limit("server") != 0 || limit(pool.Name()) < 2 {
		t.Fatalf("server %d, pool %d", limit("server"), limit(pool.Name()))
	}
}

func TestPoolCancelDropsTheRunsUnitsWithACoordinatorTokenForThatRun(t *testing.T) {
	secret := []byte("loom-test-secret")
	run := "adamic-gate-pilot-20261009T011550-8aad87d5"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		claims, err := protocol.VerifyToken(secret, strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "), time.Now())
		body, _ := io.ReadAll(request.Body)
		if err != nil || claims.Scope != protocol.ScopeCoordinator || claims.Run != run || request.Method != http.MethodPost ||
			request.URL.Path != "/pools/codex/cancel" || string(body) != `{"run":"`+run+`"}` {
			http.Error(writer, "no", http.StatusForbidden)
			return
		}
		writer.Write([]byte(`{"dropped":9}`))
	}))
	defer server.Close()
	dropped, err := cancelPoolRun(server.Client(), server.URL, secret, "codex", run)
	if err != nil || dropped != 9 {
		t.Fatalf("dropped %d, %v", dropped, err)
	}
	// A refusal is an error naming the pool's answer, never zero dropped.
	if _, err := cancelPoolRun(server.Client(), server.URL, secret, "codex", "another-run"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a refused cancel read as %v", err)
	}
}
