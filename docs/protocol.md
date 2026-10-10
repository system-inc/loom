# The Loom protocol, v0.2

Loom runs commands on machines we own or rent and turns in what they proved. Three things cross the wire: a **job** (what to run, written by a person or a tool), a **unit** (one command, handed to one runner), and **events** (what happened, streamed back). The Go types in `protocol/` are the source of truth; this document explains them. Unknown fields are refused everywhere, so a typo is an error and never a silent default.

```
  job file ──expand──► plan (every unit id, written first)
                           │
                 unit.json per unit ──► runner ──► events (JSON lines)
                                          │
                                   outputs by sha256 ──► store
                           │
            coordinator merges events against the plan ──► verdict
```

## Job

A job names its units. A unit may carry a `matrix`, which expands it into one unit per combination; `${matrix.<key>}` in its argv, env, inputs or outputs takes each value. `needs` lists unit ids (before expansion) that must be green first.

```json
{
  "name": "adamic-gate",
  "units": [
    {"id": "build", "argv": ["bash", "build.sh"], "outputs": [{"glob": "binaries/*.test"}], "timeoutSeconds": 900},
    {"id": "tests", "needs": ["build"], "matrix": {"shard": ["0", "1", "2"]},
     "argv": ["bash", "run-shard.sh", "${matrix.shard}"], "timeoutSeconds": 600}
  ]
}
```

Expansion gives `build`, `tests[shard=0]`, `tests[shard=1]`, `tests[shard=2]`. That list is the **plan**. It is written before anything runs, and the verdict is checked against it.

## Unit

What one runner receives. The coordinator fills in the store and wire addresses and the run token.

