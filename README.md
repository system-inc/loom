# Loom

The kingdom's remote compute fabric: any job, on any machine we own or rent, streamed live, decided by our coordinator.
Commands in, artifacts out. A runner holds no credential but its tokens (below); a missing unit is void, never green.
`docs/protocol.md` is the contract: jobs, units, events, verdicts, tokens, the store and the wire's endpoints.
`protocol/` holds its Go types; `runner/` and `coordinator/` (with `cmd/loom`) are the Go module's parts; `wire/` is the Cloudflare Worker, live at https://runs.loom.system.inc.
`updater/` is how the house machines get Loom's binaries: Workshop builds and publishes them once per commit, and each machine's updater installs them (`docs/updater.md`).
`serving/` keeps each Linux house machine serving its pool on the release's runner, a systemd user unit the updater's hook installs and reloads (`docs/serving.md`).
The work and its laws live in the task tree at #system_adamic_developer_tools_loom.

Tests, from a clean clone: `go test ./...`, then in `wire/` `pnpm install --frozen-lockfile && pnpm test && pnpm check`, and `updater/update_test.sh`. There is no CI service: Loom gates itself once the coordinator exists.

## What a strict worker does

A Codex instance serves its pool with `loom-runner serve --strict --exclusive`. This is everything it does, in order, as
`runner/strict.go` and `runner/prepare.sh` do it. A house box serves `--strict` without `--exclusive`
(docs/serving.md): its machine is shared with other work, so it clears only its own root and runs no setup in HOME. A
runner without `--strict` runs a unit's argv as `docs/protocol.md` says, and a test job the same way as below except the
disk trims.

**What it holds.** The pool token, which asks its pool for the next unit and reaches nothing else, and for each unit
that unit's run token, which posts that unit's events and log and reads and writes blobs through that run's own
endpoint, and expires with the run. Nothing else. Both stay in the runner's memory: serve reads the pool token from
`--token-file` and removes the file at once, so it is on no command line and no disk while units run.

**What it accepts.** Only a unit carrying a structured test job (`protocol.TestJob`). It refuses, before anything runs,
with an error event naming why, and finishes the unit broken:
- any unit with `argv`, a command or a shell string alike;
- a test job carrying an environment, inputs, a directory, or outputs other than `loom-out/test.jsonl.gz` and
  `loom-out/cpu.tsv`;
- a test job whose repository isn't exactly `https://github.com/system-inc/adamic`; whose sha, base or sample isn't 40
  lowercase hex; whose gate inputs hash isn't 64 lowercase hex; that names no package, or a package that isn't a plain
  import path under `github.com/system-inc/adamic` (no `...`, no leading dash, no space or shell text); whose `-run` or
  `-skip` pattern holds a NUL or a line break, doesn't compile as a regular expression or is over 64 KiB; or whose
  changed paths aren't paths in the repository.

**How it readies the checkout.** `prepare.sh` is compiled into the runner and written beside the unit's workspace; the
job's values reach it only as positional arguments, quoted at every use, and nothing of the job is ever part of its
text. It:
1. runs git with no system or global configuration, no prompt, no password helper, no credential helper and no hooks,
   and fetches submodules recorded over ssh from `https://github.com/` instead;
2. keeps what outlives a unit under its root, `/tmp` for an exclusive runner (`--root`), and touches nothing outside
   that root, the checkout and HOME; it first removes what earlier units left there (go's and the tests' temporary
   directories, stage 3 lane trees, half-made npm trees), since it runs one unit at a time, and, only with
   `--exclusive`, HOME's caches (the runtime's build directories, and go's build cache when under 3 GB is free); under
   1.5 GB free it stops, the instance's fault;
3. keeps the checkout at `<root>/adamic` (`--tree`) across units, but makes it again from the public repository when its
   own configuration names a URL rewrite, a credential or HTTP setting, an askpass, an ssh command, a hook path or an
   include;
4. fetches the sha, and the base when there is one, from `https://github.com/system-inc/adamic` into an empty object
   store first (the commit object alone), so only that public repository can supply it, then into the checkout;
   anything it can't fetch is refused, and nothing further runs;
