# The Loom protocol, v0.1

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
| `argv` | The command. No shell unless argv names one. |
| `environment` | Variables set for the command, on top of a minimal base (`PATH`, `HOME`, `TMPDIR`, `LANG`). Nothing else from the runner's environment leaks in. |
| `directory` | Working directory, relative to the unit's workspace. |
| `inputs` | Files placed before the command starts: `path`, `sha256`, optional `mode` (octal string) and `archive` (`"tar"` unpacks at `path`). Every byte is verified against its hash; a mismatch refuses the unit. |
| `outputs` | Globs, relative to the workspace, hashed and uploaded after the command exits. |
| `timeoutSeconds` | After this the whole process group gets `SIGTERM`, then `SIGKILL` 5 s later. The `exit` event says `timedOut: true` and the unit finishes `failed`; there is no separate timeout status. |
| `resources` | `cpus` and `memoryMegabytes` the unit needs; placement reads it, the runner reports what it had. |
| `store` | `url` of the blob endpoint (`GET`/`PUT <url>/<sha256>`). |
| `wire` | Optional `url` to post events to. |
| `token` | The run's token. Its events go to its own run only; blobs are by hash for any runner or coordinator token (see Store). It expires with the run. A runner holds nothing else. |

`protocol.CheckUnit` checks what decoding can't: the id limits above, local paths, 64-hex hashes, a plain mode, a positive timeout, a store when there are inputs or outputs. The runner calls it before anything runs (a unit it refuses finishes `broken`), and the coordinator calls it before handing a unit out.

## Events

One JSON object per line. Every event carries `run`, `unit`, `sequence` (from 0, no gaps) and `time` (RFC 3339, UTC). A viewer or the coordinator detects a lost event by a gap in `sequence`.

| `type` | Fields |
|---|---|
| `started` | `machine`, `runnerVersion`, `cpus`, `memoryMegabytes`, `inputs` (path to sha256) |
| `output` | `stream` (`stdout` or `stderr`), `text` (one line, newline removed; invalid UTF-8 replaced and `replaced: true`) |
| `exit` | `code`, `signal` (if killed), `timedOut`, `wallSeconds`, `userSeconds`, `systemSeconds` |
| `uploaded` | `path`, `sha256`, `bytes` |
| `error` | `phase`, `message` |
| `finished` | `status`: `passed`, `failed` or `broken` (below) |

`finished` is always the last event of a unit. A unit without one never finished.

**Zeros are left off.** Go writes these fields with `omitempty`, so a zero doesn't appear: an empty output line has no `text`, a zero-byte upload no `bytes`, an instant exit no `wallSeconds`. A field the type allows but the line lacks reads as its zero (`""`, `0`, `false`, `{}`), and every viewer must read it that way. `code` is the exception: exit code 0 is always written, and an `exit` with no `code` was killed by `signal`. `protocol/testdata/events.jsonl` is what Go writes; the Worker's tests post it as it stands.

**Error phases**, where the runner was when it went wrong:

| `phase` | Covers |
|---|---|
| `start` | Checking the unit, making its workspace, starting its command. |
| `fetch` | Fetching or verifying an input. |
| `run` | While the command runs: the runner was stopped, its output couldn't be read, or a process that left the unit's group held the output open after the command exited. |
| `upload` | Finding, hashing or uploading a declared output. |
| `wire` | Posting events to the wire. Never changes the status: stdout holds the whole stream either way. |

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

`protocol.Verdict` is also the body of the verdict endpoint: exactly `{"status", "failed", "problems"}`, lowercase, with an empty list written as `[]`, never `null`.

## Store

