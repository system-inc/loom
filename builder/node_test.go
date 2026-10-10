package builder

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// nodeTree is a git tree that tracks stage3/api's package.json, lockfile and an ignore of node_modules, beside many other
// files, and an install of it outside the tree: many files, more than one chunk weighs, an executable and a link.
func nodeTree(t *testing.T) (string, NodeInstall) {
	t.Helper()
	files := manyFiles(200)
	files["stage3/api/.gitignore"] = "node_modules\n"
	files["stage3/api/package.json"] = `{"name":"api"}` + "\n"
	files["stage3/api/package-lock.json"] = `{"name":"api","lockfileVersion":3}` + "\n"
	tree := gitTree(t, files)
	install := NodeInstall{Directory: "stage3/api", Lockfile: keyOf(files["stage3/api/package-lock.json"]), NodeModules: filepath.Join(t.TempDir(), "node_modules")}
	for index := range 120 {
		name := filepath.Join(install.NodeModules, fmt.Sprintf("typescript/lib/file%03d.d.ts", index))
		os.MkdirAll(filepath.Dir(name), 0o755)
		os.WriteFile(name, bytes.Repeat([]byte(fmt.Sprintf("declare const x%d: number;\n", index)), 20), 0o644)
	}
	os.MkdirAll(filepath.Join(install.NodeModules, "typescript", "bin"), 0o755)
	os.WriteFile(filepath.Join(install.NodeModules, "typescript", "bin", "tsc"), []byte("#!/usr/bin/env node\n"), 0o755)
	os.MkdirAll(filepath.Join(install.NodeModules, ".bin"), 0o755)
	os.Symlink("../typescript/bin/tsc", filepath.Join(install.NodeModules, ".bin", "tsc"))
	os.WriteFile(filepath.Join(install.NodeModules, ".package-lock.json"), []byte("{}\n"), 0o644)
	return tree, install
}

// An install is one chunk of the source, whatever it weighs, holding exactly its node_modules: the same install makes
// the same chunk, and a change elsewhere in the tree leaves it as it was, so a kept tree keeps it and a warm runner
// unpacks nothing.
func TestAnInstallIsOneChunkOfTheSource(t *testing.T) {
	withChunkTarget(t, 4<<10)
	tree, install := nodeTree(t)
	source, err := SourceChunks(tree, []NodeInstall{install})
	if err != nil {
		t.Fatal(err)
	}
	if err = CheckChunks(source.Chunks); err != nil {
		t.Fatal(err)
	}
	node, err := NodeChunks(source, []NodeInstall{install})
	if err != nil {
		t.Fatal(err)
	}
	chunk := source.Chunks[slices.IndexFunc(source.Chunks, func(chunk SourceChunk) bool { return chunk.Blob == node[0].Chunk })]
	// 120 declarations, tsc, its link and npm's own lockfile: about 72 KB, eighteen times the 4 KiB a chunk weighs.
	if chunk.Files != 123 || chunk.First != "stage3/api/node_modules/.bin/tsc" || chunk.Last != "stage3/api/node_modules/typescript/lib/file119.d.ts" {
		t.Fatalf("the install's chunk: %+v", chunk)
	}
	if node[0].Lockfile != install.Lockfile || node[0].Directory != "stage3/api" {
		t.Errorf("the index names %+v", node[0])
	}
	unpacked := t.TempDir()
	if err = UnpackChunk(bytes.NewReader(source.Blobs[chunk.Blob]), unpacked, chunk); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(unpacked, "stage3/api/node_modules/.bin/tsc")); err != nil || target != "../typescript/bin/tsc" {
		t.Errorf("the link is %q, %v", target, err)
	}
	if info, err := os.Stat(filepath.Join(unpacked, "stage3/api/node_modules/typescript/bin/tsc")); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("tsc is %v, %v", info, err)
	}
	// Every tracked file is still in the source, and nothing of the install is in any other chunk.
	for _, name := range []string{"stage3/api/.gitignore", "stage3/api/package-lock.json", "stage3/api/package.json"} {
		if !slices.ContainsFunc(source.Chunks, func(other SourceChunk) bool { return other.Blob != chunk.Blob && other.Holds(name) }) {
			t.Errorf("no other chunk holds %s", name)
		}
	}
	os.WriteFile(filepath.Join(tree, "p0/q0/file000.txt"), []byte("changed\n"), 0o644)
	gitCommit(t, tree)
	again, err := SourceChunks(tree, []NodeInstall{install})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(again.Chunks, chunk) || !bytes.Equal(again.Blobs[chunk.Blob], source.Blobs[chunk.Blob]) {
		t.Error("a change elsewhere in the tree changed the install's chunk")
	}
}

