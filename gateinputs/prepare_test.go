package gateinputs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/r2/r2test"
)

// prepare.sh's gate inputs, run as the runner runs them. Each mutant below must make a test here fail:
//
//	reading the gate inputs from blobs/ (which expires), or not as Pack writes them: TestPrepareUnpacksWhatPublishWrote
//	the reviewed order (the inputs moved aside, the new ones unpacked in place, the marker rewritten after), or only
//	unpacking in place, or the marker not removed first, or staging kept after a failure:
//	TestAFailedUnpackLeavesNoMarkerAndNoStaging
//	the trim not taking an earlier unit's staging: TestTheTrimTakesAKilledUnitsStaging
//	no room check before the fetch, or one that counts only the floor: TestPrepareMakesRoomForBothSizesBeforeItFetches
//	the tar's sha256 not checked against the job's name: TestPrepareChecksTheTarAgainstTheJobsName

// prepareTools skips a test on a machine without what a runner's prepare.sh uses: coreutils' sha256sum among them.
func prepareTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "curl", "sha256sum", "tar", "gzip"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("prepare.sh needs %s", tool)
		}
	}
	probe := exec.Command("bash", "-c", `f=$(mktemp) && echo "$(sha256sum "$f" | cut -c1-64)  $f" | sha256sum -c --quiet`)
	if output, err := probe.CombinedOutput(); err != nil {
		t.Skipf("prepare.sh needs coreutils' sha256sum (a runner's is), and this one isn't: %s", output)
	}
}

