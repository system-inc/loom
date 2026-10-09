package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/system-inc/loom/protocol"
)

// testJobs rewrites a planned job for the codex-strict pool (#098rcha, after #1pe3ndh): every unit the strict runner
// can run exactly, a go test unit (tests and products alike, the opening and unitBody with the selection's env before
// them), becomes a structured test job; every other unit keeps its argv and goes to a box. A unit is converted only
// when every part of it has a test job field that says the same thing, so nothing it did is lost.
//
//	adamic-gate test-jobs --job <job.json> > <job.json>
//
// It prints each unit kept as argv, and why, to stderr.
func testJobs(arguments []string) error {
	flags := flag.NewFlagSet("test-jobs", flag.ContinueOnError)
	jobPath := flags.String("job", "", "the planned job")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	content, err := os.ReadFile(*jobPath)
	if err != nil {
		return err
	}
	var job protocol.Job
	if err := protocol.Decode(bytes.NewReader(content), &job); err != nil {
		return fmt.Errorf("%s: %w", *jobPath, err)
	}
	converted := 0
	for index, unit := range job.Units {
		test, why := asTestJob(unit)
		if test == nil {
			fmt.Fprintf(os.Stderr, "%s: kept as argv for a box: %s\n", unit.Id, why)
			continue
		}
		job.Units[index].Argv, job.Units[index].Test = nil, test
		converted++
	}
	fmt.Fprintf(os.Stderr, "%d of %d units are test jobs\n", converted, len(job.Units))
	encoder := json.NewEncoder(os.Stdout)
	return encoder.Encode(job)
}

var gateInputsLine = regexp.MustCompile(`^gateInputs=([0-9a-f]{64})?$`)
var selectionLine = regexp.MustCompile(`^mkdir -p /tmp/loom-select/([0-9a-f]{40}) && echo ([A-Za-z0-9+/=]+) \| base64 -d \| tar -xz -C /tmp/loom-select/([0-9a-f]{40})$`)
var exportLine = regexp.MustCompile(`^export ([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

// switchesOn are the selection's env variables the strict runner always sets to 1.
var switchesOn = map[string]bool{"ADAMIC_GATE_UNCACHED": true, "ADAMIC_TEST_WASI": true, "ADAMIC_ORACLE_WASI": true, "ADAMIC_GATE_COHERE": true}

// asTestJob is the test job a go test unit is, or nil and why it stays argv.
func asTestJob(unit protocol.JobUnit) (*protocol.TestJob, string) {
	if len(unit.Argv) < 6 || unit.Argv[0] != "bash" || unit.Argv[1] != "-c" || unit.Argv[3] != "adamic-gate-unit" {
		return nil, "not a go test unit"
	}
	if len(unit.Environment) > 0 || len(unit.Inputs) > 0 || unit.Directory != "" {
		return nil, "it carries an environment, inputs or a directory"
	}
	for _, output := range unit.Outputs {
		if output.Glob != "loom-out/test.jsonl.gz" && output.Glob != "loom-out/cpu.tsv" {
			return nil, "it declares output " + output.Glob
		}
	}
	head, found := strings.CutSuffix(unit.Argv[2], codexOpening+unitBody)
	if !found {
		return nil, "its body isn't the Codex opening and unitBody"
	}
	test := &protocol.TestJob{Repository: protocol.AdamicRepository, Sha: unit.Argv[4]}
	var changedFile string
	selected := ""
	for _, line := range strings.Split(strings.TrimSuffix(head, "\n"), "\n") {
		if match := gateInputsLine.FindStringSubmatch(line); match != nil {
			test.GateInputs = match[1]
			continue
		}
		if match := selectionLine.FindStringSubmatch(line); match != nil && match[1] == match[3] {
			changed, err := changedPathsOf(match[2])
			if err != nil {
				return nil, "its selection archive: " + err.Error()
			}
			selected, changedFile = match[1], "/tmp/loom-select/"+match[1]+"/changed-paths.txt"
			test.ChangedPaths = changed
			continue
		}
		match := exportLine.FindStringSubmatch(line)
		switch {
		case match == nil:
			return nil, fmt.Sprintf("its opening holds a line a test job can't say: %.80q", line)
		case switchesOn[match[1]] && match[2] == "1":
		case match[1] == "ADAMIC_GATE_SAMPLE" && regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(match[2]):
			test.Sample = match[2]
		case match[1] == "ADAMIC_GATE_CHANGED" && selected != "" && match[2] == changedFile:
		default:
			return nil, fmt.Sprintf("its opening exports %s=%.60s, which a test job doesn't carry", match[1], match[2])
		}
	}
	for _, spec := range unit.Argv[5:] {
		if strings.HasPrefix(spec, "@unplanned=") {
			return nil, "it runs the packages new since the plan (@unplanned), which only a box can list"
		}
		packageName, pattern, found := strings.Cut(spec, "=")
		if !found {
			return nil, "a spec isn't <package>=<pattern>: " + spec
		}
		run, skip, _ := strings.Cut(pattern, " skip=")
		test.Packages = append(test.Packages, protocol.TestPackage{Package: packageName, Run: run, Skip: skip})
	}
	if err := protocol.CheckTestJob(*test); err != nil {
		return nil, "the test job it would be is refused: " + err.Error()
	}
	return test, ""
}

// changedPathsOf reads changed-paths.txt from the selection archive a unit's opening carries.
func changedPathsOf(encoded string) ([]string, error) {
	archive, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	compressed, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	reader := tar.NewReader(compressed)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("no changed-paths.txt")
		}
		if err != nil {
			return nil, err
		}
		if strings.TrimPrefix(header.Name, "./") != "changed-paths.txt" {
			continue
		}
		content, err := io.ReadAll(io.LimitReader(reader, 16<<20))
		if err != nil {
			return nil, err
		}
		var paths []string
		for _, path := range strings.Split(string(content), "\n") {
			if path != "" {
				paths = append(paths, path)
			}
		}
		return paths, nil
	}
}
