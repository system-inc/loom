// Package gateinputs publishes the gate inputs and checks them for their readers. The gate inputs are the files
// adamic's corpus and parity tests read beside the tree, under ~/adamic-tools/gate-inputs on a box (adamic's
// docs/gate-inputs.md): the pinned TypeScript checkout and the cycle ledger's, the CSS fixtures, the formatters' npm
// projects, the 100 MiB ignore corpus and the checker archive. A unit runs with them by one name, the sha256 of their
// uncompressed tar, which the planner reads from ~/.loom/gate-inputs-manifest into every unit's key and test job
// (protocol.TestJob.GateInputs, planner.KeyParts.GateInputs), and which a runner's prepare.sh fetches, checks and
// unpacks into <root>/adamic-tools/gate-inputs.
//
// The manifest, at gate-inputs/<name> in the public bucket, is text: one line per chunk of a tar.gz of that tar, each
// chunk's sha256 in order, then "total <sha256> <bytes>" of the whole tar.gz, then "tar <name> <bytes>". A runner
// checks every chunk, the total, and the tar's own sha256 against the name it was given, so the manifest needn't be
// trusted, and makes room for both sizes before it fetches anything. Chunks live beside it at gate-inputs/<sha256>,
// a prefix no lifecycle rule names, so nothing expires out from under the units keyed on it (the first manifest, made
// by hand on Oct 8, lived in blobs/, which expires after 7 days, and was gone by the first witness).
//
// The name is the tar's, not the tar.gz's, so a gzip that compresses differently (a new Go) moves no key. The tar is
// deterministic, so the name is a function of what the tests read and nothing else: entries in byte order, every time
// the same fixed time, no owner, a file's mode only 0644 or 0755, a hard link packed as its file. What a box's own runs
// write into the directory is left out or normalized, so publishing the directory a box gate runs against gives the
// same name before and after its tests: the cycle ledger's output file is never packed, and every git index (a
// checkout's, a submodule's, a worktree's) is packed with its stat data zeroed (as `git read-tree` leaves it), which
// git reads by comparing content. The same bytes always give the same name, so publishing again moves no key, and any
// byte that changes moves every key that names it.
package gateinputs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/system-inc/loom/r2"
)

// Prefix is where the gate inputs' chunks and manifests live in the public bucket, each at Prefix + its sha256.
const Prefix = "gate-inputs/"

// ChunkSize is how much of the tar.gz one chunk holds, as the Oct 8 manifest cut it.
const ChunkSize = 90 << 20

// Root is the directory every entry of the tar.gz is under, the name prepare.sh unpacks into its tools directory.
const Root = "gate-inputs"

// Excluded are the paths under the directory that are a box's own outputs, never inputs: prepare.sh points
// ADAMIC_CYCLE_LEDGER_OUTPUT at the first, and the cycle ledger's test writes it on every run.
var Excluded = map[string]bool{"cycle-ledger-output.json": true}

// Epoch is every entry's modification time.
var Epoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Sha256Hex says whether text is a sha256, 64 lowercase hex digits.
func Sha256Hex(text string) bool {
	return sha256Pattern.MatchString(text)
}

func digest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// A Manifest is the tar.gz's chunks by sha256, in order, the whole tar.gz's sha256 and size, and the tar's: Name, the
// gate inputs' one name in every key that runs with them, and Size, about what they take unpacked.
type Manifest struct {
	Chunks     []string
	Total      string
	Compressed int64
	Name       string
	Size       int64
}

// Bytes is the manifest as prepare.sh reads it.
func (manifest Manifest) Bytes() []byte {
	var text bytes.Buffer
	for _, chunk := range manifest.Chunks {
		text.WriteString(chunk + "\n")
	}
	fmt.Fprintf(&text, "total %s %d\ntar %s %d\n", manifest.Total, manifest.Compressed, manifest.Name, manifest.Size)
	return text.Bytes()
}

// sizedLine reads "<word> <sha256> <bytes>".
func sizedLine(line, word string) (string, int64, error) {
	fields := strings.Fields(line)
	if len(fields) == 3 && fields[0] == word && Sha256Hex(fields[1]) && line == strings.Join(fields, " ") {
		if size, err := strconv.ParseInt(fields[2], 10, 64); err == nil && size >= 0 && strconv.FormatInt(size, 10) == fields[2] {
			return fields[1], size, nil
		}
	}
	return "", 0, fmt.Errorf("a manifest's %s line is %s <sha256> <bytes>, not %q", word, word, line)
}

