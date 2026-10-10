package main

import (
	"bytes"
	"strings"
	"testing"
)

// The updater's hook (release/updated.d/60-release) runs `loom release install` only when `loom release` names it in
// its usage, so a rollback past the release verbs passes rather than fails every minute: the usage must keep the line.
func TestReleaseUsageNamesInstallForTheHook(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"release"}, &stdout, &stderr); code != 3 {
		t.Fatalf("exit %d", code)
	}
	found := false
	for _, line := range strings.Split(stderr.String(), "\n") {
		found = found || strings.HasPrefix(line, "  loom release install")
	}
	if !found {
		t.Fatalf("the usage has no line the hook finds:\n%s", stderr.String())
	}
}