// A tree that tracks a path inside an install's node_modules would split its chunk, and is refused at build time.
func TestATrackedPathInsideAnInstallIsRefused(t *testing.T) {
	_, install := nodeTree(t)
	tree := gitTree(t, map[string]string{"stage3/api/package-lock.json": "{}\n", "stage3/api/node_modules/kept.txt": "tracked\n"})
	if _, err := SourceChunks(tree, []NodeInstall{install}); err == nil || !strings.Contains(err.Error(), "which Workshop installs") {
		t.Fatalf("a tracked path inside node_modules: %v", err)
	}
}

// A lockfile already installed under the base is never installed again, and npm is never fetched; a tree with no
// lockfile has nothing to install.
func TestAnInstallIsMadeOncePerLockfile(t *testing.T) {
	registry := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Errorf("the registry was asked for %s", request.URL.Path)
		http.Error(writer, "no", http.StatusNotFound)
	}))
	defer registry.Close()
	original := npmRegistry
	npmRegistry = registry.URL
	t.Cleanup(func() { npmRegistry = original })
	base := t.TempDir()
	tree, _ := nodeTree(t)
	lockfile, _ := os.ReadFile(filepath.Join(tree, "stage3/api/package-lock.json"))
	installed := filepath.Join(base, keyOf(string(lockfile)), "node_modules")
	os.MkdirAll(filepath.Join(installed, "@types", "node"), 0o755)
	os.WriteFile(filepath.Join(installed, "@types", "node", "package.json"), []byte("{}\n"), 0o644)
	installs, err := InstallNodePackages(tree, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(installs) != 1 || installs[0].NodeModules != installed || installs[0].Lockfile != keyOf(string(lockfile)) {
		t.Fatalf("installs: %+v", installs)
	}
	installs, err = InstallNodePackages(gitTree(t, map[string]string{"go.mod": "module m\n"}), base)
	if err != nil || len(installs) != 0 {
		t.Fatalf("a tree with no lockfile installed %+v, %v", installs, err)
	}
}

// npm comes from the registry held to Loom's pin: any other bytes are refused, named, and nothing is kept of them.
func TestNpmIsHeldToItsPin(t *testing.T) {
	registry := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("not npm"))
	}))
	defer registry.Close()
	original := npmRegistry
	npmRegistry = registry.URL
	t.Cleanup(func() { npmRegistry = original })
	base := t.TempDir()
	if _, err := ensureNpm(base); err == nil || !strings.Contains(err.Error(), "Loom pins "+npmIntegrity) {
		t.Fatalf("npm of other bytes: %v", err)
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Errorf("the base kept %v", entries)
	}
}

