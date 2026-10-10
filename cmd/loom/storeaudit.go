package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/system-inc/loom/builder"
)

// storeAudit is Workshop's daily check of the action store against itself (#k62gwdt), listed through R2's S3
// interface with the builder's key. It prints the report as one JSON line and exits 1 on drift, a fresh ref naming a
// blob the store lacks or one that expires before it, so the job that runs it pages.
func storeAudit(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("store-audit", flag.ContinueOnError)
	flags.SetOutput(stderr)
	storeFlags := addStoreFlags(flags)
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: loom store-audit [--r2 <key file>] [--bucket <name>]")
		return 2
	}
	requests := &builder.Requests{}
	store, err := storeFlags.open(requests)
	if err != nil {
		fmt.Fprintln(stderr, "store-audit:", err)
		return 1
	}
	report, err := builder.Audit(store)
	if err != nil {
		fmt.Fprintln(stderr, "store-audit:", err)
		return 1
	}
	json.NewEncoder(stdout).Encode(report)
	fmt.Fprintf(stderr, "store-audit: %d reads\n", requests.Reads.Load())
	if report.Drift() {
		fmt.Fprintf(stderr, "store-audit: drift: %d fresh refs name a blob the store lacks, %d one that expires before them\n", len(report.Dangling), len(report.Stale))
		return 1
	}
	return 0
}
