package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/system-inc/loom/gateinputs"
	"github.com/system-inc/loom/r2/r2test"
)

// publish refuses a bucket whose lifecycle would expire gate-inputs/, writing nothing (the Oct 8 manifest lived in
// blobs/ and was gone in 7 days); refuses a bucket whose lifecycle its key may not read, unless --lifecycle-unchecked
// says so, and then publishes saying so; and writes the planner's manifest file only after the manifest reads back,
// and again on a republish that moved it. Mutants that each fail it: the lifecycle check dropped; an unreadable
// lifecycle published past without the flag; the move not said.
func TestPublishRefusesAnExpiringHomeAndWritesTheFileLast(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "gate-inputs")
	os.MkdirAll(filepath.Join(directory, "css"), 0o755)
	os.WriteFile(filepath.Join(directory, "css", "package.json"), []byte("{}"), 0o644)
	manifestFile := filepath.Join(t.TempDir(), "gate-inputs-manifest")
	fake := r2test.New(t)
	fake.Lifecycle = `<LifecycleConfiguration><Rule><ID>all</ID><Status>Enabled</Status><Filter></Filter><Expiration><Days>7</Days></Expiration></Rule></LifecycleConfiguration>`
	var stdout, stderr bytes.Buffer
	if code := publishGateInputs(directory, manifestFile, fake.Bucket(), fake.Public(), true, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), `rule "all"`) {
		t.Fatalf("an expiring home: %d %q", code, stderr.String())
	}
	if keys := fake.Keys(""); len(keys) != 0 {
		t.Fatalf("an expiring home was written: %v", keys)
	}
	if _, err := os.Stat(manifestFile); !os.IsNotExist(err) {
		t.Fatalf("the manifest file was written: %v", err)
	}
	fake.Lifecycle, fake.LifecycleStatus = "", http.StatusForbidden
	stdout.Reset()
	stderr.Reset()
	if code := publishGateInputs(directory, manifestFile, fake.Bucket(), fake.Public(), false, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "--lifecycle-unchecked") {
		t.Fatalf("an unreadable lifecycle: %d %q", code, stderr.String())
	}
	if keys := fake.Keys(""); len(keys) != 0 {
		t.Fatalf("an unreadable lifecycle was published past: %v", keys)
	}
	stderr.Reset()
	if code := publishGateInputs(directory, manifestFile, fake.Bucket(), fake.Public(), true, &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "unchecked") {
		t.Fatalf("an unreadable lifecycle: %d %q %q", code, stdout.String(), stderr.String())
	}
	first, err := gateinputs.ReadFile(manifestFile)
	if err != nil || !strings.HasPrefix(stdout.String(), first+":") {
		t.Fatalf("the manifest file: %q %v, said %q", first, err, stdout.String())
	}
	if _, err := gateinputs.Check(fake.Public(), fake.Server.Client(), first); err != nil {
		t.Fatalf("the file names a manifest that doesn't read: %v", err)
	}
	os.WriteFile(filepath.Join(directory, "css", "package.json"), []byte(`{"x":1}`), 0o644)
	fake.LifecycleStatus = 0
	stdout.Reset()
	stderr.Reset()
	if code := publishGateInputs(directory, manifestFile, fake.Bucket(), fake.Public(), false, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "moved from "+first[:12]) || stderr.Len() != 0 {
		t.Fatalf("a republish that moved: %d %q %q", code, stdout.String(), stderr.String())
	}
	if second, _ := gateinputs.ReadFile(manifestFile); second == first {
		t.Fatal("a moved directory kept the manifest file")
	}
}