// ParseManifest reads a manifest, refusing anything but at least one chunk line, each a sha256, then the total line,
// then the tar line.
func ParseManifest(content []byte) (Manifest, error) {
	text, found := strings.CutSuffix(string(content), "\n")
	if !found {
		return Manifest{}, fmt.Errorf("a manifest ends with a newline")
	}
	lines := strings.Split(text, "\n")
	if len(lines) < 3 {
		return Manifest{}, fmt.Errorf("a manifest names at least one chunk, its total and its tar")
	}
	manifest := Manifest{}
	var err error
	if manifest.Total, manifest.Compressed, err = sizedLine(lines[len(lines)-2], "total"); err != nil {
		return Manifest{}, err
	}
	if manifest.Name, manifest.Size, err = sizedLine(lines[len(lines)-1], "tar"); err != nil {
		return Manifest{}, err
	}
	for index, line := range lines[:len(lines)-2] {
		if !Sha256Hex(line) {
			return Manifest{}, fmt.Errorf("a manifest's line %d is a chunk's sha256, not %q", index+1, line)
		}
		manifest.Chunks = append(manifest.Chunks, line)
	}
	return manifest, nil
}

// counter hashes and counts what it is written.
type counter struct {
	hash  hashWriter
	bytes int64
}

func (count *counter) Write(content []byte) (int, error) {
	count.bytes += int64(len(content))
	return count.hash.Write(content)
}

// chunker cuts what it is written into chunks of size bytes, handing each to emit, and hashes the whole.
type chunker struct {
	size   int
	buffer []byte
	whole  hashWriter
	emit   func(hash string, content []byte) error
	chunks []string
	sizes  []int64
}

type hashWriter interface {
	io.Writer
	Sum([]byte) []byte
}

func (chunks *chunker) Write(content []byte) (int, error) {
	written := len(content)
	chunks.whole.Write(content)
	for len(content) > 0 {
		room := chunks.size - len(chunks.buffer)
		take := min(room, len(content))
		chunks.buffer = append(chunks.buffer, content[:take]...)
		content = content[take:]
		if len(chunks.buffer) == chunks.size {
			if err := chunks.flush(); err != nil {
				return 0, err
			}
		}
	}
	return written, nil
}

func (chunks *chunker) flush() error {
	if len(chunks.buffer) == 0 {
		return nil
	}
	hash := digest(chunks.buffer)
	chunks.chunks, chunks.sizes = append(chunks.chunks, hash), append(chunks.sizes, int64(len(chunks.buffer)))
	if err := chunks.emit(hash, chunks.buffer); err != nil {
		return err
	}
	chunks.buffer = make([]byte, 0, chunks.size)
	return nil
}

// compressionLevel is the tar.gz's; the name doesn't depend on it.
var compressionLevel = gzip.DefaultCompression

// Identity is the gate inputs' name and the tar's size, without compressing or cutting anything: what Pack's would be.
func Identity(directory string) (string, int64, error) {
	tarred := &counter{hash: sha256.New()}
	if err := writeTar(directory, tarred); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(tarred.hash.Sum(nil)), tarred.bytes, nil
}

// Pack writes the directory as one deterministic tar under Root, gzipped and cut into chunks of chunkSize bytes
// (ChunkSize when zero), handing each chunk to emit in order as it is cut, and returns the manifest. It refuses what
// can't be packed the same way twice or would unpack outside the tools directory: a special file, a symbolic link that
// is absolute or resolves outside the directory, a git lock (a git command was running in it), or a git index it
// can't read.
func Pack(directory string, chunkSize int, emit func(hash string, content []byte) error) (Manifest, error) {
	if chunkSize <= 0 {
		chunkSize = ChunkSize
	}
	chunks := &chunker{size: chunkSize, buffer: make([]byte, 0, chunkSize), whole: sha256.New(), emit: emit}
	compressed, err := gzip.NewWriterLevel(chunks, compressionLevel)
	if err != nil {
		return Manifest{}, err
	}
	tarred := &counter{hash: sha256.New()}
	err = writeTar(directory, io.MultiWriter(tarred, compressed))
	if err == nil {
		err = compressed.Close()
	}
	if err == nil {
		err = chunks.flush()
	}
	if err != nil {
		return Manifest{}, err
	}
	compressedBytes := int64(0)
	for _, size := range chunks.sizes {
		compressedBytes += size
	}
	return Manifest{Chunks: chunks.chunks, Total: hex.EncodeToString(chunks.whole.Sum(nil)), Compressed: compressedBytes,
		Name: hex.EncodeToString(tarred.hash.Sum(nil)), Size: tarred.bytes}, nil
}

