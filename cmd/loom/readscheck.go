package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/system-inc/loom/planner"
)

// readsCheck is the read lock (#ed4p461): a test unit's traced reads against its declared inputs. It prints each
// finding as a JSON line and exits 1 when there is one, so an undeclared read reds the unit and reaches its owner.
//
// Given the unit's key parts as the plan posted them (--key-parts), it is also how Workshop measures and refreshes a
// unit's read set (#xryaqdv): the trace, recorded with planner.TraceCalls, is checked against the read set the key
// held, and what the run reached inside the submodules is recorded under the unit's code key (--read-sets), so the
// next plan keys the unit on the submodule files it reads instead of the submodules' commits. A run that reached a
// path beyond its key's read set is Loom's void: named on stderr, its findings marked beyondKey, its key listed in the
// no-reuse file so no plan reuses its verdict, its clean record taken back, and its set grown by what it read, which
// moves the unit's next key. A run of a key on a read set with no finding is recorded clean, which is what lets the
// planner reuse a verdict under that key (planner.UncheckedReadSets).
func readsCheck(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("reads-check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	tree := flags.String("tree", "", "the checkout the unit ran in")
	gateTools := flags.String("gate-tools", "", "the gate tools checkout, for executors.txt's reads lines")
	importPath := flags.String("package", "", "the unit's package import path")
	tracePath := flags.String("trace", "", "the unit's strace-format trace (strace -f --decode-fds=path -e trace="+planner.TraceCalls+")")
	unitKey := flags.String("unit-key", "", "the unit's key, carried on each finding")
	keyPartsPath := flags.String("key-parts", "", "the unit's key parts as the plan posted them (a keyParts object, or a planned unit holding one): checks the trace against its read set and records what it read")
	readSets := flags.String("read-sets", planner.DefaultReadSetsDirectory, "with --key-parts, the read sets it records into and checks against, and where a clean trace of a key on one is recorded")
	noReuse := flags.String("no-reuse", planner.NoReuseFile, "with --key-parts, the no-reuse file a key whose run read beyond its read set is listed in")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *tree == "" || *gateTools == "" || *importPath == "" || *tracePath == "" {
		fmt.Fprintln(stderr, "usage: loom reads-check --tree <dir> --gate-tools <dir> --package <import path> --trace <file> [--unit-key <key>] [--key-parts <file> [--read-sets <dir>] [--no-reuse <file>]]")
		return 2
	}
	trace, err := os.Open(*tracePath)
	if err != nil {
		fmt.Fprintln(stderr, "reads-check:", err)
		return 1
	}
	defer trace.Close()
	if *keyPartsPath != "" {
		return readsCheckKeyed(*tree, *gateTools, *importPath, *unitKey, *keyPartsPath, *readSets, *noReuse, trace, stdout, stderr)
	}
	findings, err := planner.CheckUnitReads(*tree, *gateTools, *importPath, trace)
	if err != nil {
		fmt.Fprintln(stderr, "reads-check:", err)
		return 1
	}
	if code := printFindings(findings, *unitKey, stdout, stderr); code != 0 {
		return code
	}
	if len(findings) > 0 {
		fmt.Fprintf(stderr, "reads-check: %s read %d tracked files outside its declared inputs\n", *importPath, len(findings))
		return 1
	}
	return 0
}

// readsCheckKeyed checks a traced run against the key the plan gave its unit and records the read set it measured.
func readsCheckKeyed(tree, gateTools, importPath, unitKey, keyPartsPath, readSets, noReuse string, trace io.Reader, stdout, stderr io.Writer) int {
	content, err := os.ReadFile(keyPartsPath)
	if err != nil {
		fmt.Fprintln(stderr, "reads-check:", err)
		return 1
	}
	var planned struct {
		KeyParts *planner.KeyParts `json:"keyParts"`
	}
	var parts planner.KeyParts
	if err := json.Unmarshal(content, &planned); err == nil && planned.KeyParts != nil {
		parts = *planned.KeyParts
	} else if err := json.Unmarshal(content, &parts); err != nil {
		fmt.Fprintln(stderr, "reads-check: --key-parts:", err)
		return 1
	}
	if parts.Package != importPath {
		fmt.Fprintf(stderr, "reads-check: the key parts are %s's, not %s's\n", parts.Package, importPath)
		return 1
	}
	key, err := planner.UnitKey(parts)
	if err != nil {
		fmt.Fprintln(stderr, "reads-check:", err)
		return 1
	}
	if unitKey != "" && unitKey != key {
		fmt.Fprintf(stderr, "reads-check: the key parts key %s, not --unit-key %s\n", key, unitKey)
		return 1
	}
	planner.ReadSetsDirectory = readSets
	check, err := planner.CheckTrace(tree, gateTools, parts, trace)
	if err != nil {
		fmt.Fprintln(stderr, "reads-check:", err)
		return 1
	}
	recorded, err := planner.RecordReadSet(readSets, check.CodeKey, parts, check.Measured)
	if err != nil {
		fmt.Fprintln(stderr, "reads-check: recording the read set:", err)
		return 1
	}
	if code := printFindings(check.Findings, key, stdout, stderr); code != 0 {
		return code
	}
	beyond := check.Beyond()
	if len(beyond) > 0 {
		paths := []string{}
		for _, finding := range beyond {
			paths = append(paths, finding.Path)
		}
		named := strings.Join(paths, ", ")
		if len(paths) > 5 {
			named = strings.Join(paths[:5], ", ") + fmt.Sprintf(" and %d more", len(paths)-5)
		}
		why := fmt.Sprintf("Loom's void: its traced run read %d paths beyond its key's read set %.12s (%s)", len(paths), parts.ReadSet, named)
		if err := appendNoReuse(noReuse, key, why); err != nil {
			fmt.Fprintln(stderr, "reads-check: listing the key as no-reuse:", err)
			return 1
		}
		if err := planner.ForgetTraceClean(readSets, key); err != nil {
			fmt.Fprintln(stderr, "reads-check: taking back the key's clean trace:", err)
			return 1
		}
		fmt.Fprintf(stderr, "reads-check: %s, unit %s: %s; its read set is now %.12s, and %s lists its key\n", importPath, key, why, recorded, noReuse)
	}
	if owners := len(check.Findings) - len(beyond); owners > 0 {
		fmt.Fprintf(stderr, "reads-check: %s read %d tracked files outside its declared inputs\n", importPath, owners)
	}
	if len(check.Findings) > 0 {
		return 1
	}
	if parts.ReadSet != "" {
		// A clean trace of a key on a read set is what lets the planner reuse a verdict under it.
		if err := planner.RecordTraceClean(readSets, key); err != nil {
			fmt.Fprintln(stderr, "reads-check: recording the clean trace:", err)
			return 1
		}
	}
	return 0
}

func printFindings(findings []planner.Finding, unitKey string, stdout, stderr io.Writer) int {
	encoder := json.NewEncoder(stdout)
	for _, finding := range findings {
		finding.UnitKey = unitKey
		if err := encoder.Encode(finding); err != nil {
			fmt.Fprintln(stderr, "reads-check:", err)
			return 1
		}
	}
	return 0
}

// appendNoReuse lists a key as no-reuse with why, one line appended (planner.LoadNoReuse's form).
func appendNoReuse(path, key, why string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(file, "%s %s\n", key, strings.ReplaceAll(why, "\n", " ")); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
