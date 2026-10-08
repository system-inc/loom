# loom (the coordinator)

Runs a job file on Loom's slots and decides it. The only verdict authority (see `docs/protocol.md`).

```
loom run [--uncached] [--local <slots> | --slots <file>] [--pool <name>=<slots>]... [--wire <url>] <job.json>
loom pool status [--wire <url>] <name>
```

Exit codes: `0` green, `1` red, `2` void, `3` the run couldn't be set up. Build it with `go build ./cmd/loom`.

What a run does, in order:

1. Expands the job into its plan, checks every input is in the store (`HEAD`), and posts the plan. A missing input stops the run before its plan exists.
2. Places ready units onto free slots, longest first by `~/.loom/durations.tsv` (a unit never timed goes first). A unit runs once everything it `needs` has passed; one whose need didn't pass is never placed, and a `place` note says why.
3. On a box, takes one of the gate's slots the gate's own way: the same `flock` on `~/fast-gate/lock[-N]` and the same `taskset` CPU range as adamic's `cloud/fast-gate.sh`, so a Loom unit and a gate never share CPUs. Which slots Loom may use at all is `~/.loom/slots` (one `box class` per line), agreed with the gate's owner.
4. Streams the runner's events back over ssh and relays them to the wire itself, so the wire holds exactly the coordinator's record. Runners on our boxes get no wire, only the run's blob endpoint and token.
5. A box that drops a unit (its runner ends without `finished`, or runs past its timeout plus a minute) has it placed again once, on another machine when there is one. The record says so in `place` notes, and the unit's stream stays one gapless sequence across both attempts. A second drop leaves it unfinished, and the run void.
6. Decides with `protocol.Decide` over everything the runners said, posts the verdict, and prints it.

The cache: a unit marked `"cache": true` is looked up by `protocol.CacheKey` before it is placed and written after it passes; `--uncached` skips both. What lands main runs uncached.

The runner: built from this checkout for each box's platform, named for its commit (`git-<sha12>`, plus a hash of any uncommitted change to the Go module), installed at `~/.loom/bin/loom-runner-<version>` on the box. A version reaches a second box only after a green run on its first (`~/.loom/rollout.tsv`). Since the version is the whole module's, a new coordinator is held to the same law: its first run goes to one box.

`LOOM_SSH_BOX=<box> go test -run SSHMachineOnARealBox ./coordinator/` proves the ssh path on a real box.

The pool: `--pool <name>=<slots>` (repeatable) adds that many slots on a pool beside the others, for machines Loom can't ssh into (docs/protocol.md, "The pool"). Each slot queues its unit on the wire with its `wire` set, so the worker's runner posts the events itself, and follows the run's event log back (`GET /runs/<run>/events?after=`); a pool's slots share one follower per run. The late clock starts at queuing, so give a pool no more slots than it has workers. A re-placed pool unit can't finish in v1 (its runner numbers from 0 again and the wire refuses the conflict), so it leaves the run void. `loom pool status <name>` prints the queue and the workers seen in the last ten minutes.

## The board's other commands

- `loom board` prints the board's address with a fresh board token after the `#`.
- `loom gate-lines` reads what Adamic's gate is doing from its own files and posts it to the board every 3 s (docs/protocol.md, "The gate's lines"). On Kirk's Mac it runs as the LaunchAgent `com.loom.gate-lines`: `cmd/loom/com.loom.gate-lines.plist`, copied to ~/Library/LaunchAgents, running ~/.loom/bin/loom (`go build -o ~/.loom/bin/loom ./cmd/loom`), loaded with `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.loom.gate-lines.plist`, logging to ~/.loom/gate-lines.log.
- `loom top` draws the same lines in a terminal, redrawn every second.
