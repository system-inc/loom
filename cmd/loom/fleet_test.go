package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/system-inc/loom/fleet"
)

// loom fleet cap sends the change with who made it, and cap-of prints the cap alone on stdout, for rearm.sh to read.
func TestFleetCapSetsTheCapAndCapOfPrintsIt(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".loom"), 0o700)
	os.WriteFile(filepath.Join(home, ".loom", "token-secret"), []byte(strings.Repeat("s", 64)), 0o600)
	t.Setenv("HOME", home)
	var changes []fleet.Change
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			var change fleet.Change
			json.NewDecoder(request.Body).Decode(&change)
			changes = append(changes, change)
			json.NewEncoder(writer).Encode(map[string]fleet.Source{"source": {Name: "codex", Kind: "codex", Cap: *change.Cap, On: true, Changed: fleet.Changed{Seq: 7, By: change.By}}})
			return
		}
		json.NewEncoder(writer).Encode(map[string][]fleet.Source{"sources": {{Name: "codex", Kind: "codex", Cap: 25, On: true, Changed: fleet.Changed{Seq: 7, By: "kirk"}}}})
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	if code := fleetCommand([]string{"cap", "codex", "25", "--pipeline", server.URL, "--by", "kirk"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if len(changes) != 1 || *changes[0].Cap != 25 || changes[0].By != "kirk" || !strings.Contains(stdout.String(), "cap 25") {
		t.Fatalf("changes %+v, said %q", changes, stdout.String())
	}
	stdout.Reset()
	if code := fleetCommand([]string{"cap-of", "codex", "--pipeline", server.URL}, &stdout, &stderr); code != 0 || stdout.String() != "25\n" {
		t.Fatalf("exit %d, printed %q", code, stdout.String())
	}
	if code := fleetCommand([]string{"cap", "kirk-mac", "4", "--pipeline", server.URL}, &stdout, &stderr); code == 0 || len(changes) != 1 {
		t.Fatalf("Kirk's own Mac was taken as a source")
	}
}
