# Serving the pools from the house

Every Linux house machine (Workshop, Cloud, Server, Home and Chonchon) serves one pool on the wire: a systemd user unit, `loom-serve`, keeps `loom-runner serve` asking its pool for units and running them, one at a time, on the runner the updater installed (docs/updater.md). A release restarts it without breaking the unit in hand. The pool and its protocol are docs/protocol.md's ("The pool").

## Where a pool's runner comes from

A pool's runner is a release's `loom-runner linux/amd64`, and nothing else. Workshop builds it once per Loom commit with the pinned Go (`publish.sh`), every box's updater installs it at `~/.loom/bin/loom-runner`, and a Codex instance fetches the same blob by its sha256 from `https://artifacts.loom.system.inc/releases/blobs/<sha256>` (`loom pool prompt --runner <sha256>`). There is no other way to publish a runner: `loom pool publish-runner`, which built one with whatever `go` was on PATH and uploaded it through the wire's `/public/blobs`, is gone. (`/public/blobs` itself stays: adamic's build cache writes through it.)

A pool's **pin** is that runner's sha256: Workshop's `~/.loom/runner-pin`, which the planner puts in every test key, and the `runner` of each pool in `~/.loom/pools.json`, which the placer and the judge match keys to. The first section of the release manifest names it:

```bash
curl -fsS https://artifacts.loom.system.inc/releases/current.txt |
  awk '$1 == "version" { sections++ } sections == 1 && $1 == "loom-runner" && $2 == "linux/amd64" { print $3; exit }'
```

On a box, `sha256sum ~/.loom/bin/loom-runner` says the same for the release it runs.

Every job names the runner its key names (`protocol.TestJob.Runner`, set by the planner from the key's `tools.runner`; a product names none). A runner that isn't that one refuses the job before anything runs, as unfit: broken, Loom's, never the change's, and the judge voids it and places it again, as it voids anything another runner computed. Serve then asks for nothing for ten minutes, since every unit its pool holds names the same runner. So while a release and the pin disagree (a box switched before the pin moved, a box still on the release before, a canary), the box stands down instead of spending units, and it serves again once they agree: a release drains it at once, a moved pin within ten minutes. Moving the pin moves every test key with it, as any runner switch does.

## The unit

`serving/systemd/loom-serve.service` is the unit. Nobody copies it: `loom-runner install-serve` renders it from the box's settings and installs it, and the updater's hook runs that after every release, so the unit always comes with the runner it runs.

| Path | What it is |
|---|---|
| `~/.loom/serve.conf` | `key = value` lines, `#` comments, as `update.conf`: `pool` (required, the pool's name on the wire) and `phase-jobs` (`yes` for a pool that takes phase units, like `box-phase`; default `no`). Anything else is refused. |
| `~/.loom/serve-token` | The pool token for that pool (`loom pool token <pool>`), mode 600. `install-serve` refuses one anyone but its owner can read. |
| `~/.loom/updated.d/50-serve` | The hook: `exec "$LOOM_UPDATE_BIN/loom-runner" install-serve`. |
| `~/.config/systemd/user/loom-serve.service` | The rendered unit, written only when its text changed. |
| `~/loom-serve/root` | The runner's root: its blob cache and unpacked sources, under the runner's own bounds (4 GiB of blobs, at most two sources, 3 GiB free before a prebuilt unit starts). |
| `~/loom-serve/units` | Each unit's workspace while it runs. |

The unit runs `~/.loom/bin/loom-runner serve --strict [--phase-jobs] --pool https://runs.loom.system.inc/pools/<pool> --token-file <copy> --worker <hostname> --until 1h --root ~/loom-serve/root --workspace ~/loom-serve/units`. Every box worker is strict, since the placer sends a pool only test jobs, and never `--exclusive`: the box is shared with other work (on Workshop, the tree builder and Kirk's own), so a unit clears only earlier units' leavings under its own root, never HOME's caches, and runs no `cloud/setup.sh` in HOME; a checkout unit on a box without adamic's toolchain is unfit there, named. The worker's name is the host's name, the same name its started events give as the machine. Each start copies the token into the unit's own runtime directory (mode 700), and serve reads that copy and removes it, so the token is never on a command line.

- **Restart.** `Restart=always`: serve ends every hour (`--until`) once the unit in hand finishes, or when it exits for any reason, and systemd starts it again 30 s later on whatever `~/.loom/bin/loom-runner` is then. A token the pool refuses (expired, say) makes it exit 2 and start again every 30 s, saying so in the journal, until a new token is in place.
- **Reload is a drain.** `systemctl --user reload loom-serve` sends `SIGHUP`: serve asks for nothing more, lets the unit in hand finish, and exits 0, and `Restart=always` starts the new runner. A drain never cuts an ask in flight, whose unit the pool has already taken off its queue.
- **Stop breaks only the unit in hand.** `systemctl --user stop loom-serve` sends `SIGTERM` to serve alone (`KillMode=mixed`): it kills the unit's process group, posts the unit broken to the wire (the judge places it again), and exits; whatever the unit left is killed once serve has. Nothing still queued is touched.

`install-serve` reads `serve.conf`, checks the token, writes the unit when its text changed (then `systemctl --user daemon-reload`), enables it and runs `systemctl --user reload-or-restart loom-serve.service`: a running serve drains, a stopped one starts. Run twice for one release, it costs one more drain. It refuses anything but Linux.

## Installing

Once per box, after the updater is installed and has a release with `install-serve`:

```bash
umask 077
printf 'pool = box-strict\n' > ~/.loom/serve.conf          # or: pool = box-phase, phase-jobs = yes
cat > ~/.loom/serve-token                                    # the token minted on Workshop, on stdin
printf '#!/bin/sh\nexec "$LOOM_UPDATE_BIN/loom-runner" install-serve\n' > ~/.loom/updated.d/50-serve
chmod 755 ~/.loom/updated.d/50-serve
~/.loom/bin/loom-runner install-serve
```

On Workshop, the token for each pool: `~/.loom/bin/loom pool token box-strict --hours 720` (30 days, as the placer's token lasts). Every box of a pool may share its token; a new one replaces `~/.loom/serve-token` and is read at serve's next start. `~/.loom/pools.json` lists each pool once, by the name the boxes' `serve.conf` give it, with the pin as its `runner` and the boxes' host names as its `machines`.

`loginctl enable-linger` (already on for the updater) keeps the unit running logged out. `systemctl --user status loom-serve` and `journalctl --user -u loom-serve` show it; `loom pool status <pool>` on Workshop shows each worker, when it last asked and what it took.

## Tests

`go test ./serving/ ./runner/... ./planner/ ./protocol/ ./cmd/loom/`: `serving` reads and refuses settings, renders the unit, writes it only when it changed, and refuses an open or missing token; `runner` refuses a job naming another runner before anything runs, stands down after one, and drains (the unit in hand passes, idle or standing down it ends at once); `loom-runner`'s own test sends itself `SIGHUP` mid-unit and sees serve exit 0, the unit passed. The unit file itself has run only in these tests, never under systemd: the first box to install it is its first real run.
