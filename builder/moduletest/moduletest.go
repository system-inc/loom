// Package moduletest is one third-party module for tests, never a real one: example.com/dep v1.0.0, laid out as a
// module proxy (and GOMODCACHE/cache/download) holds it, with the go.sum lines a module requiring it carries, so a
// test's go reads it with GOPROXY=file://<directory> and no network.
package moduletest

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// Path and Version are the module; Require is the line a go.mod requiring it holds, and Import a package of it.
const (
	Path    = "example.com/dep"
	Version = "v1.0.0"
	Require = "require " + Path + " " + Version + "\n"
	Import  = Path
)

var files = map[string]string{
	"go.mod": "module " + Path + "\n\ngo 1.22\n",
	"dep.go": "package dep\n\n// Answer is the module's one value.\nconst Answer = 42\n",
}

// hash1 is go.sum's h1: hash of named files (dirhash.Hash1).
func hash1(contents map[string]string) string {
	names := make([]string, 0, len(contents))
	for name := range contents {
		names = append(names, name)
	}
	sort.Strings(names)
	summary := sha256.New()
	for _, name := range names {
		fmt.Fprintf(summary, "%x  %s\n", sha256.Sum256([]byte(contents[name])), name)
	}
	return "h1:" + base64.StdEncoding.EncodeToString(summary.Sum(nil))
}

// Proxy writes the module into directory as a proxy lays it out and returns the go.sum lines that verify it.
func Proxy(t testing.TB, directory string) string {
	t.Helper()
	versions := filepath.Join(directory, filepath.FromSlash(Path), "@v")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	inZip := map[string]string{}
	for name, content := range files {
		inZip[Path+"@"+Version+"/"+name] = content
		entry, err := writer.Create(Path + "@" + Version + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		entry.Write([]byte(content))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		"list": []byte(Version + "\n"), Version + ".info": []byte(`{"Version":"` + Version + `","Time":"2026-01-01T00:00:00Z"}`),
		Version + ".mod": []byte(files["go.mod"]), Version + ".zip": archive.Bytes(),
	} {
		if err := os.WriteFile(filepath.Join(versions, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Path + " " + Version + " " + hash1(inZip) + "\n" + Path + " " + Version + "/go.mod " + hash1(map[string]string{"go.mod": files["go.mod"]}) + "\n"
}
