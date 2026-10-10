# The Loom updater

Every house machine (Workshop, Cloud, Server, Home and Chonchon on Linux; Sun1 and Sun2 on macOS) runs the Loom binaries Workshop built, and only those. Workshop is the only builder: it builds each binary once per Loom commit, stores it by its sha256 and publishes one small manifest. Each machine runs `updater/loom-update.sh` every minute: it reads the manifest, downloads only what changed, checks every byte against its sha256, switches each binary with one rename, restarts its own services and reports the version it now runs. No machine clones or builds Loom or holds a GitHub credential, and the updater is the one thing ever installed by hand. It needs bash (3.2 is enough), curl, awk, and `sha256sum` or else `shasum -a 256`; nothing else.

An updated machine never builds. Its disk holds at most two versions of Loom's binaries, the running one and the one before it, plus a log capped near 1 MB.

```
  Workshop: publish.sh <commit> ──► out/blobs/<sha256>, out/manifests/<commit>.txt, out/current.txt
                                          │
                                upload.sh (blobs first, current.txt last)
                                          │
                              <base>/blobs/<sha256>, <base>/current.txt
                                          │
  each machine, every minute: loom-update.sh ──► ~/.loom/versions/<version>/<name> ◄── ~/.loom/bin/<name> ──► updated.d/ hooks
```

## The manifest

`<base>/current.txt`, written by `publish.sh`, is plain text, one record per line:

```
canary Cloud Sun1
version 039875a194643e432cf548c4145de022333d5a07
loom darwin/arm64 <sha256>
loom linux/amd64 <sha256>
loom-runner darwin/arm64 <sha256>
loom-runner linux/amd64 <sha256>
end 4
version 9b8e72f0...
loom darwin/arm64 <sha256>
...
end 4
```