func prepareScript(t *testing.T) string {
	t.Helper()
	script, err := os.ReadFile(filepath.Join("..", "runner", "prepare.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return string(script)
}

// prepareSection is prepare.sh's gate inputs section, its store's address turned to the fake's, run alone: run it
// with a root, a manifest's name and extra environment, and it returns what it printed and whether it exited 0.
func prepareSection(t *testing.T, fake *r2test.Fake) func(root, name string, environment ...string) (string, bool) {
	t.Helper()
	prepareTools(t)
	text := prepareScript(t)
	start := strings.Index(text, "# The gate inputs, from Loom's public store")
	end := strings.Index(text, "# Every Go module's dependencies")
	if start < 0 || end < start {
		t.Fatal("prepare.sh's gate inputs section isn't where this test looks")
	}
	section := text[start:end]
	store := "https://artifacts.loom.system.inc/" + Prefix + "$1"
	if !strings.Contains(section, store) {
		t.Fatalf("prepare.sh doesn't read the gate inputs from %s", store)
	}
	section = strings.ReplaceAll(section, "https://artifacts.loom.system.inc/", fake.Public()+"/")
	text = text[:strings.Index(text, "if [ \"${1:-}\" = trim-only ]")]
	return func(root, name string, environment ...string) (string, bool) {
		command := exec.Command("bash", "-c", text+"\nroot=$1 gateInputs=$2 owner=shared\n"+section+
			"\necho \"source=${ADAMIC_TYPESCRIPT_SOURCE}\"\n", "prepare", root, name)
		command.Env = append(append(os.Environ(), "HOME="+t.TempDir()), environment...)
		output, err := command.CombinedOutput()
		return string(output), err == nil
	}
}

// prepare.sh reads the gate inputs from where Publish writes them and unpacks what Pack made into a checkout that
// reads clean; a chunk that isn't its hash's bytes stops it, exit 2.
func TestPrepareUnpacksWhatPublishWrote(t *testing.T) {
	fake := r2test.New(t)
	run := prepareSection(t, fake)
	published, err := Publish(inputsFixture(t), 4096, fake.Bucket(), fake.Public(), fake.Server.Client())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	output, ok := run(root, published.Name)
	if !ok {
		t.Fatalf("prepare.sh: %s", output)
	}
	checkout := filepath.Join(root, "adamic-tools", Root, "typescript")
	if !strings.Contains(output, "source="+checkout) {
		t.Fatalf("ADAMIC_TYPESCRIPT_SOURCE isn't the unpacked checkout: %s", output)
	}
	if status := git(t, checkout, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("the checkout prepare.sh unpacked isn't clean:\n%s", status)
	}
	if marker, _ := os.ReadFile(filepath.Join(root, "adamic-tools", "gate-inputs.manifest")); strings.TrimSpace(string(marker)) != published.Name {
		t.Fatalf("prepare.sh marked %q", marker)
	}
	chunk := published.Manifest.Chunks[0]
	held, _ := fake.Object(Prefix + chunk)
	fake.Set(Prefix+chunk, append(held[:len(held)-1:len(held)-1], held[len(held)-1]^1), time.Now())
	if output, ok := run(t.TempDir(), published.Name); ok || !strings.Contains(output, "gate inputs chunk "+chunk+" failed") {
		t.Fatalf("a poisoned chunk: %v: %s", ok, output)
	}
}

// An unpack that fails (a full disk, the unit's deadline) leaves no marker and no staging, and the inputs it would
// have replaced whole, never half of each: the next unit on the manifest the root had before fetches it again and
// reads its own files.
func TestAFailedUnpackLeavesNoMarkerAndNoStaging(t *testing.T) {
	fake := r2test.New(t)
	run := prepareSection(t, fake)
	first, err := Publish(inputsFixture(t), 4096, fake.Bucket(), fake.Public(), fake.Server.Client())
	if err != nil {
		t.Fatal(err)
	}
	moved := inputsFixture(t)
	write(t, filepath.Join(moved, "css-printer", "package.json"), `{"moved":1}`, 0o644)
	second, err := Publish(moved, 4096, fake.Bucket(), fake.Public(), fake.Server.Client())
	if err != nil {
		t.Fatal(err)
	}
	realTar, _ := exec.LookPath("tar")
	stubs, failing := t.TempDir(), filepath.Join(t.TempDir(), "fail")
	// The stub tar unpacks all it can, then fails as a full disk does.
	write(t, filepath.Join(stubs, "tar"), "#!/bin/bash\nif [ -f '"+failing+"' ]; then '"+realTar+"' \"$@\" 2> /dev/null; echo 'tar: No space left on device' >&2; exit 2; fi\nexec '"+realTar+"' \"$@\"\n", 0o755)
	path := "PATH=" + stubs + ":" + os.Getenv("PATH")
	root := t.TempDir()
	marker := filepath.Join(root, "adamic-tools", "gate-inputs.manifest")
	if output, ok := run(root, first.Name, path); !ok {
		t.Fatalf("the first unit: %s", output)
	}
	os.WriteFile(failing, nil, 0o644)
	if output, ok := run(root, second.Name, path); ok || !strings.Contains(output, "unpack failed") {
		t.Fatalf("an unpack that fails: %v: %s", ok, output)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		held, _ := os.ReadFile(marker)
		t.Fatalf("after a failed unpack the marker names %.12s", held)
	}
	if staging, _ := filepath.Glob(filepath.Join(root, "adamic-tools", "staging-*")); len(staging) != 0 {
		t.Fatalf("a failed unpack left %v", staging)
	}
	if read, _ := os.ReadFile(filepath.Join(root, "adamic-tools", Root, "css-printer", "package.json")); string(read) != `{"dependencies":{"prettier":"3.9.6"}}` {
		t.Fatalf("a failed unpack left its half in place of the inputs it replaced: css-printer/package.json is %s", read)
	}
	os.Remove(failing)
	if output, ok := run(root, first.Name, path); !ok {
		t.Fatalf("the next unit: %s", output)
	}
	if read, _ := os.ReadFile(filepath.Join(root, "adamic-tools", Root, "css-printer", "package.json")); string(read) != `{"dependencies":{"prettier":"3.9.6"}}` {
		t.Fatalf("the next unit on the first manifest reads css-printer/package.json as %s", read)
	}
}

// A unit killed mid-fetch can't clean up after itself; the next strict unit's trim takes its staging, and leaves the
// unpacked inputs and their marker.
func TestTheTrimTakesAKilledUnitsStaging(t *testing.T) {
	prepareTools(t)
	root := t.TempDir()
	write(t, filepath.Join(root, "adamic-tools", "staging-4242", "gate-inputs.tar.gz"), "half", 0o644)
	write(t, filepath.Join(root, "adamic-tools", "gate-inputs.manifest"), strings.Repeat("a", 64)+"\n", 0o644)
	write(t, filepath.Join(root, "adamic-tools", Root, "css", "package.json"), "{}", 0o644)
	command := exec.Command("bash", filepath.Join("..", "runner", "prepare.sh"), "trim-only", root, "shared")
	command.Env = append(os.Environ(), "HOME="+t.TempDir())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("trim-only: %v: %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(root, "adamic-tools", "staging-4242")); !os.IsNotExist(err) {
		t.Fatalf("the trim left a killed unit's staging: %v", err)
	}
	for _, kept := range []string{"gate-inputs.manifest", "gate-inputs/css/package.json"} {
		if _, err := os.Stat(filepath.Join(root, "adamic-tools", kept)); err != nil {
			t.Fatalf("the trim took %s: %v", kept, err)
		}
	}
}

// Nothing is fetched until the root has room for the tar.gz and the unpacked inputs, both sizes the manifest's, above
// the 1500 MB floor: a 1.5 GB fetch doesn't start on a disk with 1.5 GB free.
func TestPrepareMakesRoomForBothSizesBeforeItFetches(t *testing.T) {
	fake := r2test.New(t)
	run := prepareSection(t, fake)
	directory := inputsFixture(t)
	noise := make([]byte, 3<<20)
	for index := range noise {
		noise[index] = byte(index*7919>>3 ^ index>>11)
	}
	write(t, filepath.Join(directory, "gitignore", ".gitignore"), string(noise), 0o644)
	published, err := Publish(directory, 1<<20, fake.Bucket(), fake.Public(), fake.Server.Client())
	if err != nil {
		t.Fatal(err)
	}
	need := (published.Manifest.Compressed+published.Manifest.Size)/(1<<20) + 1500
	if need < 1503 {
		t.Fatalf("the fixture needs %d MB, too little above the floor to tell its sizes were counted", need)
	}
	stubs := t.TempDir()
	df := func(free int64) string {
		write(t, filepath.Join(stubs, "df"), "#!/bin/bash\necho 'Filesystem 1048576-blocks Used Available Capacity Mounted on'\n"+
			"for path in \"${@:2}\"; do echo \"/dev/stub 9999 1 "+fmt.Sprint(free)+" 1% /\"; done\n", 0o755)
		return "PATH=" + stubs + ":" + os.Getenv("PATH")
	}
	fake.ResetRequests()
	output, ok := run(t.TempDir(), published.Name, df(need-1))
	if ok || !strings.Contains(output, fmt.Sprintf("the gate inputs need %d MB", need)) {
		t.Fatalf("%d MB free for %d MB: %v: %s", need-1, need, ok, output)
	}
	for _, chunk := range published.Manifest.Chunks {
		if fake.Count("PUBLIC", Prefix+chunk) != 0 {
			t.Fatalf("chunk %.12s was fetched onto a disk without room for it", chunk)
		}
	}
	if output, ok := run(t.TempDir(), published.Name, df(need)); !ok {
		t.Fatalf("%d MB free for %d MB: %s", need, need, output)
	}
}

// The manifest needn't be trusted: one that names another tar is refused, and one whose chunks and total are whole
// but make another tar than the job's name is refused before anything is unpacked.
func TestPrepareChecksTheTarAgainstTheJobsName(t *testing.T) {
	fake := r2test.New(t)
	run := prepareSection(t, fake)
	first, err := Publish(inputsFixture(t), 4096, fake.Bucket(), fake.Public(), fake.Server.Client())
	if err != nil {
		t.Fatal(err)
	}
	other := inputsFixture(t)
	write(t, filepath.Join(other, "css-printer", "package.json"), `{"moved":1}`, 0o644)
	second, err := Publish(other, 4096, fake.Bucket(), fake.Public(), fake.Server.Client())
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := strings.Repeat("d", 64)
	fake.Set(Prefix+elsewhere, first.Manifest.Bytes(), time.Now())
	if output, ok := run(t.TempDir(), elsewhere); ok || !strings.Contains(output, "a tar other than "+elsewhere) {
		t.Fatalf("a manifest naming another tar: %v: %s", ok, output)
	}
	swapped := second.Manifest
	swapped.Name, swapped.Size = first.Name, first.Manifest.Size
	fake.Set(Prefix+first.Name, swapped.Bytes(), time.Now())
	root := t.TempDir()
	if output, ok := run(root, first.Name); ok || !strings.Contains(output, "gate inputs tar isn't "+first.Name) {
		t.Fatalf("another tar's chunks under the first's name: %v: %s", ok, output)
	}
	if _, err := os.Stat(filepath.Join(root, "adamic-tools", Root)); !os.IsNotExist(err) {
		t.Fatalf("another tar was unpacked under the first's name: %v", err)
	}
}