// writeTar writes the directory as Pack's tar.
func writeTar(directory string, out io.Writer) error {
	directory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return err
	}
	if info, err := os.Stat(directory); err != nil {
		return err
	} else if !info.IsDir() {
		return fmt.Errorf("%s isn't a directory", directory)
	}
	archive := tar.NewWriter(out)
	err = filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if Excluded[relative] {
			return nil
		}
		name := Root
		if relative != "." {
			name += "/" + relative
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header := &tar.Header{Name: name, ModTime: Epoch, Mode: 0o644}
		switch {
		case info.IsDir():
			header.Typeflag, header.Name, header.Mode = tar.TypeDir, name+"/", 0o755
			return archive.WriteHeader(header)
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			// The link is followed all the way, through every other link on its path, as a reader would.
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("%s links to %s, which doesn't resolve: %w", relative, target, err)
			}
			if within, err := filepath.Rel(directory, resolved); filepath.IsAbs(target) || err != nil || within == ".." ||
				strings.HasPrefix(within, ".."+string(filepath.Separator)) {
				return fmt.Errorf("%s links to %s, which resolves outside the gate inputs", relative, target)
			}
			header.Typeflag, header.Linkname, header.Mode = tar.TypeSymlink, target, 0o777
			return archive.WriteHeader(header)
		case !info.Mode().IsRegular():
			return fmt.Errorf("%s is a %s, not a file, directory or link", relative, info.Mode().Type())
		}
		if strings.HasSuffix(relative, ".lock") && strings.Contains("/"+relative, "/.git/") {
			return fmt.Errorf("%s: a git command is running in the gate inputs, or died leaving its lock", relative)
		}
		if info.Mode()&0o111 != 0 {
			header.Mode = 0o755
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Every git directory's index: a checkout's .git/index, a submodule's .git/modules/<name>/index, a worktree's
		// .git/worktrees/<name>/index. A file named index elsewhere in a git directory (a branch named index) isn't one.
		if filepath.Base(relative) == "index" && strings.Contains("/"+relative, "/.git/") && bytes.HasPrefix(content, []byte("DIRC")) {
			if content, err = NormalizeIndex(content); err != nil {
				return fmt.Errorf("%s: %w", relative, err)
			}
		}
		header.Typeflag, header.Size = tar.TypeReg, int64(len(content))
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		_, err = archive.Write(content)
		return err
	})
	if err != nil {
		return err
	}
	return archive.Close()
}

// put writes content at key unless the bucket already holds it: a key is its content's sha256, so a held key of the
// same size is the same bytes, and one of another size is a store that isn't what its names say.
func put(bucket r2.Bucket, key string, content []byte, contentType string) (bool, error) {
	held, err := bucket.Head(key)
	switch {
	case err == nil && held.Size == int64(len(content)):
		return false, nil
	case err == nil:
		return false, fmt.Errorf("%s holds %d bytes, not the %d its name says", key, held.Size, len(content))
	case !errors.Is(err, r2.ErrNotFound):
		return false, err
	}
	err = bucket.Put(key, content, r2.PutOptions{ContentType: contentType, CacheControl: "public, max-age=31536000, immutable", IfNoneMatch: true})
	if errors.Is(err, r2.ErrExists) {
		return false, nil
	}
	return err == nil, err
}

// A Published manifest is what Publish made, or found already published, and how much of it the bucket lacked.
type Published struct {
	Manifest Manifest
	Name     string
	Bytes    int64
	Uploaded int
}

// Publish names the directory (Identity) and, when the public domain already holds a whole manifest by that name,
// however its tar.gz was compressed, writes nothing. Otherwise it packs the directory and writes every chunk, then the
// manifest, under Prefix, each only if the bucket lacks it, so a manifest is never there before its chunks; one held
// by that name whose chunks aren't all there is replaced. It then reads the manifest back from the public domain,
// read, as every runner will (Check), and returns it.
func Publish(directory string, chunkSize int, bucket r2.Bucket, read string, client *http.Client) (Published, error) {
	name, _, err := Identity(directory)
	if err != nil {
		return Published{}, err
	}
	if held, err := Check(read, client, name); err == nil {
		return Published{Manifest: held, Name: name}, nil
	}
	published := Published{}
	manifest, err := Pack(directory, chunkSize, func(hash string, content []byte) error {
		published.Bytes += int64(len(content))
		uploaded, err := put(bucket, Prefix+hash, content, "application/gzip")
		if uploaded {
			published.Uploaded++
		}
		return err
	})
	if err != nil {
		return Published{}, err
	}
	if manifest.Name != name {
		return Published{}, fmt.Errorf("%s changed while it was packed: named %.12s, packed %.12s", directory, name, manifest.Name)
	}
	published.Manifest, published.Name = manifest, name
	options := r2.PutOptions{ContentType: "text/plain; charset=utf-8", CacheControl: "no-cache", IfNoneMatch: true}
	if held, err := bucket.Head(Prefix + name); err == nil {
		options.IfNoneMatch, options.IfMatch = false, held.ETag
	} else if !errors.Is(err, r2.ErrNotFound) {
		return Published{}, err
	}
	if err = bucket.Put(Prefix+name, manifest.Bytes(), options); err != nil {
		return Published{}, fmt.Errorf("the manifest %s: %w", name, err)
	}
	published.Uploaded++
	if _, err = Check(read, client, name); err != nil {
		return Published{}, fmt.Errorf("published %s, but it doesn't read back: %w", name, err)
	}
	return published, nil
}