| Line | Meaning |
|---|---|
| `canary <host>...` | Optional, and only as the first line. It names the hosts that follow the second section (matched to the updater's host name, ignoring case), and promises that a second section exists. Every other machine follows the first section. |
| `version <commit>` | Opens a section: the Loom commit its files were built from, 40 hex. It names the machine's install directory, so it is a plain name: a letter or digit, then up to 127 letters, digits, `.`, `_` and `-`. |
| `<name> <os>/<arch> <sha256>` | One binary of the section, by its installed name and Go platform (`<GOOS>/<GOARCH>`), its sha256 as 64 lowercase hex. A machine installs the names built for its own platform and skips the rest; a section with none for it is refused. |
| `end <count>` | Closes a section, counting its file lines on every platform. |

Every section must close with an end line whose count matches, and a manifest with a canary line must hold both sections. So a manifest cut short at any line, which would otherwise read as a smaller one and drop the binaries it no longer saw, is refused whole. Any other line, a file outside a section, or a name listed twice for one platform, refuses it too. A blob lives at `<base>/blobs/<sha256>` and never changes: its name is its content. Nothing else is read. `publish.sh` also keeps each commit's own manifest, one section with no canary, at `<base>/manifests/<commit>.txt`, which is what a rollback publishes.

The release store is the `loom-artifacts` bucket, under `releases/`: `https://artifacts.loom.system.inc/releases` is the base, so blobs are `releases/blobs/<sha256>` and the manifest `releases/current.txt`. That prefix never expires. The bucket's own `blobs/`, `refs/` and `trees/` are the action store (builder/store.go), each object expired 7 days after its upload, and never hold a release.

## On a machine

Everything lives under `~/.loom`:

| Path | What it is |
|---|---|
| `loom-update.sh` | The updater, installed by hand. |
| `update.conf` | `key = value` lines, `#` comments: `base` (required), `host` (default `hostname -s`), `report` (optional). `LOOM_UPDATE_BASE`, `LOOM_UPDATE_HOST` and `LOOM_UPDATE_REPORT` override each. |
| `versions/<version>/` | One installed version: its binaries, mode 755, and `SHA256SUMS` (`<sha256> <name>` lines, sorted bytewise). A version is never changed once installed. |
| `bin/<name>` | A symlink to `../versions/<version>/<name>`. Services run `~/.loom/bin/<name>`. |
| `version`, `previous` | The version running and the one before it, one line each. `version` is written last, once every `bin/` link points into it. |
| `updated.d/` | Hooks: every executable in it runs, in name order, after each switch, and again on every run until they all pass. |
| `hooked` | The version whose hooks all passed. |
| `update.log` | One line per install, refusal, lock takeover, failed hook or failed report, and the hooks' own output. Once it passes 1 MB it is cut to its last 2000 lines. |
| `reported` | The last version the report URL accepted. |
| `update.lock/` | The lock: a directory holding the running updater's `pid`. |

Each `bin/` link, and `version`, `previous` and `hooked`, is replaced by writing a new one beside it and `mv -f` over it. A rename over a file or a symlink to a file is atomic on Linux and macOS alike, so a service never finds its binary missing. (A symlink to a directory would not do: there `mv` moves the new one inside it, which is why there is no `current` directory link.) Something already at `bin/<name>`, like a binary installed by hand, is replaced the same way; a directory there is refused.

One run, in order:

1. Takes the lock without waiting: `mkdir update.lock`, atomic on every system, then its pid inside. If another run holds it, it says so and exits 0. A lock whose pid is no longer a running `loom-update.sh` (dead, or the number reused by another program, as `ps` shows) is stale, and the first run to make `update.lock/takeover-<pid>` takes it over, logging it; that directory stays until the lock is released, so another run that judged the same pid stale can't take it twice. A lock with no pid yet belongs to a run between its `mkdir` and its pid, unless it is over a minute old. The lock is released when the run exits, so nothing a hook starts keeps it.
2. Cuts `update.log` to its last 2000 lines if it has passed 1 MB.
3. Fetches `current.txt` and picks this machine's section (canary or first), then its files for this platform.
4. If `version` names that section with exactly those files and every `bin/` link points into it, it runs the hooks again if `hooked` doesn't name that version, retries an unsent report, and exits. This is every minute but the ones after a publish.
5. If `versions/<version>` is already on disk (the previous version, after a rollback), it is used as it is; one holding other files than the manifest names is refused, since a version never changes. Otherwise each file is hard-linked from an installed version holding the same sha256 (rehashed first), or downloaded into a temporary file in a staging directory beside the versions, hashed, and refused loudly if its hash isn't its name. Any refusal removes the staging directory file by file and switches nothing: the running version stays. The finished staging directory is renamed to `versions/<version>`.
6. Points each `bin/<name>` at the new version's file, then writes `previous` (the version that was running) and `version`, last.
7. Removes a `bin/` link into `versions/` whose name the new version no longer ships, then every version but the new one and the previous, file by file and then `rmdir`, never with a recursive delete. A directory holding anything but files is kept and logged.
8. Runs each hook with `LOOM_UPDATE_VERSION`, `LOOM_UPDATE_PREVIOUS` (empty on a first install) and `LOOM_UPDATE_BIN` (`~/.loom/bin`), stdin closed, output to `update.log`. When every hook exits 0 it writes `hooked`. A failed hook is logged and makes the run exit 1, after the others run, and since `hooked` isn't written, the next run runs every hook again, and so on until they pass. A service is never left on old code without the log saying so every minute.
9. Appends `installed <version>, previous <version>` to `update.log`, and POSTs `{"host", "version", "previous", "at"}` to the report URL if one is set. A failed report is logged and retried by every later run until one succeeds; it never fails the update.

A run killed between two `bin/` links leaves `version` naming the old version, so the next run switches every link again. A hook is how a machine says which of its services Loom's binaries feed. For example, `updated.d/50-serve` on a Linux box, which `loom-runner install-serve` writes, runs `"$LOOM_UPDATE_BIN/loom-runner" install-serve`, which installs the release's `loom-serve.service` and reloads serve when it runs an older release, so serve drains the unit in hand and starts again on the new runner (docs/serving.md); on macOS a hook would hold `launchctl kickstart -k gui/$(id -u)/<label>` for a service running `~/.loom/bin/loom-runner`. Since a hook can run more than once for one version, it should be safe to repeat, as a restart is. The updater knows nothing of the services.

## Publishing

On Workshop, from a Loom clone:

```bash
export LOOM_PUBLISH_GO=/home/ahra/adamic-tools/go/bin/go               # Workshop's Go isn't on a service's PATH
updater/publish.sh <commit> ~/loom-releases                        # every machine
updater/publish.sh --canary Cloud,Sun1 <commit> ~/loom-releases    # those hosts only; the first section stays
updater/upload.sh ~/loom-releases r2:loom-artifacts/releases
```

`publish.sh` builds exactly the Go mains the repository has, `loom` (`./cmd/loom`) and `loom-runner` (`./runner/cmd/loom-runner`), and refuses a commit holding any other `package main` directory, so a new command is never left unshipped. It checks the commit out in a detached worktree and builds for linux/amd64 and darwin/arm64 (`LOOM_PUBLISH_PLATFORMS` replaces the list), static (`CGO_ENABLED=0`, `-trimpath`), the runner's version stamped `git-<sha12>` as `loom run` stamps the runners it builds. Each binary goes to `blobs/<sha256>`, the commit's manifest to `manifests/<commit>.txt`, and `current.txt` is that manifest, or with `--canary` a canary line, the published first section and this commit's manifest.

The compiler is pinned. A different Go can make a different binary from the same commit, so `publish.sh` takes the Go named by `LOOM_PUBLISH_GO` (else `go` on PATH), runs it with `GOTOOLCHAIN=local`, and refuses unless its `go version` is exactly the `toolchain` line of the commit's `go.mod` (today `go1.27.1`, Workshop's). It prints the version it built with. A commit whose `go.mod` names no toolchain is refused.

