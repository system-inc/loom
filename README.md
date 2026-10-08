# Loom

The kingdom's remote compute fabric: any job, on any machine we own or rent, streamed live, decided by our coordinator.
Commands in, artifacts out. A runner never holds a credential; a missing unit is void, never green.
`docs/protocol.md` is the contract: jobs, units, events, verdicts, tokens, the store and the wire's endpoints.
`protocol/` holds its Go types; `runner/` and `coordinator/` (with `cmd/loom`) are the Go module's parts; `wire/` is the Cloudflare Worker, live at https://loom-wire.kirk-ouimet.workers.dev.
The work and its laws live in the task tree at #system_adamic_developer_tools_loom.

Tests, from a clean clone: `go test ./...`, then in `wire/` `pnpm install --frozen-lockfile && pnpm test && pnpm check`. There is no CI service: Loom gates itself once the coordinator exists.
