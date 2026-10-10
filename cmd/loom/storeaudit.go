package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/system-inc/loom/builder"
)

// storeAudit is Workshop's daily check of its index against the action store's own listing (#k62gwdt). It prints the
// report as one JSON line and exits 1 on drift, a ref or blob the index claims that the store doesn't hold, so the
// job that runs it pages.
func storeAudit(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("store-audit", flag.ContinueOnError)
	flags.SetOutput(stderr)
	home, _ := os.UserHomeDir()
	write := flags.String("write", "", "loom-pipeline's action store, https://<pipeline>/actions")
	tokenFile := flags.String("token-file", filepath.Join(home, ".loom", "build-token"), "file holding this builder's build token")
	indexDirectory := flags.String("index", filepath.Join(home, "loom-builder", "index"), "Workshop's index")
	if err := flags.Parse(arguments); err != nil || *write == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: loom store-audit --write <https://pipeline/actions> [--token-file <path>] [--index <dir>]")
		return 2
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(stderr, "store-audit:", err)
		return 1
	}
	index, err := builder.OpenIndex(*indexDirectory)
	if err != nil {
		fmt.Fprintln(stderr, "store-audit:", err)
		return 1
	}
	defer index.Close()
	requests := &builder.Requests{}
	report, err := builder.Audit(builder.Store{Write: strings.TrimSuffix(*write, "/"), Token: strings.TrimSpace(string(token)), Requests: requests}, index)
	if err != nil {
		fmt.Fprintln(stderr, "store-audit:", err)
		return 1
	}
	json.NewEncoder(stdout).Encode(report)
	fmt.Fprintf(stderr, "store-audit: %d listing reads\n", requests.Reads.Load())
	if report.Drift() {
		fmt.Fprintf(stderr, "store-audit: drift: the index claims %d refs and %d blobs the store doesn't hold\n", len(report.MissingRefs), len(report.MissingBlobs))
		return 1
	}
	return 0
}
