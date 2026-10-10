package protocol

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// AdamicRepository is the one repository a test job may name: public, fetched with no credentials.
const AdamicRepository = "https://github.com/system-inc/adamic"

// AdamicModule is the module path every package a test job names sits under.
const AdamicModule = "github.com/system-inc/adamic"

// MaximumPatternBytes bounds a -run or -skip pattern: a packed unit names its tests in one alternation.
const MaximumPatternBytes = 64 << 10

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// GofmtPhase is the one phase run.py doesn't hold, which the runner runs itself: gofmt -l over the change's .go files,
// red on any output (Loom, Oct 10 01:42Z). It names no unit, and it is the only phase job that carries ChangedPaths,
// since what it checks is exactly them.
const GofmtPhase = "gofmt"

// goVersionPattern is a Go release as go env GOVERSION names it (go1.27.1, go1.28rc1), with any experiments after it.
var goVersionPattern = regexp.MustCompile(`^go1\.[0-9]+(\.[0-9]+)?((rc|beta)[0-9]+)?( X:[a-z0-9,]+)?$`)

// phaseNamePattern is a phase of the box fast gate as run.py names it (coverage, vet, wasi, ...).
var phaseNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// phaseUnitPattern is a phase's one unit (a fixture, a stage 3 command, a catalog entry): a plain token, no leading dash.
var phaseUnitPattern = regexp.MustCompile(`^[A-Za-z0-9_.:/=+][A-Za-z0-9_.:/=+-]*$`)

// packagePattern is an import path under the module, each element a plain name: no "...", no leading dash, no space.
var packagePattern = regexp.MustCompile(`^github\.com/system-inc/adamic(/[A-Za-z0-9_][A-Za-z0-9._-]*)*$`)

// changedPathPattern is a path in the repository, as git names it: no leading slash or dash, no control characters.
var changedPathPattern = regexp.MustCompile(`^[^/\-\x00-\x1f\x7f][^\x00-\x1f\x7f]*$`)

// A TestJob is the unit a strict runner runs: a go test of the public adamic repository at a sha. It carries data,
// never a command: the runner fetches the sha, readies the tree with its own preparation, and builds the go test
// command itself, each field one argument. A unit carries a test job or argv, never both (docs/protocol.md).
type TestJob struct {
	Repository string `json:"repository"`
	// Sha is the commit the tests run at; Base, for a merge gate, the main it was merged onto.
	Sha  string `json:"sha"`
	Base string `json:"base,omitempty"`
	// Packages are the go test runs, one package each, with its -run and -skip patterns (empty: every test).
	Packages []TestPackage `json:"packages"`
	// GateInputs is the sha256 of the gate inputs' manifest in Loom's public store (the pinned TypeScript, the
	// formatters' libraries and corpora), fetched and checked by hash. Empty: none.
	GateInputs string `json:"gateInputs,omitempty"`
	// ChangedPaths are the paths the change touched, which the tests read through ADAMIC_GATE_CHANGED, and which a
	// gofmt phase job checks. No other phase job carries them.
	ChangedPaths []string `json:"changedPaths,omitempty"`
	// Sample is the commit ADAMIC_GATE_SAMPLE names, for the tests that sample a corpus by commit. Empty: unset.
	Sample string `json:"sample,omitempty"`
	// Phase, in place of Packages, is one non-test phase of the box fast gate: run.py's unit line, the phase and, for
	// wasi, stage3 or catalog, its one unit (the planner's phase units, keyParts.select.run). The runner builds
	// run.py --phase <phase> [--unit <unit>] itself, each a separate argument. Empty: a go test job.
	Phase string `json:"phase,omitempty"`
	// Tools is the gate tools' commit in the same public repository, whose cloud/fast-gate/run.py a phase job runs:
	// the ref its phase unit was keyed on. A phase job's only.
	Tools string `json:"tools,omitempty"`
	// Go is the Go release the gofmt phase's key names (keyParts.tools.go, go env GOVERSION): the runner runs only a
	// gofmt that release built. A gofmt phase job's only.
	Go string `json:"go,omitempty"`
	// Tree is the tree key of Workshop's build of the job's tree (builder.TreeKey, as `loom build-tree` prints it):
	// the runner reads trees/<Tree>.json from the action store and runs the prebuilt test binaries it names, each
	// fetched by sha256 with the tree's source and the products its tests read, and never builds. A go test job's only.
	// Empty: the runner compiles the packages with go test, as before Workshop built every tree.
	Tree string `json:"tree,omitempty"`
}

// A TestPackage is one package's go test: its import path and its -run and -skip patterns.
type TestPackage struct {
	Package string `json:"package"`
	Run     string `json:"run,omitempty"`
	Skip    string `json:"skip,omitempty"`
}

