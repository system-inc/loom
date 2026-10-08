# The Loom protocol, v0

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
| `run`, `unit` | The run id and the unit's planned id. |
| `argv` | The command. No shell unless argv names one. |
| `environment` | Variables set for the command, on top of a minimal base (`PATH`, `HOME`, `TMPDIR`, `LANG`). Nothing else from the runner's environment leaks in. |
| `directory` | Working directory, relative to the unit's workspace. |
| `inputs` | Files placed before the command starts: `path`, `sha256`, optional `mode` (octal string) and `archive` (`"tar"` unpacks at `path`). Every byte is verified against its hash; a mismatch refuses the unit. |
| `outputs` | Globs, relative to the workspace, hashed and uploaded after the command exits. |
| `timeoutSeconds` | After this the whole process group is killed and the unit exits `timeout`. |
| `resources` | `cpus` and `memoryMegabytes` the unit needs; placement reads it, the runner reports what it had. |
| `store` | `url` of the blob endpoint (`GET`/`PUT <url>/<sha256>`). |
| `wire` | Optional `url` to post events to. |
| `token` | The run's token. Scoped to one run's blobs and events, and it expires with the run. A runner holds nothing else. |

## Events

One JSON object per line. Every event carries `run`, `unit`, `sequence` (from 0, no gaps) and `time` (RFC 3339, UTC). A viewer or the coordinator detects a lost event by a gap in `sequence`.

| `type` | Fields |
|---|---|
| `started` | `machine`, `runnerVersion`, `cpus`, `memoryMegabytes`, `inputs` (path to sha256) |
| `output` | `stream` (`stdout` or `stderr`), `text` (one line, newline removed; invalid UTF-8 replaced and `replaced: true`) |
| `exit` | `code`, `signal` (if killed), `timedOut`, `wallSeconds`, `userSeconds`, `systemSeconds` |
| `uploaded` | `path`, `sha256`, `bytes` |
| `error` | `phase` (`fetch`, `start`, `run`, `upload`), `message` |
| `finished` | `status`: `passed` (exit 0, every output uploaded), `failed`, or `broken` (the runner couldn't do its job: fetch, start or upload failed) |

`finished` is always the last event of a unit. A unit without one never finished.

## Verdict

The coordinator decides a run from the plan and the events, never from a runner's say-so:

- **green**: every planned unit has `finished` with `passed`, each exactly once.
- **red**: some unit finished `failed`. The verdict names each one with its last output lines.
- **void**: anything else. A unit missing, late, duplicated, out of sequence or `broken`, or a unit nobody planned. Void is never green and never red: it says the run proved nothing.

## Store

Blobs are addressed by sha256. `GET <store>/<sha256>` returns the bytes, `PUT <store>/<sha256>` stores them, and the store recomputes the hash and refuses a mismatch. A `PUT` of a blob that already exists succeeds without rewriting it. Machines that build inputs (the coordinator's boxes) write to the bucket directly with their own keys; runners only ever use the run token.

## Cache (later)

A unit's result may be reused when its key matches: argv, environment, every input hash, directory, outputs, and the runner version. Each of those has a test that drops it from the key and must turn a hit into a miss. `--uncached` bypasses the cache, and an uncached run is what lands main.

## Tokens

`<base64url(claims JSON)>.<base64url(HMAC-SHA256(secret, first part))>`, base64url without padding. Claims: `run`, `scope` (`runner`, `viewer` or `coordinator`) and `expires` (Unix seconds). The coordinator mints them (`protocol.MintToken`); the Worker verifies them with the same secret (Worker secret `LOOM_TOKEN_SECRET`; on the coordinator's machine `~/.loom/token-secret`, mode 600). `TestTokenVector` pins one token so the Go and TypeScript sides sign the same bytes:

- secret `loom-test-secret`, claims `{"run":"r-vector","scope":"runner","expires":4102444800}`
- token `eyJydW4iOiJyLXZlY3RvciIsInNjb3BlIjoicnVubmVyIiwiZXhwaXJlcyI6NDEwMjQ0NDgwMH0.5yFFC9AOwC9L6zqwWz8V0aCJNrLwusF5LjbOv_hoDWY`

## The wire's endpoints

All on the Worker (`wire/`). A token goes in `Authorization: Bearer <token>`, or `?token=` where a browser can't set headers (the page and its WebSocket).

| Endpoint | Token | What it does |
|---|---|---|
| `POST /runs/<run>/plan` | coordinator | The run's plan: `{"units": ["<id>", ...]}`. Set once; a second, different plan is refused. |
| `POST /runs/<run>/events` | runner or coordinator | JSON lines of events for that run. Each is checked against the schema and the run id; an event already held (same unit and sequence) is dropped as a replay; out of order is held until its gap fills. |
| `POST /runs/<run>/verdict` | coordinator | `{"status", "failed", "problems"}` from `protocol.Decide`. Shown, never computed, by the Worker. |
| `GET /runs/<run>/stream` | any for the run | WebSocket: the tail so far, then every event as it is accepted, then the verdict. |
| `GET /runs/<run>` | any for the run | The live page: one row per planned unit, filling as it runs, red with its last lines the moment it fails. |
| `GET /blobs/<sha256>` | runner or coordinator | The blob's bytes from R2 (`loom-store`). |
| `PUT /blobs/<sha256>` | runner or coordinator | Stores the body if its sha256 matches; an existing blob is left as is (200). Up to 100 MB per blob in v0. |

A finished run's events are archived to R2 as `runs/<run>/events.jsonl`.
