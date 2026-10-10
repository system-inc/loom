package gateinputs

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/r2"
	"github.com/system-inc/loom/r2/r2test"
)

// The gate inputs' publisher and its readers. Each mutant below must make a test here fail:
//
//	Pack keeping a file's own time, or its own permissions, or packing a git index as it is on disk, or packing the
//	cycle ledger's output: TestPackGivesTheSameHashAfterABoxsOwnRuns
//	Pack dropping the executable bit (every file 0644): TestPackMovesTheHashWithEveryByteAndModeTheTestsRead
//	NormalizeIndex zeroing the mode with the stat, or dropping the extended flags (skip-worktree): TestAnUnpackedCheckoutReadsAsItWasPacked
//	NormalizeIndex reading a mandatory extension as optional: TestNormalizeIndexRefusesWhatItCantRewriteWhole
//	Publish writing what the bucket holds again (no HEAD first), or its chunks under blobs/:
//	TestPublishWritesChunksFirstUnderGateInputsAndNothingTwice
//	Check skipping the manifest's tar, or the chunks' presence: TestCheckRefusesAManifestNoRunnerCouldRead
//	the name taken from the tar.gz, or a publish that writes again what is already whole by name:
//	TestTheNameIsTheTarsSoACompressionChangeMovesNoKey
//	a held manifest kept over its missing chunks: TestPublishReplacesAManifestWhoseChunksAreGone
//	a Pin reading its file once, or keying on a manifest it didn't check: TestAPinRereadsItsFileAndChecksEachManifestOnce
//	a Pin trusting a check forever, or a failed check: TestAPinChecksItsManifestAgainEveryHour
//	HomeExpires missing a whole-bucket rule, or a rule on a key under the prefix: TestHomeExpiresNamesEveryRuleThatReachesThePrefix

// git runs git in directory with no configuration but a fixed identity and times, and fails the test on an error.
func git(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=Loom", "GIT_AUTHOR_EMAIL=loom@system.inc", "GIT_COMMITTER_NAME=Loom", "GIT_COMMITTER_EMAIL=loom@system.inc",
		"GIT_AUTHOR_DATE=2026-10-08T00:00:00Z", "GIT_COMMITTER_DATE=2026-10-08T00:00:00Z")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// inputsFixture is a small gate inputs directory: a pinned checkout with a skip-worktree path (as a sparse checkout
// keeps one), an npm project with an executable, a link within the directory, and the cycle ledger's output.
func inputsFixture(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "gate-inputs")
	checkout := filepath.Join(directory, "typescript")
	write(t, filepath.Join(checkout, "src", "compiler", "parser.ts"), "export const parser = 1;\n", 0o644)
	write(t, filepath.Join(checkout, "bin", "tsc"), "#!/bin/sh\necho tsc\n", 0o755)
	write(t, filepath.Join(checkout, "tests", "far.ts"), "far\n", 0o644)
	git(t, checkout, "init", "-q", "-b", "main")
	git(t, checkout, "add", "-A")
	git(t, checkout, "commit", "-q", "-m", "TypeScript")
	git(t, checkout, "update-index", "--skip-worktree", "tests/far.ts")
	os.Remove(filepath.Join(checkout, "tests", "far.ts"))
	write(t, filepath.Join(directory, "css-printer", "package.json"), `{"dependencies":{"prettier":"3.9.6"}}`, 0o644)
	write(t, filepath.Join(directory, "css-printer", "node_modules", "prettier", "index.js"), "module.exports = {};\n", 0o644)
	write(t, filepath.Join(directory, "checker", "tsgo.a"), "!<arch>\n", 0o644)
	if err := os.Symlink("css-printer", filepath.Join(directory, "json-prettier")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(directory, "cycle-ledger-output.json"), `{"run":1}`, 0o664)
	return directory
}

