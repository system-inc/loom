package builder

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An entry is one tar entry a test writes by hand, as a dishonest or broken archive would hold it.
type entry struct {
	name, body, link string
	typeflag         byte
	mode             int64
}

// tarGzip writes entries, in order, as a gzipped tar, with nothing checked.
func tarGzip(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressor := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressor)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: entry.mode, Typeflag: entry.typeflag, Size: int64(len(entry.body))}
		if header.Mode == 0 {
			header.Mode = 0o644
		}
		if entry.link != "" {
			header.Linkname, header.Size = entry.link, 0
			if header.Typeflag == 0 {
				header.Typeflag = tar.TypeSymlink
			}
		}
		if header.Typeflag == 0 {
			header.Typeflag = tar.TypeReg
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			writer.Write([]byte(entry.body))
		}
	}
	writer.Close()
	compressor.Close()
	return buffer.Bytes()
}

// products writes two buildcache products into a fresh cache directory, at the given modification time and with
// the given umask-like mode on data files, and returns the directory.
func products(t *testing.T, modified time.Time, dataMode os.FileMode, order []string) string {
	t.Helper()
	cache := t.TempDir()
	files := map[string]string{
		keyOf("a") + "/bin/tool":       "#!/bin/sh\necho tool\n",
		keyOf("a") + "/data/case.json": "{}\n",
		keyOf("a") + ".inputs":         "name a\n",
		keyOf("b") + "/b.a":            "an archive",
	}
	for _, name := range order {
		path := filepath.Join(cache, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(path), 0o755)
		mode := dataMode
		if strings.Contains(name, "/bin/") {
			mode = 0o755
		}
		os.WriteFile(path, []byte(files[name]), mode)
		os.Chmod(path, mode)
		os.Chtimes(path, modified, modified)
	}
	return cache
}

// A product's archive depends only on its files' names, bytes and executable bits: built twice, from files written
// in another order, at another time, with another mode on a data file, it is the same bytes.
func TestAProductArchiveIsTheSameBytesFromTheSameFiles(t *testing.T) {
	order := []string{keyOf("a") + "/bin/tool", keyOf("a") + "/data/case.json", keyOf("a") + ".inputs", keyOf("b") + "/b.a"}
	reversed := []string{order[3], order[2], order[1], order[0]}
	first := products(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 0o644, order)
	second := products(t, time.Date(2026, 10, 10, 6, 30, 0, 0, time.UTC), 0o600, reversed)
	one, files, err := ProductArchive(first, []string{keyOf("a"), keyOf("b")})
	if err != nil || files != 4 {
		t.Fatalf("%d files, %v", files, err)
	}
	again, _, _ := ProductArchive(first, []string{keyOf("b"), keyOf("a")})
	other, _, err := ProductArchive(second, []string{keyOf("a"), keyOf("b")})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one, again) || !bytes.Equal(one, other) {
		t.Fatalf("three archives of the same files: %s, %s, %s", digest(one), digest(again), digest(other))
	}
	// Whatever order its entries are given in.
	forward := []archiveEntry{{Name: "x/one", File: filepath.Join(first, keyOf("b"), "b.a")}, {Name: "y/two", File: filepath.Join(first, keyOf("a"), "bin", "tool"), Executable: true}}
	left, _ := writeArchive(forward)
	right, _ := writeArchive([]archiveEntry{forward[1], forward[0]})
	if !bytes.Equal(left, right) {
		t.Fatal("one archive's entries in two orders made two archives")
	}
	// And it unpacks to the same files, executable where they were.
	unpacked := t.TempDir()
	if err = Unpack(one, unpacked, buildcachePath); err != nil {
		t.Fatal(err)
	}
	if content, _ := os.ReadFile(filepath.Join(unpacked, keyOf("a"), "data", "case.json")); string(content) != "{}\n" {
		t.Fatalf("case.json: %q", content)
	}
	if info, err := os.Stat(filepath.Join(unpacked, keyOf("a"), "bin", "tool")); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("the tool: %v %v", info, err)
	}
	if info, err := os.Stat(filepath.Join(unpacked, keyOf("a"), "data", "case.json")); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("a data file: %v %v", info, err)
	}
	// A test binary is gzip alone, the same way.
	binary := bytes.Repeat([]byte("ELF"), 1000)
	if left, right := mustGzip(t, binary), mustGzip(t, append([]byte{}, binary...)); !bytes.Equal(left, right) {
		t.Fatal("two gzips of one binary differ")
	}
}

func mustGzip(t *testing.T, content []byte) []byte {
	t.Helper()
	blob, err := gzipped(content)
	if err != nil {
		t.Fatal(err)
	}
	if back, err := gunzipped(blob); err != nil || !bytes.Equal(back, content) {
		t.Fatalf("gunzipped %d bytes, %v", len(back), err)
	}
	return blob
}

