package judge

import _ "embed"

// ServiceName is the judge's systemd user unit on Workshop, and ServiceText that unit as this release has it: `loom
// install-units` (package daemons) writes it from the release a machine runs, so the unit never drifts from the binary
// it starts (Oct 10, #pzrz9r8).
const ServiceName = "loom-judge.service"

//go:embed systemd/loom-judge.service
var ServiceText string