Workshop's disk is guarded both ways, since Workshop is the only builder. Before building, `publish.sh` refuses when the temporary directory, the out directory, Go's build cache or Go's module cache has under 10 GB free (`LOOM_PUBLISH_FLOOR_GB`), naming which one and how much it has. Workshop runs it with `LOOM_PUBLISH_FLOOR_GB=100`, the same floor `loom build-tree` keeps there (`--floor-gb`, #ckv0pmg); the default stays 10 for a machine that only publishes. After publishing, the out directory keeps the manifests `current.txt` names and the newest 10 others (`LOOM_PUBLISH_KEEP`), and only the blobs those name; every other manifest and blob is deleted, file by file. A rollback reaches as far back as the kept manifests.

`upload.sh` sends every blob `current.txt` names, then the named versions' manifests, then `current.txt` last, so no machine reads a manifest naming a blob that isn't there yet. A destination is a directory a web server serves (as the tests use it) or `r2:<bucket>/<prefix>`, which PUTs each object to R2's S3 endpoint, `https://<account id>.r2.cloudflarestorage.com/<bucket>/<prefix>/<key>`, signed by curl itself (`--aws-sigv4 aws:amz:auto:s3`), blobs `Cache-Control: public, max-age=31536000, immutable` and manifests no-cache. No wrangler or node is needed. Its keys come from `LOOM_UPLOAD_CREDENTIALS` (default `~/.loom/r2-releases.conf`), `key = value` lines for `account_id`, `access_key_id` and `secret_access_key`; the key pair reaches curl on its standard input as a config file, never on its command line, so `ps` never shows it. A destination with no prefix is refused. The R2 path has run only against a stub curl so far.

`upload.sh` deletes nothing remote: the release prefix keeps every blob ever uploaded. Pruning the bucket the same way `publish.sh` prunes its out directory is a follow-up.

Rolling back, or promoting a canary to every machine, is publishing that commit's own manifest: `cp ~/loom-releases/manifests/<commit>.txt ~/loom-releases/current.txt`, then `upload.sh`. Its blobs are already there, and a machine whose previous version it is switches back without a download.

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

Then point each Loom service at `~/.loom/bin/<name>` and give it a hook in `~/.loom/updated.d/`. A Linux box that serves a pool does both with `loom-serve` (docs/serving.md). The lander (Workshop) does both with `loom push install` once `~/.loom/push.conf` makes it the lander: it writes `loom-pusher.service`, `loom-pusher.timer` and the hook `60-push`, which installs each release's units again and never enables or starts the timer; that stays Kirk's call, `systemctl --user enable --now loom-pusher.timer` (package lander).

## Tests

`updater/update_test.sh` runs the updater against python3's http.server on a temporary directory, fed by the real `publish.sh` (with a stub `go`) and `upload.sh`, each machine a fake home: a first install, a no-op rerun, an update that swaps and runs hooks while downloading only what changed, a manifest cut at a line boundary (plain, and a canary manifest cut after its first section) refused with nothing changed, a corrupted blob refused with the previous version kept, a canary host getting the canary while others don't, a rollback by republishing an old manifest, a failed report that doesn't fail the update and is retried, a hook that fails once running again on the next run and then stopping, a log over 1 MB cut to its last 2000 lines, settings from `update.conf`, an overlapping run held off by the lock, which a hook's service never keeps, and stale locks taken over (a dead pid, a reused pid, a pidless lock over a minute old) while a fresh pidless one holds. Then `publish.sh` refusing the wrong compiler, a disk under its floor and an unshipped Go main, and pruning its out directory; and `upload.sh`'s R2 path against a stub curl, checking the order, the URLs, the cache headers, and that the secret never reaches curl's arguments. Only the test's server and report checks use python3. Its header names mutants of the updater that must fail it.