// Unpack refuses every entry that would land outside its directory, or that isn't a plain file or a link that
// stays inside, and writes nothing outside it whatever the archive holds.
func TestUnpackRefusesAnEntryThatWouldLandOutsideItsDirectory(t *testing.T) {
	cases := map[string][]entry{
		"climbing out":                     {{name: "../escaped", body: "x"}},
		"climbing out from inside":         {{name: "a/../../escaped", body: "x"}},
		"absolute":                         {{name: "/tmp/escaped", body: "x"}},
		"unclean":                          {{name: "./a", body: "x"}},
		"a link out":                       {{name: "link", link: "../escaped"}},
		"a link out from deeper":           {{name: "a/b/link", link: "../../../escaped"}},
		"an absolute link":                 {{name: "link", link: "/etc/passwd"}},
		"a link climbing after names":      {{name: "a/link", link: "here/../../../escaped"}},
		"a file through a link":            {{name: "a/real", body: "x"}, {name: "inside", link: "a"}, {name: "inside/written", body: "x"}},
		"a hard link":                      {{name: "a", body: "x"}, {name: "hard", link: "a", typeflag: tar.TypeLink}},
		"a device":                         {{name: "device", typeflag: tar.TypeChar}},
		"a directory entry":                {{name: "a", typeflag: tar.TypeDir}},
		"a link to its own directory":      {{name: "link", link: "."}},
		"a link to a directory it is in":   {{name: "a/b/up", link: ".."}},
		"a link to itself":                 {{name: "a/self", link: "self"}},
		"the review's case-folding escape": {{name: "L", link: "."}, {name: "l/M", link: "."}, {name: "l/m/x", link: "../.."}},
		"one name twice":                   {{name: "a", body: "x"}, {name: "a", body: "y"}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			directory := filepath.Join(parent, "one", "two", "unpacked")
			if err := Unpack(tarGzip(t, entries...), directory, nil); err == nil {
				t.Fatalf("%+v unpacked", entries)
			}
			filepath.WalkDir(parent, func(path string, found os.DirEntry, err error) error {
				if err == nil && !found.IsDir() && !strings.HasPrefix(path, directory+string(filepath.Separator)) {
					t.Fatalf("a refused archive wrote %s", path)
				}
				return nil
			})
		})
	}
	// Adamic's own tracked links unpack: siblings, a dangling one, and one up and over.
	honest := tarGzip(t,
		entry{name: "walking/B.txt", body: "b"},
		entry{name: "walking/dangling", link: "missing"},
		entry{name: "walking/link-to-file", link: "B.txt"},
		entry{name: "evidence/cumulative/after.jsonl.gz", body: "z"},
		entry{name: "evidence/followup/before.jsonl.gz", link: "../cumulative/after.jsonl.gz"},
	)
	directory := t.TempDir()
	if err := Unpack(honest, directory, nil); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(directory, "evidence", "followup", "before.jsonl.gz")); err != nil || string(content) != "z" {
		t.Fatalf("an honest link: %q %v", content, err)
	}
	// What allowed refuses is refused.
	if err := Unpack(honest, t.TempDir(), buildcachePath); err == nil {
		t.Fatal("a tree's files unpacked as a product")
	}
	if err := Unpack([]byte("not gzip"), t.TempDir(), nil); err == nil {
		t.Fatal("bytes that aren't gzip unpacked")
	}
}

// gitCommit commits everything in tree.
func gitCommit(t *testing.T, tree string) {
	t.Helper()
	for _, arguments := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "more"}} {
		if output, err := exec.Command("git", append([]string{"-C", tree}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", arguments, err, output)
		}
	}
}

// A tree's source archive keeps its tracked links, and a link Unpack would refuse fails the build.
func TestTheSourceArchiveKeepsLinksAndRefusesOneThatLeaves(t *testing.T) {
	tree := gitTree(t, map[string]string{"go.mod": "module m\n", "a/B.txt": "b\n", "run.sh": "#!/bin/sh\n"})
	os.Chmod(filepath.Join(tree, "run.sh"), 0o755)
	os.Symlink("B.txt", filepath.Join(tree, "a", "link"))
	gitCommit(t, tree)
	archive, err := SourceArchive(tree)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := SourceArchive(tree)
	if !bytes.Equal(archive, again) {
		t.Fatal("two archives of one tree differ")
	}
	directory := t.TempDir()
	if err = Unpack(archive, directory, nil); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(directory, "a", "link")); err != nil || target != "B.txt" {
		t.Fatalf("the link: %q %v", target, err)
	}
	if info, err := os.Stat(filepath.Join(directory, "run.sh")); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("run.sh: %v %v", info, err)
	}
	os.Symlink("../../outside", filepath.Join(tree, "a", "out"))
	gitCommit(t, tree)
	if _, err = SourceArchive(tree); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("a link out of the tree: %v", err)
	}
}

// folds reports whether the filesystem under a test's temporary directories takes spelling for the same name as written.
func folds(t *testing.T, written, spelling string) bool {
	t.Helper()
	probe := t.TempDir()
	if err := os.WriteFile(filepath.Join(probe, written), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := os.Lstat(filepath.Join(probe, spelling))
	return err == nil
}

// An entry's parents are looked up on disk, so a link reached by another spelling the filesystem folds to its name
// (case on APFS and most macOS volumes, normalization on APFS) is refused as surely as the link's own name. The
// review's archive is refused everywhere by its first link; these need no link to an ancestor at all.
func TestUnpackLooksUpParentsOnDiskWhereTheFilesystemFoldsNames(t *testing.T) {
	cases := []struct {
		name, written, spelling string
	}{
		{"case", "Link", "link"},
		{"normalization", "é", "é"},
	}
	for _, folded := range cases {
		t.Run(folded.name, func(t *testing.T) {
			directory := t.TempDir()
			if !folds(t, folded.written, folded.spelling) {
				t.Skipf("this filesystem keeps %q and %q apart", folded.written, folded.spelling)
			}
			archive := tarGzip(t, entry{name: "sub/kept", body: "x"}, entry{name: folded.written, link: "sub"}, entry{name: folded.spelling + "/through", body: "x"})
			if err := Unpack(archive, directory, nil); err == nil || !strings.Contains(err.Error(), "under the link") {
				t.Fatalf("an entry under %q spelled %q: %v", folded.written, folded.spelling, err)
			}
			if _, err := os.Lstat(filepath.Join(directory, "sub", "through")); err == nil {
				t.Fatal("an entry was written through the link")
			}
		})
	}
}
