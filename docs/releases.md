# Releases

Every commit on loom main releases itself. On Workshop, `loom release watch` (package `release`, unit `release/systemd/loom-release.service`) fetches main every 30 s; a new head is published as a canary for Cloud, held to Cloud's own reports through a soak, and promoted to every box only if Cloud stays healthy. Any failure stops it, loudly, and nothing more is released until a person says so. Never two releases at once. The watcher only drives what a person did by hand before (docs/updater.md): `publish.sh --canary Cloud`, `upload.sh`, then the commit's own manifest as `current.txt`, and `upload.sh` again.

```
  main moves ──► publish.sh --canary Cloud, upload.sh ──► Cloud installs, reports ──► soak 10m ──► manifests/<commit>.txt as current.txt, upload.sh
                                                              │ (15m to report)          │ healthy?               │
                                                              └──── no: STOPPED ◄────────┘                        every box installs, reports (15m)
```

## What the watcher reads

The boxes' own reports. Each box's updater posts its state to the watcher (`report = http://<Workshop>:7381/report` in its `update.conf`; docs/updater.md, "The report") whenever it changes and every 5 minutes besides: its version, the one before, when it last switched, the version whose hooks all passed, its hold, its last refusal, and a line per service its health probes watch, like `loom-serve.service active running restarts=0`. The watcher keeps each box's latest at `~/.loom/releases/reports/<host>.json`, stamped with when it arrived; a report naming a host that isn't one of `boxes` is refused, so a mistyped host shows at once rather than as a box that never reports. It also reads, with Workshop's token secret, when each box's serve last asked its pool (`GET /pools/<pool>` on the wire, as `loom pool status` does; a worker is `<host>-<machine id>`).

## One release

1. **Begin.** The head of `origin/main` in the clone isn't what `out/current.txt` gives every box. The watcher refuses (stops) a head that doesn't descend from that release, and a published canary it didn't begin (a person's, or one left by a stopped release since resumed): two releases would be in flight. It reads the commit's order (below).
2. **Canary.** `<clone>/updater/publish.sh --canary Cloud <commit> <out>`, then `upload.sh <out> r2:loom-artifacts/releases`, run from the clone's checkout, so a commit can't change how it is itself published. Cloud has 15 minutes (`canary-within`) to report the commit installed, its hooks passed for it, and `loom-serve.service` active. Only a report that arrived after the canary was published counts.
3. **Soak.** 10 minutes (`soak`, at least 6, since a quiet box reports every 5). Cloud must stay on the commit; its serve may restart at most twice (`restarts`: the release's drain, and the hourly `--until` turnover; a crash loop restarts every 30 s). At the end, Cloud must have reported during the soak, every service it reports must be active, and its serve must have asked its pool in the last 3 minutes (with 5 more minutes' grace for the wire).
4. **Promote.** `manifests/<commit>.txt` becomes `current.txt`, and `upload.sh`. Every other box has 15 minutes (`fleet-within`) to report the commit with its hooks passed. A held box is skipped and named. A box that doesn't report in time is named LAGGING, loudly, but doesn't stop the release: an offline box catches up when it returns, and `loom release status` says it lags until then.
5. **After**, then done.

A pass is under `<out>/.release.lock`, taken without waiting: two watchers, or a watcher and a person's `loom release rollback`, never publish at once, and a pass that finds it held changes nothing. The release in hand is `~/.loom/releases/release.json` (commit, phase, since), written after each step, so a watcher restarted mid-release carries on: every release's hook restarts it on Workshop.

## When it fails

Any failure in the canary or the soak (no report in time, hooks failing, a service down, restarts past the allowance, leaving the commit, a serve not asking), a failed publish, upload or promotion, a head that doesn't descend, an order it can't honor, a canary it didn't begin: the watcher logs `RELEASE STOPPED: <commit>: <why>` to its journal, keeps it in `release.json`, and releases nothing more. **Cloud is left on the canary, for inspection**; every other box stays on the release before. It never rolls back by itself.

- `loom release rollback` ends the canary: the fleet's release becomes `current.txt` alone again, uploaded, and Cloud switches back (its previous version is on disk). It refuses while a release is moving or when no canary is published.
- `loom release resume` lets the watcher release again, once no canary is published. The commit that stopped it isn't released again until main moves past it; `--retry` releases it again.

Rolling the whole fleet back stays by hand: an older `manifests/<commit>.txt` as `current.txt`, then `upload.sh` (docs/updater.md).

## A release's order

