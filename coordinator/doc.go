// Package coordinator is Loom's only verdict authority. It runs on our own boxes: it takes a job file,
// writes the plan, places units onto machine slots longest first, starts the runner on each, collects their
// events and decides the run with protocol.Decide. A unit missing or late makes the run void, never green.
// Built by task #lmcoord; docs/protocol.md is the contract it keeps.
package coordinator