Blobs are addressed by sha256. `GET <store>/<sha256>` returns the bytes, `PUT <store>/<sha256>` stores them, and the store recomputes the hash and refuses a mismatch. A `PUT` of a blob that already exists succeeds without rewriting it. Machines that build inputs (the coordinator's boxes) write to the bucket directly with their own keys; runners only ever use the run token.

- **Layout.** In R2 (`loom-store`) a blob lives at `blobs/<sha256>`, and a finished run's log at `runs/<run>/events.jsonl` (with `runs/<run>/held.jsonl` beside it when events were still waiting on a gap).
- **Access is by hash, not by run.** Any valid runner or coordinator token reads and writes any blob; a viewer token reads none. A hash names its bytes, so a blob is no secret to the runs that share it; per-run presigned access comes with the store task (#lmstore).
- **A `PUT` needs `Content-Length`** (411 without it), and the body must be exactly that long.
- **Size.** Up to 100 MiB (104,857,600 bytes) per blob in v0.1; more is 413.

## Cache (later)

A unit's result may be reused when its key matches: argv, environment, every input hash, directory, outputs, and the runner version. Each of those has a test that drops it from the key and must turn a hit into a miss. `--uncached` bypasses the cache, and an uncached run is what lands main.

## Tokens

`<base64url(claims JSON)>.<base64url(HMAC-SHA256(secret, first part))>`, base64url without padding. Claims: `run`, `scope` (`runner`, `viewer` or `coordinator`) and `expires` (Unix seconds). The coordinator mints them (`protocol.MintToken`, which refuses a run id the wire wouldn't take); the Worker verifies them with the same secret (Worker secret `LOOM_TOKEN_SECRET`; on the coordinator's machine `~/.loom/token-secret`, mode 600).

The HMAC key is the secret's **text as it stands**, surrounding whitespace trimmed: the file holds hex digits, and those digits are signed as UTF-8 text, never decoded to bytes. `protocol.ReadTokenSecret` reads it that way. Both sides verify alike (`protocol.VerifyToken`, the Worker's `verifyToken`): the signature first, then claims with exactly the keys `run`, `scope` and `expires` in that exact case and nothing else, a non-empty run, a known scope, a positive expiry, and not expired. `TestTokenVector` pins one token so the Go and TypeScript sides sign the same bytes:

- secret `loom-test-secret`, claims `{"run":"r-vector","scope":"runner","expires":4102444800}`
- token `eyJydW4iOiJyLXZlY3RvciIsInNjb3BlIjoicnVubmVyIiwiZXhwaXJlcyI6NDEwMjQ0NDgwMH0.5yFFC9AOwC9L6zqwWz8V0aCJNrLwusF5LjbOv_hoDWY`

## The wire's endpoints

All on the Worker (`wire/`). A token goes in `Authorization: Bearer <token>`, or `?token=` where a browser can't set headers (the page and its WebSocket).

| Endpoint | Token | What it does |
|---|---|---|
| `POST /runs/<run>/plan` | coordinator | The run's plan, `protocol.Plan`: `{"units": ["<id>", ...]}` (`protocol.PlanOf` builds it). Set once; the same plan again is 200, a different one 409. |
| `POST /runs/<run>/events` | runner or coordinator | JSON lines of events for that run. Each is checked against the schema and the run id, and a batch lands whole or not at all. An event identical to one already held (same unit and sequence, same content) is dropped as a replay. The same unit and sequence with **different** content is a conflict, not a replay: the first stays, and the answer is 409 with `conflicts`, a list of `"<unit> <sequence>"`. Out of order is held until its gap fills. |
| `POST /runs/<run>/verdict` | coordinator | `protocol.Verdict` from `protocol.Decide`. Shown, never computed, by the Worker. |
| `GET /runs/<run>/stream` | any for the run | WebSocket: the tail so far, then every event as it is accepted, then the verdict. |
| `GET /runs/<run>` | any for the run | The live page: one row per planned unit, filling as it runs, red with its last lines the moment it fails. |
| `GET /blobs/<sha256>` | runner or coordinator | The blob's bytes from R2 (`loom-store`). |
| `PUT /blobs/<sha256>` | runner or coordinator | Stores the body if its sha256 matches (201); an existing blob is left as is (200). Needs `Content-Length`; up to 100 MiB. |

A finished run's events are archived to R2 as `runs/<run>/events.jsonl`.
