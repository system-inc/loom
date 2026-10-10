package main

import (
	"bytes"
	"strings"
	"testing"
)

// The house cache refuses to listen anywhere but one address on the house's network before it touches anything, and
// loom's usage names `house-cache install` on a line of its own, which the updater's hook reads to tell a release
// that has it.
func TestHouseCacheServeRefusesAnAddressOffTheHouse(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:7380", "8.8.8.8:7380", "cloud.local:7380"} {
		directory := t.TempDir()
		var stdout, stderr bytes.Buffer
		if code := run([]string{"house-cache", "serve", "--listen", listen, "--directory", directory}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), listen) {
			t.Fatalf("listen %s: exit %d, %s", listen, code, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	run(nil, &stdout, &stderr)
	if !strings.Contains(stderr.String(), "\n  loom house-cache install\n") {
		t.Fatalf("loom's usage has no line the hook reads:\n%s", stderr.String())
	}
}