// HomeExpires is the lifecycle rule that would delete what Publish writes, or nil: an enabled expiring rule whose
// prefix is Prefix, a prefix of it (the empty prefix is every key), or a key under it.
func HomeExpires(rules []r2.LifecycleRule) *r2.LifecycleRule {
	for index, rule := range rules {
		if strings.HasPrefix(Prefix, rule.Prefix) || strings.HasPrefix(rule.Prefix, Prefix) {
			return &rules[index]
		}
	}
	return nil
}

// Check reads the manifest of the gate inputs named hash from the public domain, read, as a runner's prepare.sh will:
// it must parse and name that tar, and the domain must hold every chunk it names. A runner checks each chunk's bytes
// and the tar's as it fetches them; this checks only that they are there, so a planner can refuse to key units on gate
// inputs no runner could read.
func Check(read string, client *http.Client, hash string) (Manifest, error) {
	if !Sha256Hex(hash) {
		return Manifest{}, fmt.Errorf("the gate inputs are named by their tar's sha256, not %q", hash)
	}
	if client == nil {
		client = &http.Client{Timeout: time.Minute}
	}
	base := strings.TrimSuffix(read, "/") + "/" + Prefix
	response, err := client.Get(base + hash)
	if err != nil {
		return Manifest{}, err
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	response.Body.Close()
	if err != nil {
		return Manifest{}, err
	}
	if response.StatusCode != http.StatusOK {
		return Manifest{}, fmt.Errorf("%s%s answered %s", base, hash, response.Status)
	}
	manifest, err := ParseManifest(content)
	if err != nil {
		return Manifest{}, fmt.Errorf("%s%s: %w", base, hash, err)
	}
	if manifest.Name != hash {
		return Manifest{}, fmt.Errorf("%s%s is the manifest of the tar %s", base, hash, manifest.Name)
	}
	for _, chunk := range manifest.Chunks {
		response, err := client.Head(base + chunk)
		if err != nil {
			return Manifest{}, err
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return Manifest{}, fmt.Errorf("manifest %.12s names chunk %s, and %s%s answered %s", hash, chunk, base, chunk, response.Status)
		}
	}
	return manifest, nil
}

// ReadFile reads a manifest file as the planner keeps it: the gate inputs' name on one line.
func ReadFile(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	hash := strings.TrimSpace(string(content))
	if !Sha256Hex(hash) {
		return "", fmt.Errorf("%s holds %q, not the gate inputs' name, a sha256", path, hash)
	}
	return hash, nil
}

// WriteFile writes the manifest file whole or not at all: a reader never sees half a hash.
func WriteFile(path, hash string) error {
	if !Sha256Hex(hash) {
		return fmt.Errorf("%q isn't the gate inputs' name, a sha256", hash)
	}
	partial := path + ".partial"
	if err := os.WriteFile(partial, []byte(hash+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(partial, path)
}

// RecheckEvery is how long a Pin trusts a check of the manifest it names before it checks it again.
const RecheckEvery = time.Hour

// A Pin is the planner's gate inputs: File, reread before every pull, so a publish reaches the next plan without a
// restart, and the manifest it names checked in the public domain, Read, before any unit is keyed on it and again
// every Every (RecheckEvery when zero), so a chunk that goes missing stops the planning within the hour. Now is the
// clock (nil means time.Now).
type Pin struct {
	File    string
	Read    string
	Client  *http.Client
	Every   time.Duration
	Now     func() time.Time
	current string
	checked map[string]time.Time
}

// Last is the name Current last returned, empty before the first.
func (pin *Pin) Last() string {
	return pin.current
}

// Current is the name File holds, checked; moved says it differs from the one Current last returned. A file that
// doesn't hold a sha256, or a name whose manifest the public domain doesn't hold whole, is an error, and the pull that
// asked keys nothing on it.
func (pin *Pin) Current() (string, bool, error) {
	hash, err := ReadFile(pin.File)
	if err != nil {
		return "", false, err
	}
	now, every := time.Now(), pin.Every
	if pin.Now != nil {
		now = pin.Now()
	}
	if every <= 0 {
		every = RecheckEvery
	}
	if checked, found := pin.checked[hash]; !found || now.Sub(checked) >= every {
		if _, err := Check(pin.Read, pin.Client, hash); err != nil {
			delete(pin.checked, hash)
			return "", false, fmt.Errorf("the gate inputs %s names: %w", pin.File, err)
		}
		if pin.checked == nil {
			pin.checked = map[string]time.Time{}
		}
		pin.checked[hash] = now
	}
	moved := pin.current != "" && pin.current != hash
	pin.current = hash
	return hash, moved, nil
}