// CheckTestJob refuses a test job a strict runner mustn't run, naming why. It is the whole of what the job's
// fields may be: everything after it treats them as checked data.
func CheckTestJob(job TestJob) error {
	if job.Repository != AdamicRepository {
		return fmt.Errorf("repository %q: a test job runs only %s", job.Repository, AdamicRepository)
	}
	if !commitPattern.MatchString(job.Sha) {
		return fmt.Errorf("sha %q isn't a full commit, 40 lowercase hex digits", job.Sha)
	}
	if job.Base != "" && !commitPattern.MatchString(job.Base) {
		return fmt.Errorf("base %q isn't a full commit, 40 lowercase hex digits", job.Base)
	}
	if job.Sample != "" && !commitPattern.MatchString(job.Sample) {
		return fmt.Errorf("sample %q isn't a full commit, 40 lowercase hex digits", job.Sample)
	}
	if job.GateInputs != "" && !Sha256Pattern.MatchString(job.GateInputs) {
		return fmt.Errorf("gateInputs %q isn't a sha256, 64 lowercase hex digits", job.GateInputs)
	}
	if job.Phase != "" {
		fields := strings.Fields(job.Phase)
		switch {
		case len(job.Packages) > 0:
			return fmt.Errorf("a phase job names no packages: it runs a phase, not a go test")
		case !commitPattern.MatchString(job.Tools):
			return fmt.Errorf("tools %q isn't a full commit, 40 lowercase hex digits", job.Tools)
		case !commitPattern.MatchString(job.Base):
			return fmt.Errorf("a phase job needs its base, the main it was merged onto (run.py --base)")
		case strings.Join(fields, " ") != job.Phase || len(fields) > 2 || !phaseNamePattern.MatchString(fields[0]):
			return fmt.Errorf("phase %q isn't run.py's unit line, a phase and at most one unit", job.Phase)
		case len(fields) == 2 && !phaseUnitPattern.MatchString(fields[1]):
			return fmt.Errorf("phase %q: its unit %q isn't a plain token", job.Phase, fields[1])
		case fields[0] == GofmtPhase && len(fields) != 1:
			return fmt.Errorf("phase %q: gofmt names no unit, it checks the change's paths", job.Phase)
		case fields[0] != GofmtPhase && len(job.ChangedPaths) > 0:
			return fmt.Errorf("phase %q carries changed paths, and only gofmt's phase job checks them", job.Phase)
		case fields[0] == GofmtPhase && !goVersionPattern.MatchString(job.Go):
			return fmt.Errorf("go %q: a gofmt phase job names the Go release its key holds, as go env GOVERSION says it", job.Go)
		case fields[0] == GofmtPhase && len(job.ChangedPaths) == 0 && job.Base != job.Sha:
			// Only a witness, the base run against itself, changes nothing; any other gofmt with no paths checks nothing.
			return fmt.Errorf("a gofmt phase job with no changed paths would check nothing, and only a witness (base equal to sha) has none")
		}
	} else if job.Tools != "" {
		return fmt.Errorf("tools names a phase job's gate tools, and this job has no phase")
	}
	if job.Go != "" && job.Phase != GofmtPhase {
		return fmt.Errorf("go names the gofmt phase's Go release, and this job isn't gofmt's")
	}
	if job.Tree != "" && job.Phase != "" {
		return fmt.Errorf("tree names a build of test binaries, and a phase job runs none")
	}
	if job.Tree != "" && !Sha256Pattern.MatchString(job.Tree) {
		return fmt.Errorf("tree %q isn't a tree key, 64 lowercase hex digits", job.Tree)
	}
	if len(job.Packages) == 0 && job.Phase == "" {
		return fmt.Errorf("a test job names at least one package")
	}
	for _, testPackage := range job.Packages {
		if !packagePattern.MatchString(testPackage.Package) {
			return fmt.Errorf("package %q isn't an import path under %s", testPackage.Package, AdamicModule)
		}
		for name, pattern := range map[string]string{"run": testPackage.Run, "skip": testPackage.Skip} {
			if len(pattern) > MaximumPatternBytes {
				return fmt.Errorf("package %s: its %s pattern is over %d bytes", testPackage.Package, name, MaximumPatternBytes)
			}
			if strings.ContainsAny(pattern, "\x00\n\r") {
				return fmt.Errorf("package %s: its %s pattern holds a NUL or a line break", testPackage.Package, name)
			}
			if _, err := regexp.Compile(pattern); err != nil {
				return fmt.Errorf("package %s: its %s pattern isn't a regular expression: %v", testPackage.Package, name, err)
			}
		}
	}
	for _, path := range job.ChangedPaths {
		if !changedPathPattern.MatchString(path) || slices.Contains(strings.Split(path, "/"), "..") {
			return fmt.Errorf("changed path %q isn't a path in the repository", path)
		}
	}
	return nil
}
