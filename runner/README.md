# loom-runner

Runs one Loom unit on this machine and streams its events to stdout as JSON lines (see `docs/protocol.md`).

```
loom-runner run [--workspace <directory>] [--keep] [--strict] [--phase-jobs] [--exclusive] [--root <directory>] [--tree <directory>] <unit.json | https URL | ->
loom-runner serve --pool <wire>/pools/<pool> --token-file <file> --worker <name> --until <duration> [--workspace <directory>] [--log <file>]
loom-runner install-serve
loom-runner version
```

`serve` is for machines Loom can't ssh into (docs/protocol.md, "The pool"): it asks the pool for a unit (`POST <pool>/next`), runs it as `run` would, each unit posting its own events to the wire it names, and asks again, until less than a minute remains before `--until`. Events go to `--log` or nowhere; stdout gets one summary line at the end (units, passed, failed, broken, seconds). It exits 0 at the deadline, on `SIGHUP` once the unit in hand finishes (a drain), or on `SIGTERM` (which breaks the unit in hand), and 2 when the pool refuses its token. A unit whose test job names another runner runs on that runner, fetched by its sha256 from the release store into `<root>/loom-runners` (`--releases`), checked, and run as `run` with serve's own settings and the unit on its stdin (docs/serving.md).

`install-serve` readies a Linux box's `loom-serve.service` from `~/.loom/serve.conf` and `~/.loom/serve-token`, writes the updater's hook, enables serve, and starts or reloads it only when it isn't running this unit and this release; the hook runs it after every release (docs/serving.md).

Flags go before the unit. Exit codes: `0` passed, `1` failed, `2` broken (or the unit couldn't be read).

What a run does, in order:

1. Decodes the unit strictly and checks it (local paths, 64-hex hashes, a positive timeout, a store when there are inputs or outputs). A bad unit is `broken` before anything runs.
2. Makes a fresh workspace under `$TMPDIR` (or `--workspace`), fetches each input from `<store>/<sha256>` with the unit's token, and verifies the hash before placing it. A mismatch is refused. `archive: "tar"` (gzipped or not) unpacks at `path`, refusing any entry that would land outside it.
3. Runs argv in `workspace/<directory>` with only `PATH`, `HOME`, `TMPDIR` and `LANG` from the runner's environment, plus the unit's `environment`, in its own process group. At the timeout the group gets SIGTERM, then SIGKILL 5 s later. Whatever the leader leaves in the group dies when it exits.
4. Emits `started`, one `output` per line of each stream, `exit`, `uploaded` per output file, `error`s, and `finished` last. Sequence numbers are gapless and match the line order.
5. Hashes and PUTs each output glob's files to `<store>/<sha256>`. A glob that matches nothing fails the unit; a store that refuses a file breaks it.
6. When the unit names a wire, also POSTs the same lines there in batches (at least every 250 ms). A failing wire shows up as an `error` event with phase `wire` and never changes the status.
7. Deletes the workspace unless `--keep` is given.

A release build sets the version with `-ldflags "-X github.com/system-inc/loom/runner.Version=<version>"`. Static builds: `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./runner/cmd/loom-runner` (and `arm64`, and `GOOS=darwin GOARCH=arm64`).

Overhead per unit, measured with `go test -bench True ./runner/cmd/loom-runner`, is the runner's own process start plus a few milliseconds.