// An index's npm projects are checked as it is read: each one Loom installs, named once, with a lockfile's sha256 and a
// chunk of the source inside its node_modules; anything else is the store's poison.
func TestAnIndexsNpmProjectsArePoisonUnlessTheyAreTheSources(t *testing.T) {
	chunk := func(first, last string) SourceChunk {
		return SourceChunk{Blob: keyOf(first + last), First: first, Last: last, Files: 2, Bytes: 1}
	}
	source := []SourceChunk{chunk("a", "stage3/api/.gitignore"), chunk("stage3/api/node_modules/.bin/tsc", "stage3/api/node_modules/z"), chunk("stage3/api/package-lock.json", "z")}
	honest := NodeProject{Directory: "stage3/api", Lockfile: keyOf("lock"), Chunk: source[1].Blob}
	index := TreeIndex{Format: TreeIndexFormat, Tree: "t", Source: source, Node: []NodeProject{honest}, Packages: map[string]TreePackage{}}
	encoded, _ := index.encode()
	if _, err := ParseTree("k", encoded); err != nil {
		t.Fatalf("an honest index: %v", err)
	}
	for name, node := range map[string][]NodeProject{
		"a project Loom doesn't install": {{Directory: "elsewhere", Lockfile: honest.Lockfile, Chunk: honest.Chunk}},
		"one named twice":                {honest, honest},
		"a lockfile not a sha256":        {{Directory: "stage3/api", Lockfile: "lock", Chunk: honest.Chunk}},
		"a chunk not in the source":      {{Directory: "stage3/api", Lockfile: honest.Lockfile, Chunk: keyOf("elsewhere")}},
		"a chunk outside node_modules":   {{Directory: "stage3/api", Lockfile: honest.Lockfile, Chunk: source[2].Blob}},
	} {
		index.Node = node
		encoded, _ := index.encode()
		if _, err := ParseTree("k", encoded); err == nil || !strings.Contains(err.Error(), "poisoned") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// npm ci runs as prepare.sh ran it, from the project's package.json and lockfile alone, with a user and a global
// configuration that are two empty files of its own (npm refuses one file as both, which broke the first install on
// Workshop, Oct 10) and a cache of its own; its node_modules is moved into place whole. A stand-in node plays npm.
func TestNpmCiRunsWithNoConfigurationOfTheMachines(t *testing.T) {
	base := t.TempDir()
	cli := filepath.Join(base, "npm-"+npmRelease, "package", "bin", "npm-cli.js")
	os.MkdirAll(filepath.Dir(cli), 0o755)
	os.WriteFile(cli, []byte("// npm\n"), 0o644)
	bin := t.TempDir()
	seen := filepath.Join(t.TempDir(), "seen")
	os.WriteFile(filepath.Join(bin, "node"), []byte(`#!/bin/bash
for config in "$npm_config_userconfig" "$npm_config_globalconfig"; do
	[ -f "$config" ] && [ ! -s "$config" ] || { echo "config $config isn't an empty file" >&2; exit 1; }
done
[ "$npm_config_userconfig" != "$npm_config_globalconfig" ] || { echo "double-loading config" >&2; exit 1; }
[ "$(dirname "$npm_config_cache")" -ef "$(dirname "$(pwd -P)")" ] || { echo "the cache $npm_config_cache isn't the install's own" >&2; exit 1; }
echo "$* | $(ls -A | tr '\n' ' ')" > `+seen+`
mkdir -p node_modules/@types/node && echo '{"version":"25.3.3"}' > node_modules/@types/node/package.json
`), 0o755)
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	tree, _ := nodeTree(t)
	installs, err := InstallNodePackages(tree, base)
	if err != nil {
		t.Fatal(err)
	}
	said, _ := os.ReadFile(seen)
	want := cli + " ci --ignore-scripts --no-audit --no-fund --install-strategy=hoisted --registry=" + npmRegistry + " | package-lock.json package.json "
	if strings.TrimSpace(string(said)) != strings.TrimSpace(want) {
		t.Errorf("npm ran as %q, want %q", said, want)
	}
	if content, err := os.ReadFile(filepath.Join(installs[0].NodeModules, "@types", "node", "package.json")); err != nil || !strings.Contains(string(content), "25.3.3") {
		t.Errorf("the install isn't in place: %q %v", content, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(installs[0].NodeModules)); len(entries) != 1 {
		t.Errorf("the install holds %v beside node_modules", entries)
	}
}