// pack is Pack with small chunks, keeping them.
func pack(t *testing.T, directory string) (Manifest, map[string][]byte) {
	t.Helper()
	chunks := map[string][]byte{}
	manifest, err := Pack(directory, 4096, func(hash string, content []byte) error {
		chunks[hash] = append([]byte{}, content...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return manifest, chunks
}

// A box gate runs its tests against the directory it would publish: they write the cycle ledger's output, `git
// status` rewrites a checkout's index with the stat data it finds, and an umask or a touch moves modes and times.
// None of it is what the tests read, so none of it moves the hash.
func TestPackGivesTheSameHashAfterABoxsOwnRuns(t *testing.T) {
	directory := inputsFixture(t)
	before, _ := pack(t, directory)
	if again, _ := pack(t, directory); again.Name != before.Name {
		t.Fatalf("the same directory packed twice: %s then %s", before.Name, again.Name)
	}
	index := filepath.Join(directory, "typescript", ".git", "index")
	raw, _ := os.ReadFile(index)
	later := time.Now().Add(time.Hour)
	for _, path := range []string{"typescript/src/compiler/parser.ts", "typescript/bin/tsc", "css-printer/package.json", "checker/tsgo.a"} {
		if err := os.Chtimes(filepath.Join(directory, path), later, later); err != nil {
			t.Fatal(err)
		}
	}
	git(t, filepath.Join(directory, "typescript"), "update-index", "-q", "--refresh")
	if rewritten, _ := os.ReadFile(index); bytes.Equal(rewritten, raw) {
		t.Fatal("the refresh didn't rewrite the index, so this test proves nothing about it")
	}
	os.Chmod(filepath.Join(directory, "css-printer", "package.json"), 0o664)
	os.Chmod(filepath.Join(directory, "typescript", "bin", "tsc"), 0o775)
	write(t, filepath.Join(directory, "cycle-ledger-output.json"), `{"run":2,"longer":true}`, 0o644)
	if after, _ := pack(t, directory); after.Name != before.Name {
		t.Fatalf("a box's own runs moved the gate inputs' hash: %s then %s", before.Name, after.Name)
	}
}

// Every byte and mode the tests read is in the hash: a changed file, a new one, a lost executable bit.
func TestPackMovesTheHashWithEveryByteAndModeTheTestsRead(t *testing.T) {
	directory := inputsFixture(t)
	before, _ := pack(t, directory)
	changes := map[string]func(){
		"a changed byte": func() {
			write(t, filepath.Join(directory, "css-printer", "node_modules", "prettier", "index.js"), "module.exports = {x:1};\n", 0o644)
		},
		"a new file":            func() { write(t, filepath.Join(directory, "graphql", "package.json"), "{}", 0o644) },
		"a lost executable bit": func() { os.Chmod(filepath.Join(directory, "typescript", "bin", "tsc"), 0o644) },
	}
	seen := map[string]string{before.Name: "the fixture"}
	for _, name := range []string{"a changed byte", "a new file", "a lost executable bit"} {
		changes[name]()
		after, _ := pack(t, directory)
		if earlier, found := seen[after.Name]; found {
			t.Errorf("%s packs to %s, as %s did", name, after.Name, earlier)
		}
		seen[after.Name] = name
	}
}

// unpack writes the chunks in order as one tar.gz and unpacks it with the system's tar, as prepare.sh does.
func unpack(t *testing.T, manifest Manifest, chunks map[string][]byte) string {
	t.Helper()
	var whole bytes.Buffer
	for _, chunk := range manifest.Chunks {
		whole.Write(chunks[chunk])
	}
	if digest(whole.Bytes()) != manifest.Total {
		t.Fatalf("the chunks join to %s, and the manifest's total is %s", digest(whole.Bytes()), manifest.Total)
	}
	destination := t.TempDir()
	archive := filepath.Join(t.TempDir(), "gate-inputs.tar.gz")
	os.WriteFile(archive, whole.Bytes(), 0o644)
	if output, err := exec.Command("tar", "-C", destination, "-xzf", archive).CombinedOutput(); err != nil {
		t.Fatalf("tar: %v: %s", err, output)
	}
	return filepath.Join(destination, Root)
}

// Unpacked anywhere, the checkout is the one packed: clean, at its commit, its skip-worktree path still skipped, its
// executable still executable, and a file edited to the same size still read as modified (git compares content).
func TestAnUnpackedCheckoutReadsAsItWasPacked(t *testing.T) {
	directory := inputsFixture(t)
	manifest, chunks := pack(t, directory)
	if len(manifest.Chunks) < 2 {
		t.Fatalf("the fixture packs to %d chunk, too few to prove chunks join in order", len(manifest.Chunks))
	}
	unpacked := unpack(t, manifest, chunks)
	checkout := filepath.Join(unpacked, "typescript")
	if status := git(t, checkout, "status", "--porcelain", "--ignored", "--untracked-files=all"); status != "" {
		t.Fatalf("the unpacked checkout isn't clean:\n%s", status)
	}
	if head, packed := git(t, checkout, "rev-parse", "HEAD"), git(t, filepath.Join(directory, "typescript"), "rev-parse", "HEAD"); head != packed {
		t.Fatalf("the unpacked checkout is at %s, packed at %s", head, packed)
	}
	if listed := git(t, checkout, "ls-files", "-v", "tests/far.ts"); listed != "S tests/far.ts\n" {
		t.Fatalf("the skip-worktree path lists as %q", listed)
	}
	if info, err := os.Stat(filepath.Join(checkout, "bin", "tsc")); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("bin/tsc unpacked as %v (%v)", info.Mode(), err)
	}
	if _, err := os.Stat(filepath.Join(unpacked, "cycle-ledger-output.json")); !os.IsNotExist(err) {
		t.Fatalf("the cycle ledger's output was packed: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(unpacked, "json-prettier")); err != nil || target != "css-printer" {
		t.Fatalf("json-prettier links to %q (%v)", target, err)
	}
	write(t, filepath.Join(checkout, "src", "compiler", "parser.ts"), "export const parser = 2;\n", 0o644)
	if status := git(t, checkout, "status", "--porcelain"); status != " M src/compiler/parser.ts\n" {
		t.Fatalf("a same-size edit reads as %q", status)
	}
}

// An index NormalizeIndex can't rewrite whole is refused, never packed half-understood: version 4's compressed
// paths, a split index, a damaged checksum.
func TestNormalizeIndexRefusesWhatItCantRewriteWhole(t *testing.T) {
	directory := inputsFixture(t)
	checkout := filepath.Join(directory, "typescript")
	index := filepath.Join(checkout, ".git", "index")
	healthy, _ := os.ReadFile(index)
	if _, err := NormalizeIndex(healthy); err != nil {
		t.Fatalf("a version 3 index: %v", err)
	}
	damaged := append([]byte{}, healthy...)
	damaged[20] ^= 1
	if _, err := NormalizeIndex(damaged); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("a damaged index: %v", err)
	}
	git(t, checkout, "update-index", "--index-version", "4")
	if version4, _ := os.ReadFile(index); true {
		if _, err := NormalizeIndex(version4); err == nil || !strings.Contains(err.Error(), "version 4") {
			t.Errorf("a version 4 index: %v", err)
		}
	}
	git(t, checkout, "update-index", "--index-version", "3")
	git(t, checkout, "update-index", "--split-index")
	split, _ := os.ReadFile(index)
	if _, err := NormalizeIndex(split); err == nil || !strings.Contains(err.Error(), `"link"`) {
		t.Errorf("a split index: %v", err)
	}
	if _, err := Pack(directory, 0, func(string, []byte) error { return nil }); err == nil || !strings.Contains(err.Error(), "typescript/.git/index") {
		t.Errorf("packing a split index: %v", err)
	}
}

// Pack refuses what would unpack outside the tools directory, and a checkout git is still writing.
func TestPackRefusesALinkOutAndARunningGit(t *testing.T) {
	for name, plant := range map[string]func(directory string){
		"an absolute link": func(directory string) { os.Symlink("/etc", filepath.Join(directory, "etc")) },
		"a link out":       func(directory string) { os.Symlink("../../outside", filepath.Join(directory, "css-printer", "out")) },
		"a git lock": func(directory string) {
			write(t, filepath.Join(directory, "typescript", ".git", "index.lock"), "", 0o644)
		},
	} {
		directory := inputsFixture(t)
		plant(directory)
		if _, err := Pack(directory, 0, func(string, []byte) error { return nil }); err == nil {
			t.Errorf("%s was packed", name)
		}
	}
}

// Publish writes every chunk, then the manifest, under gate-inputs/ and nowhere else, reads it back as a runner
// will, and publishing the same directory again writes nothing.
func TestPublishWritesChunksFirstUnderGateInputsAndNothingTwice(t *testing.T) {
	fake := r2test.New(t)
	directory := inputsFixture(t)
	published, err := Publish(directory, 4096, fake.Bucket(), fake.Public(), fake.Server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := pack(t, directory); published.Name != want.Name || published.Uploaded != len(want.Chunks)+1 {
		t.Fatalf("published %s with %d objects written, packed %s with %d chunks", published.Name, published.Uploaded, want.Name, len(want.Chunks))
	}
	puts := []string{}
	for _, request := range fake.Requests() {
		if method, key, _ := strings.Cut(request, " "); method == http.MethodPut {
			puts = append(puts, key)
		}
	}
	if len(puts) == 0 || puts[len(puts)-1] != Prefix+published.Name {
		t.Fatalf("the manifest wasn't written last: %v", puts)
	}
	for _, key := range puts {
		if !strings.HasPrefix(key, Prefix) {
			t.Fatalf("Publish wrote %s, outside %s", key, Prefix)
		}
	}
	if keys := fake.Keys(""); len(keys) != len(published.Manifest.Chunks)+1 {
		t.Fatalf("the bucket holds %v", keys)
	}
	if body, _ := fake.Object(Prefix + published.Name); !bytes.Equal(body, published.Manifest.Bytes()) {
		t.Fatalf("the manifest reads %q", body)
	}
	fake.ResetRequests()
	again, err := Publish(directory, 4096, fake.Bucket(), fake.Public(), fake.Server.Client())
	if err != nil || again.Name != published.Name || again.Uploaded != 0 || fake.Count(http.MethodPut, "") != 0 {
		t.Fatalf("publishing again: %s, %d written, %d puts, %v", again.Name, again.Uploaded, fake.Count(http.MethodPut, ""), err)
	}
}

// Check refuses every manifest a runner's prepare.sh couldn't read whole: missing, not hashing to its name, not a
// manifest, or naming a chunk the store doesn't hold.
func TestCheckRefusesAManifestNoRunnerCouldRead(t *testing.T) {
	fake := r2test.New(t)
	published, err := Publish(inputsFixture(t), 4096, fake.Bucket(), fake.Public(), fake.Server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Check(fake.Public(), fake.Server.Client(), published.Name); err != nil {
		t.Fatalf("a whole manifest: %v", err)
	}
	notAManifest := []byte("not a manifest\n")
	fake.Set(Prefix+digest(notAManifest), notAManifest, time.Now())
	poisoned := strings.Repeat("a", 64)
	fake.Set(Prefix+poisoned, published.Manifest.Bytes(), time.Now())
	// In order: the chunk goes last, so another tar's manifest is refused for its name alone.
	cases := []struct{ name, hash string }{
		{"missing", strings.Repeat("b", 64)},
		{"another tar's manifest", poisoned},
		{"not a manifest", digest(notAManifest)},
		{"not even a sha256", "../blobs/x"},
		{"missing a chunk", published.Name},
	}
	for _, check := range cases {
		if check.name == "missing a chunk" {
			fake.Delete(Prefix + published.Manifest.Chunks[len(published.Manifest.Chunks)/2])
		}
		if _, err := Check(fake.Public(), fake.Server.Client(), check.hash); err == nil {
			t.Errorf("%s: Check took it", check.name)
		}
	}
}

// A planner's pin rereads its file before every pull, checks each manifest it names once, says when it moved, and
// refuses a file naming a manifest the store doesn't hold.
func TestAPinRereadsItsFileAndChecksEachManifestOnce(t *testing.T) {
	fake := r2test.New(t)
	first, err := Publish(inputsFixture(t), 4096, fake.Bucket(), fake.Public(), fake.Server.Client())
	if err != nil {
		t.Fatal(err)
	}
	moved := inputsFixture(t)
	write(t, filepath.Join(moved, "graphql", "package.json"), "{}", 0o644)
	second, err := Publish(moved, 4096, fake.Bucket(), fake.Public(), fake.Server.Client())
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "gate-inputs-manifest")
	pin := &Pin{File: file, Read: fake.Public(), Client: fake.Server.Client()}
	if _, _, err := pin.Current(); err == nil {
		t.Fatal("a missing file was read")
	}
	WriteFile(file, first.Name)
	fake.ResetRequests()
	for range 2 {
		if hash, moved, err := pin.Current(); err != nil || hash != first.Name || moved {
			t.Fatalf("the first manifest: %s, moved %v, %v", hash, moved, err)
		}
	}
	if reads := fake.Count("PUBLIC", Prefix+first.Name); reads != 1 {
		t.Fatalf("the first manifest was read %d times in two pulls", reads)
	}
	WriteFile(file, second.Name)
	if hash, moved, err := pin.Current(); err != nil || hash != second.Name || !moved {
		t.Fatalf("after a publish: %s, moved %v, %v", hash, moved, err)
	}
	WriteFile(file, strings.Repeat("c", 64))
	if hash, _, err := pin.Current(); err == nil {
		t.Fatalf("a manifest the store doesn't hold was keyed on: %s", hash)
	}
	os.WriteFile(file, []byte("413dd3a5\n"), 0o644)
	if _, _, err := pin.Current(); err == nil {
		t.Fatal("a file holding no sha256 was read")
	}
}

// HomeExpires names any expiring rule whose prefix reaches gate-inputs/: one on the whole bucket, on a prefix of it, or
// on keys under it; the action store's and the releases' rules never do.
func TestHomeExpiresNamesEveryRuleThatReachesThePrefix(t *testing.T) {
	for prefix, expires := range map[string]bool{
		"blobs/": false, "refs/": false, "trees/": false, "releases/": false, "gate-inputsx/": false,
		"": true, "gate": true, "gate-inputs/": true, "gate-inputs/0123": true,
	} {
		rules := []r2.LifecycleRule{{Id: "action store", Prefix: "blobs/", Days: 7}, {Id: "this one", Prefix: prefix, Days: 7}}
		if got := HomeExpires(rules) != nil; got != expires {
			t.Errorf("a rule on %q: expires the home %v, want %v", prefix, got, expires)
		}
	}
}

// The name is the tar's: a gzip that compresses otherwise (a new Go's) makes other chunks and another total, the same
// name, and a publish that finds that name already whole writes nothing.
func TestTheNameIsTheTarsSoACompressionChangeMovesNoKey(t *testing.T) {
	directory := inputsFixture(t)
	fake := r2test.New(t)
	published, err := Publish(directory, 4096, fake.Bucket(), fake.Public(), fake.Server.Client())
	if err != nil {
		t.Fatal(err)
	}
	name, size, err := Identity(directory)
	if err != nil || name != published.Name || size != published.Manifest.Size {
		t.Fatalf("Identity is %s, %d bytes (%v); Pack's %s, %d", name, size, err, published.Name, published.Manifest.Size)
	}
	defer func(level int) { compressionLevel = level }(compressionLevel)
	compressionLevel = gzip.BestSpeed
	faster, _ := pack(t, directory)
	if faster.Total == published.Manifest.Total || faster.Name != published.Name {
		t.Fatalf("another compression: total %.12s (was %.12s), name %.12s (was %.12s)", faster.Total, published.Manifest.Total, faster.Name, published.Name)
	}
	fake.ResetRequests()
	again, err := Publish(directory, 4096, fake.Bucket(), fake.Public(), fake.Server.Client())
	if err != nil || again.Name != published.Name || again.Uploaded != 0 || fake.Count(http.MethodPut, "") != 0 {
		t.Fatalf("publishing under another compression: %.12s, %d written, %d puts, %v", again.Name, again.Uploaded, fake.Count(http.MethodPut, ""), err)
	}
}

// A manifest held by the name whose chunks aren't all there is replaced, its missing chunk written again.
func TestPublishReplacesAManifestWhoseChunksAreGone(t *testing.T) {
	directory := inputsFixture(t)
	fake := r2test.New(t)
	published, err := Publish(directory, 4096, fake.Bucket(), fake.Public(), fake.Server.Client())
	if err != nil {
		t.Fatal(err)
	}
	fake.Delete(Prefix + published.Manifest.Chunks[0])
	again, err := Publish(directory, 4096, fake.Bucket(), fake.Public(), fake.Server.Client())
	if err != nil || again.Uploaded != 2 {
		t.Fatalf("publishing over a manifest missing a chunk: %d written, %v", again.Uploaded, err)
	}
	if _, err := Check(fake.Public(), fake.Server.Client(), published.Name); err != nil {
		t.Fatal(err)
	}
}

// A manifest is read whole or refused: chunks, then the total and the tar, each with its size.
func TestParseManifestRefusesAnythingElse(t *testing.T) {
	a, b, c := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	good := a + "\ntotal " + b + " 12\ntar " + c + " 3072\n"
	if manifest, err := ParseManifest([]byte(good)); err != nil || manifest.Name != c || manifest.Size != 3072 || manifest.Compressed != 12 || len(manifest.Chunks) != 1 {
		t.Fatalf("%+v %v", manifest, err)
	}
	for _, bad := range []string{
		strings.TrimSuffix(good, "\n"), "total " + b + " 12\ntar " + c + " 3072\n", a + "\ntar " + c + " 3072\ntotal " + b + " 12\n",
		a + "\ntotal " + b + "\ntar " + c + " 3072\n", a + "\ntotal " + b + " 12\ntar " + c + " -1\n", a + "\ntotal " + b + " 12\ntar " + c + " 03072\n",
		a + "\ntotal " + b + "  12\ntar " + c + " 3072\n", "x\ntotal " + b + " 12\ntar " + c + " 3072\n", a + "\ntotal " + b + " 12\n",
	} {
		if _, err := ParseManifest([]byte(bad)); err == nil {
			t.Errorf("read %q", bad)
		}
	}
}

// A pin trusts a check for an hour and no longer: a chunk that goes missing (deleted by hand, or a lifecycle rule
// added after the publish) stops the planning at the next check, and a manifest made whole again is taken again.
func TestAPinChecksItsManifestAgainEveryHour(t *testing.T) {
	fake := r2test.New(t)
	published, err := Publish(inputsFixture(t), 4096, fake.Bucket(), fake.Public(), fake.Server.Client())
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "gate-inputs-manifest")
	WriteFile(file, published.Name)
	now := time.Date(2026, 10, 10, 16, 0, 0, 0, time.UTC)
	pin := &Pin{File: file, Read: fake.Public(), Client: fake.Server.Client(), Now: func() time.Time { return now }}
	if _, _, err := pin.Current(); err != nil {
		t.Fatal(err)
	}
	chunk := published.Manifest.Chunks[0]
	held, _ := fake.Object(Prefix + chunk)
	fake.Delete(Prefix + chunk)
	now = now.Add(59 * time.Minute)
	if _, _, err := pin.Current(); err != nil {
		t.Fatalf("within the hour: %v", err)
	}
	now = now.Add(time.Minute)
	if hash, _, err := pin.Current(); err == nil {
		t.Fatalf("an hour on, a manifest missing chunk %.12s was keyed on: %s", chunk, hash)
	}
	if hash, _, err := pin.Current(); err == nil {
		t.Fatalf("the pull after a failed check keyed on %s", hash)
	}
	fake.Set(Prefix+chunk, held, now)
	if _, _, err := pin.Current(); err != nil {
		t.Fatalf("made whole again: %v", err)
	}
}
