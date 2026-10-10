# The Loom updater

Every house machine (Workshop, Cloud, Server, Home and Chonchon on Linux; Sun1 and Sun2 on macOS) runs the Loom binaries Workshop built, and only those. Workshop builds each binary once per Loom commit, stores it by its sha256 and publishes one small manifest. Each machine runs `updater/loom-update.sh` every minute: it reads the manifest, downloads only what changed, checks every byte against its sha256, switches versions with one rename, restarts its own services and reports the version it now runs. No machine clones or builds Loom or holds a GitHub credential, and the updater is the one thing ever installed by hand.

```
  Workshop: publish.sh <commit> ──► out/blobs/<sha256>, out/manifests/<commit>.json, out/current.json
                                          │
                                upload.sh (blobs first, current.json last)
                                          │
                              <base>/blobs/<sha256>, <base>/current.json
                                          │
  each machine, every minute: loom-update.sh ──► ~/.loom/versions/<version>/ ──► ~/.loom/current ──► updated.d/ hooks
```

## The manifest

`<base>/current.json`, written by `publish.sh`:

```json
{
  "version": "039875a194643e432cf548c4145de022333d5a07",
  "files": {
    "loom":        {"linux/amd64": "<sha256>", "darwin/arm64": "<sha256>"},
    "loom-runner": {"linux/amd64": "<sha256>", "darwin/arm64": "<sha256>"},
    "adamic-gate": {"linux/amd64": "<sha256>", "darwin/arm64": "<sha256>"}
  },
  "canary": {
    "hosts": ["Cloud"],
    "version": "9b8e72f0...",
    "files": {"loom": {"linux/amd64": "<sha256>", "darwin/arm64": "<sha256>"}, "...": {}}
  }
}
```

| Field | Meaning |
|---|---|
| `version` | The Loom commit these files were built from, 40 hex. It names the machine's install directory, so it is a plain name: a letter or digit, then up to 127 letters, digits, `.`, `_` and `-`. |
| `files` | Each binary by its installed name, then by Go platform (`<GOOS>/<GOARCH>`), its sha256 as 64 lowercase hex. A machine installs the names built for its own platform and skips the rest; a version with none for it is refused. |
| `canary` | Optional. A second `version` and `files` for the machines named in `hosts` (matched to the updater's host name, ignoring case). Every other machine follows the top level. |

A blob lives at `<base>/blobs/<sha256>` and never changes: its name is its content. Nothing else is read. `publish.sh` also keeps each commit's own manifest, without a canary, at `<base>/manifests/<commit>.json`, which is what a rollback publishes.

The release store is the `loom-artifacts` bucket, under `releases/`: `https://artifacts.loom.system.inc/releases` is the base, so blobs are `releases/blobs/<sha256>` and the manifest `releases/current.json`. That prefix never expires. The bucket's own `blobs/` and `refs/` are test products, expired after 7 days, and never hold a release.

## On a machine

Everything lives under `~/.loom`:

| Path | What it is |
|---|---|
| `loom-update.sh` | The updater, installed by hand. |
| `update.conf` | `key = value` lines, `#` comments: `base` (required), `host` (default `hostname -s`), `report` (optional). `LOOM_UPDATE_BASE`, `LOOM_UPDATE_HOST` and `LOOM_UPDATE_REPORT` override each. |
| `versions/<version>/` | One installed version: its binaries, mode 755, and `SHA256SUMS` (`<sha256> <name>` lines, sorted). A version is never changed once installed. |
| `current`, `previous` | Symlinks to `versions/<version>`, each replaced by a rename so neither is ever missing. Services run `~/.loom/current/<name>`. |
| `updated.d/` | Hooks: every executable in it runs, in name order, after each switch. |
| `update.log` | One line per install, refusal, failed hook or failed report. |
| `reported` | The last version the report URL accepted. |
| `update.lock` | The lock: an `flock` held for the whole run. |

One run, in order:

1. Takes the lock without waiting. If another run holds it, it says so and exits 0. Hooks run without the lock's descriptor, so a service a hook starts never holds it.
2. Fetches `current.json` and picks this machine's entry (canary or top level), then its files for this platform.
3. If `current` is that version with exactly those files, it only retries an unsent report, and exits 0. This is every minute but the ones after a publish.
4. If `versions/<version>` is already on disk (the previous version, after a rollback), it is used as it is; one holding other files than the manifest names is refused, since a version never changes. Otherwise each file is linked from an installed version holding the same sha256 (rehashed first), or downloaded into a temporary file in a staging directory beside the versions, hashed, and refused loudly if its hash isn't its name. Any refusal removes the staging directory file by file and switches nothing: the running version stays. The finished staging directory is renamed to `versions/<version>`.
5. Points `previous` at the version that was running and `current` at the new one.
6. Removes every version but those two, file by file and then `rmdir`, never with a recursive delete. A directory holding anything but files is kept and logged.
7. Runs each hook with `LOOM_UPDATE_VERSION`, `LOOM_UPDATE_PREVIOUS` (empty on a first install) and `LOOM_UPDATE_CURRENT` (`~/.loom/current`), stdin closed, output to `update.log`. A failed hook is logged and makes the run exit 1, after the others run. It is not run again by the next run, which finds the machine current.
8. Appends `installed <version>, previous <version>` to `update.log`, and POSTs `{"host", "version", "previous", "at"}` to the report URL if one is set. A failed report is logged and retried by every later run until one succeeds; it never fails the update.

It runs unchanged on Linux and on macOS's bash 3.2, with curl and python3 (the manifest's JSON, the lock and the symlink rename), and `sha256sum` or else `shasum -a 256`.