A commit may declare what must happen around the fleet taking it, in `updater/release-order`: `#` comments, and lines `<step> after fleet` (once every box runs it, say `workers after fleet` for a Worker deploy that needs the Go judge on the boxes first) or `fleet after <step>` (before the canary is published). The watcher does none of the steps: it honors them and says what it waits on. A step before the fleet holds publishing until `loom release mark <commit> <step>`; if main moves on meanwhile, nothing was published, and the new head is released instead. A step after the fleet holds the release open, and the next release behind it, until marked. Whoever does the step marks it (the Worker deploy, #5tvjjx3, or a person). Any other line stops the release unpublished: a rule the watcher can't honor is never skipped.

## Holding a box

`hold = current` or `hold = <version>` in a box's `update.conf` keeps it there (docs/updater.md, "A hold"). Its reports carry the hold; the watcher skips it and names it HELD; status shows it held, not lagging.

## `loom release status`

```
release  8d1c9a9e0b1c for every box; canary 0f3a5c7e9b1d for Cloud
watcher  soak of 0f3a5c7e9b1d since 2026-10-10T16:05:00Z (4m10s)

box       version       updated               held  heard     asked    services                         state
Workshop  8d1c9a9e0b1c  2026-10-10T09:12:00Z  -     40s ago   -        loom-plan active r0, ...         ok
Cloud     0f3a5c7e9b1d  2026-10-10T16:05:40Z  -     1m2s ago  12s ago  loom-serve active r1             ok
Home      82bb68a1c3e5  2026-10-10T09:19:00Z  -     2m ago    30s ago  loom-serve active r0             LAGS: runs 82bb68a1c3e5, the release for it is 8d1c9a9e0b1c
```

It reads `<base>/current.txt` (what every box reads), the watcher's state, the reports, and the pools. Each box: the version it runs, when it last updated, its hold, when it was last heard from and when its serve last asked, its services, and plainly what's wrong: LAGS (not on the release the manifest gives it, unheld), HELD, HOOKS FAILING, REFUSED, SILENT (unheard for 12 minutes), UNHEALTHY (a service not active), NOT ASKING (its serve hasn't asked in 3 minutes), NEVER REPORTED. It exits 1 when any box falls short or the watcher is stopped.

## Installing

Once, on Workshop, after a release with `loom release` is installed:

```bash
printf '# Workshop releases\n' > ~/.loom/release.conf   # its presence is the switch; every key has a default
~/.loom/bin/loom release install                          # writes the hook, the probe and the unit, and starts the watcher
```

`install` writes `~/.loom/updated.d/60-release` (after every release it runs `loom release install` again: the unit rewritten if changed, the watcher restarted on the new binary), `~/.loom/health.d/60-release` (Workshop's daemons, release.conf's `units`, in its reports), and `~/.config/systemd/user/loom-release.service`. Where there is no `release.conf` it does nothing, so no other box ever releases. Then point every box's `update.conf` at the receiver (`report = ...`, docs/updater.md); the receiver's port must be reachable from the boxes.

`release.conf`, `key = value`, every key optional: `repository` (`~/Projects/system/loom`, a clone whose remote fetches without a prompt), `remote` (`origin`), `branch` (`main`), `out` (`~/loom-releases/out`), `destination` (`r2:loom-artifacts/releases`), `base` (`https://artifacts.loom.system.inc/releases`), `state` (`~/.loom/releases`), `listen` (`:7381`), `canary` (`Cloud`), `boxes` (`Workshop Cloud Server Home Chonchon`), `canary-services` (`loom-serve.service`), `pools` (`box-strict box-phase`), `units` (Workshop's daemons), `canary-within` (`15m`), `soak` (`10m`), `fleet-within` (`15m`), `restarts` (`2`), `interval` (`30s`). Any other key, or a value that doesn't read, is refused. The unit runs publish.sh with `LOOM_PUBLISH_GO=~/adamic-tools/go/bin/go` and `LOOM_PUBLISH_FLOOR_GB=100`; upload.sh signs with `~/.loom/r2-releases.conf`.

## Tests

`go test ./release/ ./updater/ ./serving/`: a canary that never reports, with a report from before it naming the same commit, is stopped at its time and never promoted, Cloud left on it, nothing more published; a failing canary stops (restarts past the allowance, hooks failing, serve down at the soak's end, leaving the commit, serve not asking, the wire unable to say); a healthy canary is promoted after the soak and the fleet follows, a held box named and not waited on; a box that doesn't follow is named lagging; two watchers on one out directory never publish at once, and the loser stops at the canary it didn't begin; an order's steps after and before the fleet are waited on, and a rule it can't honor stops the release; a head that doesn't descend stops; rollback and resume, and a resumed watcher not releasing the commit that stopped it; status flags a lagging box, and names a held, silent, unhealthy, hook-failing, never-reporting or non-asking one; the receiver, the manifest, release.conf and the order file; the real git steps against a local remote; install doing nothing unconfigured, and its hook passing on a release from before it; the probe's lines from a stub systemctl. `updater/update_test.sh` covers the report and the hold on the updater's side. Mutants that each fail them: the canary never timing out, a report from before the canary counting, restarts unbounded, a soak ending unhealthy promoted, the pools unread, no lock, an order's steps ignored, a held box waited on, a canary it didn't begin ignored, a stopped commit released again, status calling no box lagging or a held one lagging or none silent, the receiver taking any host. Nothing here has run under systemd or against R2: the first real release is Workshop's.