| Field | Meaning |
|---|---|
| `run`, `unit` | The run id and the unit's planned id. A run id is a letter or digit, then up to 127 letters, digits, `.`, `-` and `_` (`protocol.RunIdPattern`, the same the wire takes); a unit id is 1 to 256 bytes. |
| `argv` | The command. No shell unless argv names one. A unit carries `argv` or `test`, exactly one. |
| `test` | A structured go test of the adamic repository in place of a command (below). |
| `environment` | Variables set for the command, on top of a minimal base (`PATH`, `HOME`, `TMPDIR`, `LANG`, and the machine's facts `LOOM_SLOT` and `LOOM_SLOT_CPUS` when whatever started the runner set them). Nothing else from the runner's environment leaks in. |
| `directory` | Working directory, relative to the unit's workspace. |
| `inputs` | Files placed before the command starts: `path`, `sha256`, optional `mode` (octal string) and `archive` (`"tar"` unpacks at `path`). Every byte is verified against its hash; a mismatch refuses the unit. |
| `outputs` | Globs, relative to the workspace, hashed and uploaded after the command exits. |
| `timeoutSeconds` | After this the whole process group gets `SIGTERM`, then `SIGKILL` 5 s later. The `exit` event says `timedOut: true` and the unit finishes `failed`; there is no separate timeout status. |
| `resources` | `cpus` and `memoryMegabytes` the unit needs; placement reads it, the runner reports what it had. |
| `store` | `url` of the run's blob endpoint, `<wire>/runs/<run>/blobs` (`GET`/`PUT <url>/<sha256>`). |
| `wire` | Optional `url` to post events to. |
| `token` | The run's token. It posts events to its own run and reads and writes blobs only through its own run's blob endpoint, as Store says. It expires with the run. A runner holds nothing else. |

`protocol.CheckUnit` checks what decoding can't: the id limits above, local paths, 64-hex hashes, a plain mode, a positive timeout, a store when there are inputs or outputs. The runner calls it before anything runs (a unit it refuses finishes `broken`), and the coordinator calls it before handing a unit out.

### Test jobs

`test` (`protocol.TestJob`) is data, never a command: `repository` (exactly `https://github.com/system-inc/adamic`), `sha`, `base` (for a merge gate), `packages` (each `{"package", "run", "skip"}`: an import path under the module and its `-run` and `-skip` patterns), `gateInputs` (the sha256 of the gate inputs' manifest in the public store), `changedPaths`, `sample`, `tree` (the key of Workshop's build of the job's tree, 64 lowercase hex: the runner then runs the prebuilt test binaries `trees/<tree>.json` names and builds nothing; a go test job's only), and `runner` (the runner binary's sha256 the unit's key names: a serving runner hands the job to that runner, fetched from the release store, and a runner given it by hand whose own sha256 differs refuses it as unfit, broken, before anything runs; docs/serving.md). `protocol.CheckTestJob` is the whole of what they may be. The runner fetches the commit from the public repository itself, readies it with its own compiled-in preparation, and builds each `go test` command, every pattern one argument. A strict runner (`loom-runner serve --strict`, the Codex pool's) runs nothing else: README.md, "What a strict worker does", is the line by line account. The cache key includes `test`.

## Events

One JSON object per line. Every event carries `run`, `unit`, `sequence` (from 0, no gaps) and `time` (RFC 3339, UTC). A viewer or the coordinator detects a lost event by a gap in `sequence`.

| `type` | Fields |
|---|---|
| `started` | `machine`, `runnerVersion`, `runnerSha256` (the runner binary's sha256), `cpus`, `memoryMegabytes`, `inputs` (path to sha256) |
| `output` | `stream` (`stdout` or `stderr`), `text` (one line, newline removed; invalid UTF-8 replaced and `replaced: true`) |
| `exit` | `code`, `signal` (if killed), `timedOut`, `wallSeconds`, `userSeconds`, `systemSeconds` |
| `uploaded` | `path`, `sha256`, `bytes` |
| `error` | `phase`, `message` |
| `cached` | `key` (the unit's cache key), `fromRun` (the run that proved it), `events` (sha256 of that unit's event log) |
| `timing` | `timing`: what only the runner knows of the unit's time and size (`protocol.Timing`), one event just before `finished`: `fetchSeconds`, `unpackSeconds`, `prepareSeconds`, `testSeconds`, `storeBytes`, `cacheBytes`, `houseBytes`, `peakMegabytes` (its cgroup's `memory.peak`), `shareCpus`, `shareMemoryMegabytes`, `unitsInHand` and `load` (at its start), each left off at zero |
| `finished` | `status`: `passed`, `failed` or `broken` (below) |

`finished` is always the last event of a unit. A unit without one never finished.

**Zeros are left off.** Go writes these fields with `omitempty`, so a zero doesn't appear: an empty output line has no `text`, a zero-byte upload no `bytes`, an instant exit no `wallSeconds`. A field the type allows but the line lacks reads as its zero (`""`, `0`, `false`, `{}`), and every viewer must read it that way. `code` is the exception: exit code 0 is always written, and an `exit` with no `code` was killed by `signal`. `protocol/testdata/events.jsonl` is what Go writes; the Worker's tests post it as it stands.

**Every unit leaves a row.** Once the judge has posted a run, it writes one row for each unit that reported in it (`judge/rows.go`): run, future, attempt, unit key, name and kind, the worker, when the run was placed (the placer's ledger), started and finished, the queue wait between placed and started, the `exit` event's seconds, the `finished` status, and the `timing` event's fields at the row's top level. A run's rows are one object, `units/<YYYY>/<MM>/<DD>/<run>.jsonl` in `loom-artifacts` (the day it was decided, UTC), under no lifecycle rule. `loom units [--days N | --day YYYY-MM-DD] [--name <text>] [--box <text>] [--by name|box|day] [--json]` reads them with the store's key and prints each group's count, statuses, and the p50 and p90 of its wall, queue wait, phases and peak memory, over the rows that say each.

**Error phases**, where the runner was when it went wrong:

| `phase` | Covers |
|---|---|
| `start` | Checking the unit, making its workspace, starting its command. |
| `fetch` | Fetching or verifying an input. |
| `run` | While the command runs: the runner was stopped, its output couldn't be read, or a process that left the unit's group held the output open after the command exited. |
| `upload` | Finding, hashing or uploading a declared output. |
| `wire` | Posting events to the wire. Never changes the status: stdout holds the whole stream either way. |
| `place` | The coordinator's own note in a unit's stream: a box that dropped the unit (its runner ended without `finished`, or ran past its timeout), and where the unit was placed again. A unit is placed again once; a second drop leaves it unfinished, and the run void. |

**How a unit finishes:**

- `passed`: the command exited 0 and every declared output was uploaded.
- `failed`: the command's doing. A nonzero exit, a timeout, a signal, or an output it declared but didn't make: a glob that matches no file, or a match that resolves outside the workspace.
- `broken`: the runner couldn't do its job, so nothing was proved. A unit `CheckUnit` refuses, an input that won't fetch or verify, a command that won't start, output that couldn't be read, a store that refuses an upload, or the runner itself stopped by a signal (`SIGINT` or `SIGTERM`), which kills the unit's group first.

When more than one applies, the worse wins: `broken` over `failed` over `passed`. Outputs are uploaded whatever the exit, since a failed unit's logs are what a person reads next.

## Verdict

The coordinator decides a run from the plan and the events, never from a runner's say-so:

- **green**: every planned unit has `finished` with `passed`, each exactly once.
- **red**: some unit finished `failed`. The verdict names each one with its last output lines.
- **void**: anything else. A unit missing, late, duplicated, out of sequence or `broken`, or a unit nobody planned. Void is never green and never red: it says the run proved nothing.

`protocol.Verdict` is also the body of the verdict endpoint: exactly `{"status", "failed", "problems", "cached"}`, lowercase, with an empty list written as `[]`, never `null`. `cached` names the units served from the cache instead of run; it never changes the status, and a run that lands main has it empty.

## Store

Blobs are addressed by sha256 and live in R2 (`loom-runs`) at `blobs/<sha256>`. A blob is reached only through a run: `GET <wire>/runs/<run>/blobs/<sha256>` returns the bytes and `PUT` stores them, the Worker recomputing the hash and refusing a mismatch. A `PUT` of a blob that already exists succeeds without taking its bytes again. The unit's `store.url` is that run's endpoint, so a runner never sees a key, a bucket or another run.

What each token may do there (its run must be the run in the path):

| Token | `GET` | `PUT` |
|---|---|---|
| runner | a hash the run's plan declares as an input, or one this run has uploaded (an earlier unit's output) | any hash, recorded as the run's |
| coordinator | any hash | any hash |
| viewer | a hash this run has uploaded, so a person can read a unit's logs | no |

This is the run-scoped access the store promised, and it is better than an S3 presigned URL: a runner's output hashes aren't known until it runs, so a write can't be presigned in advance, and a presigned `PUT` would skip the hash check. The run token already expires with the run and is scoped to it. A run's uploads are listed in `runs/<run>/blobs.jsonl` when it is archived.

- A `PUT` needs `Content-Length` (411 without it), and the body must be exactly that long.
- Up to 100 MiB (104,857,600 bytes) per blob; more is 413. A bigger input is split into several, one per file.
- The coordinator's machines upload inputs through the same endpoint with a coordinator token; nothing holds an R2 key but the Worker.
- Both buckets delete what they hold a fixed time after its upload, each its own: `loom-runs` everything after 7 days, `loom-artifacts` its `blobs/`, `refs/` and `trees/` after 30 (Kirk, Oct 10). Whatever is used is kept fresh rather than exempted, which would grow the buckets forever. A held blob that a `PUT` or a `HEAD` reaches, or that a ref or cache entry being written names, and that was uploaded longer ago than its bucket's freshness, is written again onto itself in R2: its own bytes, streamed from R2 back into R2 and checked against its sha256 again, never sent by the writer. That starts its bucket's days over. The freshness leaves the last days of each lifecycle for whoever reads it: `RunsFreshForMilliseconds` the last two of `loom-runs`' 7, and `FreshForMilliseconds`, builder/store.go's `FreshFor`, the last five of `loom-artifacts`' 30, so a held artifact is copied once a lifecycle, not every few days. A held object that doesn't hash to its name is replaced by the writer's verified body.

### The public store

Open-source projects (Adamic, cohere) keep their build products and test inputs in a public bucket instead, by Kirk's call (Oct 8): `loom-artifacts`, read by anyone direct at `https://artifacts.loom.system.inc/blobs/<sha256>`, so a hundred instances fetch from Cloudflare's edge and never through the Worker. Every reader verifies the sha256 it asked for, so a public read can't be poisoned. Writes stay authenticated: `PUT /public/blobs/<sha256>` on the Worker, a coordinator token of any run, the same hash check, and `HEAD` to skip a blob already held (refreshed when stale, as Store says); there is no public write. Uploads come from a machine with a fast uplink (Workshop's fiber, or a Codex instance while the star owns Workshop), never from Kirk's home connection. `loom-runs` and its run-scoped access are unchanged for every other project.

The edge keeps what never changes. A Cache Rule on the system.inc zone (ruleset `f38094946f90495f9213f085a9f95c67`, the zone's `http_request_cache_settings` entrypoint; rule `0487c4418dae403baa83bc48ddb9b12f`, ref `loom_artifacts_immutable_blobs`, Oct 10) matches

    (http.host eq "artifacts.loom.system.inc" and (starts_with(http.request.uri.path, "/blobs/") or starts_with(http.request.uri.path, "/releases/blobs/")))

and makes it eligible for cache with an edge TTL of a year (`override_origin`, 31536000 s), browser TTL as the origin says, and every status from 300 up never stored (`status_code_ttl` from 300, `no-store`), so a blob missing now is never remembered missing. A blob is named by its sha256, so a cached copy is always right, even one the lifecycle has since taken from R2: a builder asks the bucket itself whether it holds a blob, never the edge. Everything else on the domain, `releases/current.txt`, `refs/`, `trees/` and `gate-inputs/`, is outside the rule and answers `cf-cache-status: DYNAMIC`. Measured after the rule went in: a blob's first download `MISS`, its second `HIT`; current.txt, a ref and a tree index `DYNAMIC` both times; a missing blob 404 `BYPASS`.

## Cache

A unit that passed may stand in for a later unit with the same **cache key**: `protocol.CacheKey(unit, runnerVersion, platform)`, the sha256 of a canonical encoding of the unit's argv, environment, directory, every input (path, sha256, mode, archive), outputs and timeout, plus the runner version and the platform (`linux/amd64`, `darwin/arm64`). The run id, unit id, resources, store, wire and token are left out: they say where a unit ran, not what it computed. Every part of the key has a test that changes it and must turn a hit into a miss, and a mutant that drops the part and must fail that test.

Caching is opt-in per job unit: `"cache": true` says the unit is hermetic, its result depending on nothing its key doesn't hold. It is off by default, because a unit that reads a machine's own state (a warm checkout, a build cache) has inputs its key can't see.

A job unit may also say `"expectedSeconds"`: the planner's estimate of its wall. The coordinator places ready units longest first by it, and only for a unit without one by the wall that unit id recorded last run (`~/.loom/durations.tsv`), since a planner that re-cuts its units every run reuses ids for different work. It never reaches the runner and is no part of a cache key.

The coordinator keeps the cache. After a unit passes it writes a `protocol.CacheEntry` (`key`, `run`, `unit`, `machine`, `runnerVersion`, `wallSeconds`, `outputs` with path, sha256 and bytes, `events`, the sha256 of the unit's event log as a blob) to `PUT /cache/<key>`. Before placing a unit it reads `GET /cache/<key>`; on a hit it posts the unit's stream itself, `cached` then `finished passed`, and the verdict lists the unit under `cached`. Only passed units are cached, an entry is written once and never replaced, and the Worker refuses an entry whose outputs or event log aren't in the store, and refreshes each that is stale (Store), so an entry never names a blob about to expire.

`--uncached` bypasses the cache entirely: no reads, every unit runs. An uncached run is what lands main, and its verdict's `cached` is empty.

## Tokens

`<base64url(claims JSON)>.<base64url(HMAC-SHA256(secret, first part))>`, base64url without padding. Claims: `run`, `scope` (`runner`, `viewer`, `coordinator`, `board` or `pool`) and `expires` (Unix seconds). The coordinator mints them (`protocol.MintToken`, which refuses a run id the wire wouldn't take); the Worker verifies them with the same secret (Worker secret `LOOM_TOKEN_SECRET`; on the coordinator's machine `~/.loom/token-secret`, mode 600).

The HMAC key is the secret's **text as it stands**, surrounding whitespace trimmed: the file holds hex digits, and those digits are signed as UTF-8 text, never decoded to bytes. `protocol.ReadTokenSecret` reads it that way. Both sides verify alike (`protocol.VerifyToken`, the Worker's `verifyToken`): the signature first, then claims with exactly the keys `run`, `scope` and `expires` in that exact case and nothing else, a non-empty run, a known scope, a positive expiry, and not expired. `TestTokenVector` pins one token so the Go and TypeScript sides sign the same bytes:

- secret `loom-test-secret`, claims `{"run":"r-vector","scope":"runner","expires":4102444800}`
- token `eyJydW4iOiJyLXZlY3RvciIsInNjb3BlIjoicnVubmVyIiwiZXhwaXJlcyI6NDEwMjQ0NDgwMH0.5yFFC9AOwC9L6zqwWz8V0aCJNrLwusF5LjbOv_hoDWY`

## The pool

Machines Loom can't ssh into (Codex cloud instances) pull their units instead. A **pool** (`codex`, say) is a queue of units on the wire. Each instance runs `loom-runner serve`, which asks the pool for its next unit, runs it, and asks again until its deadline, posting each unit's events straight to the run's events endpoint with the unit's own run token. The coordinator puts units in and reads their events back from the wire. One long Codex turn runs one `serve`, which runs many units, and its output stays in a file, off the model.

A **strict pool** (`loom run --strict-pool codex-strict=<slots>`) is one whose workers serve `--strict`: the coordinator places only units carrying a test job there, never argv, and while a run has one, its test jobs go only to it. `adamic-gate test-jobs --job <job.json>` rewrites a planned job's go test units (tests and products) as test jobs and leaves every unit a test job can't say exactly (build-vet, phases, stage 3, select, the `@unplanned` remainder) as argv, for a box.

A **pool token** (scope `pool`, run = the pool's name) is all an instance holds besides the units it is handed: it reaches its pool's `next` and nothing else. Each unit carries its own run token, store and wire, as any unit does.

- `POST /pools/<pool>/units` (a coordinator token of any run): `{"units": [<unit>, ...]}`, each a whole `protocol.Unit` with its `wire` set. They join the end of the queue in the order given (the coordinator sends longest first). Answers `{"queued": <the queue's length>}`.
- `POST /pools/<pool>/next` (a pool token for that pool): `{"worker": "<name>", "cpus": <n>}`. Answers 200 with one unit, taken off the queue, or 204 when none arrives within 20 s (the call waits, so an idle instance asks about three times a minute).
- `POST /pools/<pool>/cancel` (a coordinator token): `{"run": "<run>"}` drops that run's units still queued; answers `{"dropped": <n>}`.
- `POST /pools/<pool>/queued` (a coordinator token): `{"run": "<run>"}` answers `{"units": ["<unit>", ...]}`, that run's units still queued, oldest first. A unit that has left the queue but whose runner never posted an event was handed to a worker that is gone (an ask whose turn ended as it was answered); the coordinator queues it again after two minutes. It posted no events, so nothing in the run's log conflicts with the next attempt.
- `GET /pools/<pool>` (a coordinator or board token): `{"queued": <n>, "workers": [{"worker", "cpus", "seenAt", "took"}]}`: every worker seen in the last four hours (a worker running a unit asks nothing until it ends), when it last asked, and the unit it last took. The board shows a worker that asked in the last ten minutes or is running a unit.
- `GET /runs/<run>/events?after=<position>` (a coordinator token for the run): the log's events after that position as JSON lines (`{"position", "event"}` each), waiting up to 20 s for the first one when there are none yet. The coordinator follows a pool unit's stream this way: plain HTTP, no WebSocket client needed.

`loom-runner serve --pool <wire>/pools/<pool> --token-file <file> --worker <name> --until <duration> [--strict]` reads the pool token from the file and removes it, so it is never on the runner's command line. It exits 0 at its deadline, finishing the unit in hand first if it can within the unit's timeout, the same on `SIGHUP` (a drain: nothing more is asked for, and the unit in hand finishes), or at once on `SIGTERM` (the unit is then broken, as with any stopped runner). A serving runner's started events name its `--worker` as the machine. A unit whose job names another runner is started by serve itself, then continued by that runner, fetched from the release store (docs/serving.md). A Linux box serves through the `loom-serve` user unit, docs/serving.md.

## The board

One live view of every run on every machine. A single board object (a Durable Object named `board`) holds a summary of each run and each machine and streams changes to its viewers. Each run's own object feeds it, at most twice a second per run, so a run whose runners post straight to the wire appears too. The coordinator adds what only it knows: each machine's cores and slots, so an idle slot shows as idle.

A **board token** (scope `board`, run `board`) watches the board and nothing else: no run endpoint takes it. It can ask the board for a viewer token to any one run, to open that run's live page and stream.

A run's summary, as the board holds it:

```json
{"run": "adamic-gate-20261008T200212-20fe227d", "job": "adamic-gate", "plannedAt": "...", "updatedAt": "...",
 "units": 11, "queued": 3, "running": 6, "passed": 1, "failed": 1, "broken": 0, "cached": 0, "verdict": null,
 "active": [{"unit": "tests[shard=3]", "machine": "home", "since": "..."}],
 "failures": [{"unit": "tests[shard=1]", "at": "...", "status": "failed", "lines": ["last", "five", "output", "lines"]}]}
```

`job` is the run id without its time and random suffix. `queued` counts planned units that haven't started; `running` those started and not finished; `verdict` is null until the run has one, then green, red or void. `active` lists every running unit; `failures` the last 20 units that finished failed or broken, newest first, each with its last five output lines. Runs leave the board 24 hours after their verdict, and a run that has seen no event for 24 hours leaves too.

A machine, as the coordinator posts it and the board adds to: `{"name": "home", "cores": 64, "slots": 3}`, plus `running`, the number of active units on it across every run.

The stream (`GET /board/stream`, WebSocket) sends `{"kind": "snapshot", "runs": [...], "machines": [...], "pulse": {...}}` first, then `{"kind": "run", "run": {...}}` and `{"kind": "machines", "machines": [...]}` as they change, then one `{"kind": "pulse", "running", "finishedLastMinute", "queued", "longest"}` after each batch of changes: units running now, units finished in the last minute, units queued, and the longest-running unit (`{"unit", "run", "machine", "since"}`, or null). A run with its verdict counts toward no pulse or machine. `passed` includes cached units, so queued, running, passed, failed and broken add up to `units`. The page passes its token as the WebSocket subprotocol `token.<token>` beside `loom`, so the token is never in a URL; the page's own address carries it after `#`, which a browser never sends.

## The gate's lines

The board also shows Adamic's fast gate and whole gate, machine by machine and slot by slot, read from their own files only by `loom gate-lines` on Kirk's Mac (package `gatelines`): the watcher's `slots`, `running/`, `running-started/` and `front` in ~/.adamic-fast-gate-watch, and over ssh each running gate's `status.txt` and `box.txt` on its box (and the whole gate's newest run on Home). It is read-only on the gate's state and posts the whole picture to `POST /board/gate` (a coordinator token of any run) every 3 s when it changed, and every 30 s regardless. The board keeps the latest, includes it in the snapshot as `gate`, and streams `{"kind": "gate", "gate": {...}}` when it changes.

```json
{"at": "2026-10-08T21:07:43Z", "machines": [{"name": "threadripper", "aliases": ["cloud"], "cores": 64, "lines": [
  {"slot": 3, "class": "B", "state": "gating", "kind": "fast", "branch": "cloud/land-stack-s1-views-slice1", "sha": "6fbfdbf74194",
   "step": "tests", "since": "2026-10-08T21:00:22Z", "star": true, "detail": ""}]}]}
```

`state` is `gating`, `green`, `red` (red the moment status.txt says so, while the gate still runs for triage), `crash` (void: a box or tool failure, or a gate stopped before its verdict) or `idle`. `step` is the step whose log the gate wrote last. `star` says the branch matches a glob in the watcher's `front` file. A slot whose gate just ended shows that gate's own final status for 90 s, then goes idle. A machine with no gate slots has one idle line.

## The wire's endpoints

All on the Worker (`wire/`). A token goes in `Authorization: Bearer <token>`, or `?token=` where a browser can't set headers (the page and its WebSocket).

| Endpoint | Token | What it does |
|---|---|---|
| `POST /runs/<run>/plan` | coordinator | The run's plan, `protocol.Plan`: `{"units": ["<id>", ...], "inputs": ["<sha256>", ...]}`, every unit id and every input hash the plan names (`protocol.PlanOf` builds it). Set once; the same plan again is 200, a different one 409. |
| `POST /runs/<run>/events` | runner or coordinator | JSON lines of events for that run. Each is checked against the schema and the run id, and a batch lands whole or not at all. An event identical to one already held (same unit and sequence, same content) is dropped as a replay. The same unit and sequence with **different** content is a conflict, not a replay: the first stays, and the answer is 409 with `conflicts`, a list of `"<unit> <sequence>"`. Out of order is held until its gap fills. |
| `POST /runs/<run>/verdict` | coordinator | `protocol.Verdict` from `protocol.Decide`. Shown, never computed, by the Worker. |
| `GET /runs/<run>/stream` | any for the run | WebSocket: the tail so far, then every event as it is accepted, then the verdict. |
| `GET /runs/<run>` | any for the run | The live page: one row per planned unit, filling as it runs, red with its last lines the moment it fails. |
| `GET /runs/<run>/blobs/<sha256>` | as Store says | The blob's bytes from R2. A hash the token may not read is 403; one not in the store is 404. A viewer token may come as `?token=`. |
| `HEAD /runs/<run>/blobs/<sha256>` | as `GET` | 200 with `Content-Length` when the blob is in the store, 404 when not. The coordinator asks before uploading an input, so a blob already held costs no bytes, and one held but stale is refreshed first (Store). It records nothing: only a `PUT`, which proves the bytes, counts as the run's upload. |
| `PUT /runs/<run>/blobs/<sha256>` | runner or coordinator | Stores the body if its sha256 matches (201); an existing blob isn't taken again, only refreshed when stale (200, `refreshed` says which). Either way the hash is recorded as uploaded by the run. Needs `Content-Length`; up to 100 MiB. |
| `GET /board/stream` | board | WebSocket: the board's snapshot, then every change (see The board). The token comes as the subprotocol `token.<token>`. |
| `GET /board/snapshot` | board | The same snapshot as JSON. |
| `POST /board/gate` | coordinator (any run) | The gate's lines, posted whole (see The gate's lines). |
| `POST /board/machines` | coordinator (any run) | `{"machines": [{"name", "cores", "slots"}]}`: the machines this coordinator places units on. Each replaces the board's entry of that name. |
| `POST /board/runs/<run>/viewer` | board | `{"token": "<viewer token for that run>"}`, expiring with the board token. |
| `HEAD`, `PUT /public/blobs/<sha256>` | coordinator (any run) | The public store's write side (see The public store). Reads go direct to artifacts.loom.system.inc, never here. |
| `GET /cache/<key>` | coordinator (any run) | The `protocol.CacheEntry` for that key, or 404. |
| `PUT /cache/<key>` | coordinator (any run) | Writes the entry once (201); an existing one is left as is (200). Refused when its `key` isn't the path's, or its outputs or event log aren't in the store. |

A finished run's events are archived to R2 as `runs/<run>/events.jsonl`, and its uploads as `runs/<run>/blobs.jsonl` (`{"sha256", "bytes", "scope"}` per line). Cache entries live at `cache/<key>`.