A hook is how a machine says which of its services Loom's binaries feed. For example, `updated.d/50-runner` on Linux holds `systemctl --user restart loom-runner`, and on macOS `launchctl kickstart -k gui/$(id -u)/com.loom.runner`. The updater knows nothing of them.

## Publishing

On Workshop, from a Loom clone with `go` on PATH:

```bash
updater/publish.sh <commit> ~/loom-releases                        # every machine
updater/publish.sh --canary Cloud,Sun1 <commit> ~/loom-releases    # those hosts only; the top level stays
updater/upload.sh ~/loom-releases r2:loom-artifacts/releases
```

`publish.sh` checks the commit out in a detached worktree and builds `loom` (`./cmd/loom`), `loom-runner` (`./runner/cmd/loom-runner`) and `adamic-gate` (`./pilots/adamic-gate`) for linux/amd64 and darwin/arm64 (`LOOM_PUBLISH_PLATFORMS` replaces the list), static (`CGO_ENABLED=0`, `-trimpath`), the runner's version stamped `git-<sha12>` as `loom run` stamps the runners it builds. Each binary goes to `blobs/<sha256>`, the commit's manifest to `manifests/<commit>.json`, and `current.json` is that manifest, or with `--canary` the published top level with this commit as its canary. Promoting a canary, like rolling back, is publishing a kept manifest (below).

`upload.sh` sends every blob `current.json` names, then the named versions' manifests, then `current.json` last, so no machine reads a manifest naming a blob that isn't there yet. A destination is a directory a web server serves (as the tests use it) or `r2:<bucket>/<prefix>` through `wrangler r2 object put --remote`, blobs immutable and `current.json` no-cache. The R2 path has not run yet.

Rolling back, or promoting a canary to every machine, is publishing that commit's own manifest: `cp ~/loom-releases/manifests/<commit>.json ~/loom-releases/current.json`, then `upload.sh`. Its blobs are already there, and a machine whose previous version it is switches back without a download.

## Installing

Once per machine, by hand:

```bash
mkdir -p ~/.loom/updated.d
cp updater/loom-update.sh ~/.loom/loom-update.sh
echo "base = https://artifacts.loom.system.inc/releases" > ~/.loom/update.conf
```

Linux, as a user unit (with `loginctl enable-linger` for the user, so it runs logged out):

```bash
cp updater/systemd/loom-update.service updater/systemd/loom-update.timer ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now loom-update.timer
```

macOS, as a LaunchAgent:

```bash
sed "s#HOME_DIRECTORY#${HOME}#g" updater/com.loom.update.plist > ~/Library/LaunchAgents/com.loom.update.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.loom.update.plist
```

## Tests

`updater/update_test.sh` runs the updater against python3's http.server on a temporary directory, fed by the real `publish.sh` (with a stub `go`) and `upload.sh`, each machine a fake home: a first install, a no-op rerun, an update that swaps and runs hooks while downloading only what changed, a corrupted blob refused with the previous version kept, a canary host getting the canary while others don't, a rollback by republishing an old manifest, a failed report that doesn't fail the update and is retried, settings from `update.conf`, and an overlapping run held off by the lock, which a hook's service never keeps. Its header names mutants of the updater that must fail it.
