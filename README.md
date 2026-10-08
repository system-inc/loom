# Loom

The kingdom's remote compute fabric: any job, on any machine we own or rent, streamed live, decided by our coordinator.

- `docs/protocol.md`: jobs, units, events, verdicts. Start here.
- `protocol/`: the Go types and the two shared decisions (expand a job into its plan, decide a run).
- `runner/`, `coordinator/`, `wire/` (the Cloudflare Worker): the parts, built in the order of task #loom.

Commands in, artifacts out. A runner never holds a credential; a missing unit is void, never green.