5. checks out the sha, and refuses a sha that doesn't descend from its base;
6. refuses a submodule whose URL isn't on GitHub, then updates the submodules over HTTPS with no credentials;
7. with `--exclusive`, runs adamic's own `cloud/setup.sh --wasi-sdk` at that commit, once per instance, which installs
   adamic's toolchain into HOME from the sources that script names; without it, uses the machine's own toolchain, and
   a machine with none is unfit for the unit; then loads its `env.sh`;
8. runs `npm ci --ignore-scripts` for `stage3/api` from https://registry.npmjs.org, once per lockfile;
9. fetches the gate inputs named by the job's hash from Loom's public store
   (https://artifacts.loom.system.inc/gate-inputs/, where `loom gate-inputs publish` writes them), checking every chunk
   and the total by sha256;
10. runs `go mod download` for every Go module in the checkout, from Go's module proxy;
11. writes the resulting environment for the tests.

**How it runs the tests.** It execs, itself, for each package: `go test -count=1 -json -timeout 3h -run=<pattern>
[-skip=<pattern>] <package>` in the checkout, each pattern one argument, never read by a shell; first a compile-only
pass (`-exec /bin/true`) to time the build. Half the CPUs' packages run at once. Their environment is the runner's base
(`PATH`, `HOME`, `TMPDIR`, `LANG`, `LOOM_SLOT`, `LOOM_SLOT_CPUS`, the proxy and CA variables), what prepare.sh added,
`ADAMIC_GATE_UNCACHED`, `ADAMIC_TEST_WASI`, `ADAMIC_ORACLE_WASI` and `ADAMIC_GATE_COHERE` set to 1, and the job's sample
and changed paths as `ADAMIC_GATE_SAMPLE` and `ADAMIC_GATE_CHANGED`. A `TestWASI` pattern runs with the WASI SDK's
clang first on `PATH`, and is refused as broken where the SDK's builtins are missing. These tests are adamic's own code
at the verified commit, and they run with the user's permissions.

**A job naming its tree's build.** A test job with `tree` set (the tree key `loom build-tree` prints) runs what
Workshop built and builds nothing (`runner/prebuilt.go`, #pcn6prz). In place of the checkout and `go test` above, it:
1. keeps every blob it reads from Loom's public store in one cache, `<root>/loom-blobs/<sha256>`, read-only, bounded
   (4 GiB by default) with the least recently used going first; before every unit, while the cache's own disk has
   under its floor free (3 GiB by default), it removes more, then the unpacked sources no unit holds; a disk still short,
   or a short disk the caches aren't on, refuses the unit as unfit, broken, never failed (serve itself keeps today's
   1500 MB floor);
2. reads `trees/<tree>.json` from the store (the runner's own setting, never the unit's), holds it to its key and its
   format (`builder.TreeIndexFormat`: an index in another format is unfit, as good as none, and Workshop builds the tree
   again), refuses a tree built for another platform (the key and index name `GOOS`/`GOARCH`) before fetching anything,
   and fetches each package's test binary, the products its tests read, and the chunks of the tree's source it doesn't
   already hold (step 3), every one checked against its sha256 as it arrives and again when read from the cache, each
   fetch's bytes and seconds on the unit's record (the chunks' in a sum); a fetch is
   written to a locked `.partial-` and renamed only when whole, so a kill or a full disk never leaves a blob's name on
   anything else; the index, every fetch and every wait on another unit's fetch end at the unit's deadline; whatever
   the store lacks or can't give whole is named, and the unit is broken, never red;
3. keeps each tree's source at `<root>/loom-sources/<sum of its chunk list>` for every unit of the tree, assembled from
   its chunks (each a gzipped tar of a run of its paths, named by its sha256, its range in the index; 836 for adamic's
   98,459 files) beside it under a lock, every entry in its chunk's range, marked complete, synced once and only then
   renamed, never trusted without its marker, held by a lock while a unit runs in it. A tree sharing at least half its
   bytes with a kept tree no unit holds, unchanged since it was assembled (every entry's change time, which no test can
   set back), is made from that tree: renamed in, the entries of its chunks the new tree lacks removed, the rest of the
   chunks unpacked in, so a one-file change costs about one chunk (`runner/assemble.go`);
4. readies the environment with `prepare.sh environment` (the instance's adamic toolchain, which must already be set
   up, stage3/api's npm packages and the gate inputs; no git, no setup, no Go), with the products in the unit's own
   `ADAMIC_BUILD_CACHE_DIR`;
5. runs each binary as `go test -json` would, in the package's directory of the source:
   `<binary> -test.paniconexit0 -test.timeout=3h0m0s -test.count=1 -test.v=test2json -test.run=<pattern>
   [-test.skip=<pattern>]`, its output through Go's own test2json conversion (`runner/test2json`, vendored) into the
   same go test lines. A package whose test binary didn't compile or vet on Workshop is its red, said as go test says
   it, with Workshop's diagnostics; one that failed for Workshop's reasons is broken. A stand-in `go` comes first on
   the tests' `PATH`: read-only queries (`version`, `env`, `list` with allow-listed flags, none that builds or asks a
   proxy) go to the runner's own go, which must report exactly the tree's release under `GOTOOLCHAIN=local` or the
   unit is unfit. The test's own `GOFLAGS` and `GOTOOLCHAIN` pass through as it set them, so `go env` answers as it
   did on Workshop (adamic keys products on them), when every flag is on an allow list (`-buildvcs`, `-trimpath`,
   `-p`, `-mod=readonly`, `-mod=mod`, `-tags`, `-ldflags`, `-gcflags`, `-asmflags`, `-race`, `-cover...`,
   `-pgo=off|auto`) and the toolchain is unset, `auto`, `local` or the tree's release. Above them the runner sets
   `GOENV=off`, `GOSUMDB=off` and `GOPROXY=file://` the tree's module cache, which Workshop publishes with the tree
   (`go mod download all`, 63 MB for adamic) and the runner fetches and unpacks once like a source, into a
   `GOMODCACHE` per tree that the blob cache's bound counts, filled before the tests; a module it lacks breaks the
   unit, named, and nothing reaches the network. A query in the tree's source, when the test set no `GOWORK`, uses a
   copy of the tree's `go.work` outside the source (its paths made absolute), so go writes no `go.work.sum` into the
   tree. Everything else is refused, and a
   red whose test was refused is broken, naming the command. With no go, a unit whose tests asked one is unfit.

These binaries are Workshop's builds from Loom's store, not compiled on the instance from the public repository at the
job's commit.

**What it writes.** The go test lines as `loom-out/test.jsonl.gz` and each package's CPU seconds as `loom-out/cpu.tsv`,
uploaded through the run token, and the unit's events through the run token. On the instance: the checkout, its caches
(`<root>/adamic-npm`, `<root>/adamic-tools`, `<root>/adamic-setup-done`, Go's caches in HOME) and the unit's workspace, removed when
the unit ends.

**What it reads.** The public adamic repository and its public submodules on GitHub, https://registry.npmjs.org, Go's
module proxy (https://proxy.golang.org) and checksum database (https://sum.golang.org), Loom's public store, and what
adamic's own `cloud/setup.sh` downloads, which as of adamic 3a391aff13 is: https://go.dev and https://dl.google.com
(the Go toolchain), GitHub releases (LLVM, the WASI SDK) and https://nodejs.org (Node.js). setup.sh sets
`GOPROXY=https://proxy.golang.org|direct`, so a module the proxy doesn't hold is fetched from its own host. These are
as of that commit: setup.sh runs at the job's commit and may change them.

**What "verifiable" means.** A commit is verifiable when it can be fetched from the public repository's URL with no
credentials. That includes commits in pull requests from forks, which GitHub serves through the same URL. The runner
proves only that GitHub holds the commit; the coordinator, not the runner, chooses which commit a job names.
