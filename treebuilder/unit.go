package treebuilder

import _ "embed"

// ServiceName is the tree builder's systemd user unit on Workshop, and ServiceText that unit as this release has it: `loom
// install-units` (package daemons) writes it from the release a machine runs, so the unit never drifts from the binary
// it starts (Oct 10, #pzrz9r8).
const ServiceName = "loom-build-trees.service"

//go:embed systemd/loom-build-trees.service
var ServiceText string
