package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/gateinputs"
	"github.com/system-inc/loom/r2"
)

// gateInputsDefaults are Workshop's: the directory a box's env.sh names, and the file the planner reads.
func gateInputsDefaults() (directory, manifestFile string) {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "adamic-tools", "gate-inputs"), filepath.Join(home, ".loom", "gate-inputs-manifest")
}

// gateInputs publishes the gate inputs (Workshop, after the directory changes) or checks the published ones
// (gateinputs/gateinputs.go):
//
//	loom gate-inputs publish [--dir <dir>] [--manifest-file <path>] [--r2 <key file>] [--bucket <name>] [--read <url>] [--dry-run]
//	loom gate-inputs check [--manifest-file <path>] [--read <url>] [<name>]
//
// publish names the directory by its deterministic tar, writes its chunks and manifest under gate-inputs/ in the
// bucket (nothing when that name is already whole there, and otherwise each only if missing), reads the manifest back
// from the public domain, and only then writes the name to the manifest file, which the planner rereads before every
// pull. It refuses a bucket whose lifecycle would expire
// gate-inputs/, and says so when its key may not read the lifecycle. --dry-run packs and prints the name, and
// writes nothing anywhere. check reads a manifest back as a runner would and exits 1 when it isn't whole.
func gateInputs(arguments []string, stdout io.Writer, stderr io.Writer) int {
	usage := "usage: loom gate-inputs publish [--dir <dir>] [--manifest-file <path>] [--r2 <key file>] [--bucket <name>] [--read <url>] [--dry-run]\n" +
		"       loom gate-inputs check [--manifest-file <path>] [--read <url>] [<name>]"
	if len(arguments) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	defaultDirectory, defaultManifestFile := gateInputsDefaults()
	flags := flag.NewFlagSet("gate-inputs "+arguments[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestFile := flags.String("manifest-file", defaultManifestFile, "the file holding the published manifest's sha256, which the planner reads")
	switch arguments[0] {
	case "publish":
		directory := flags.String("dir", defaultDirectory, "the gate inputs' directory, as a box's env.sh names it")
		dryRun := flags.Bool("dry-run", false, "pack and print the manifest's sha256; write nothing")
		storeFlags := addStoreFlags(flags)
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		if *dryRun {
			bytes := int64(0)
			manifest, err := gateinputs.Pack(*directory, 0, func(_ string, content []byte) error {
				bytes += int64(len(content))
				return nil
			})
			if err != nil {
				fmt.Fprintln(stderr, "gate-inputs publish:", err)
				return 1
			}
			fmt.Fprintf(stdout, "%s: %d chunks, %d bytes, %d unpacked (dry run: nothing written)\n", manifest.Name, len(manifest.Chunks), bytes, manifest.Size)
			return 0
		}
		store, err := storeFlags.open(&builder.Requests{})
		if err != nil {
			fmt.Fprintln(stderr, "gate-inputs publish:", err)
			return 1
		}
		return publishGateInputs(*directory, *manifestFile, *store.Bucket, store.Read, stdout, stderr)
	case "check":
		read := flags.String("read", builder.PublicRead, "the public store, read direct")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() > 1 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		hash := flags.Arg(0)
		if hash == "" {
			var err error
			if hash, err = gateinputs.ReadFile(*manifestFile); err != nil {
				fmt.Fprintln(stderr, "gate-inputs check:", err)
				return 1
			}
		}
		manifest, err := gateinputs.Check(strings.TrimSuffix(*read, "/"), nil, hash)
		if err != nil {
			fmt.Fprintln(stderr, "gate-inputs check:", err)
			return 1
		}
		fmt.Fprintf(stdout, "%s: %d chunks, total %s, every chunk held\n", hash, len(manifest.Chunks), manifest.Total)
		return 0
	}
	fmt.Fprintln(stderr, usage)
	return 2
}

// publishGateInputs is publish past its flags: the lifecycle checked, the directory published, the file written last.
func publishGateInputs(directory, manifestFile string, bucket r2.Bucket, read string, stdout, stderr io.Writer) int {
	rules, err := bucket.Lifecycle()
	switch {
	case errors.Is(err, r2.ErrLifecycleUnreadable):
		fmt.Fprintf(stderr, "gate-inputs publish: this key may not read the bucket's lifecycle, so that %s never expires is unchecked\n", gateinputs.Prefix)
	case err != nil:
		fmt.Fprintln(stderr, "gate-inputs publish:", err)
		return 1
	default:
		if rule := gateinputs.HomeExpires(rules); rule != nil {
			fmt.Fprintf(stderr, "gate-inputs publish: the bucket's lifecycle rule %q expires keys under %q, which reaches %s: nothing published\n", rule.Id, rule.Prefix, gateinputs.Prefix)
			return 1
		}
	}
	published, err := gateinputs.Publish(directory, 0, bucket, read, nil)
	if err != nil {
		fmt.Fprintln(stderr, "gate-inputs publish:", err)
		return 1
	}
	previous, _ := gateinputs.ReadFile(manifestFile)
	if err = gateinputs.WriteFile(manifestFile, published.Name); err != nil {
		fmt.Fprintln(stderr, "gate-inputs publish:", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s: %d chunks, %d bytes, %d objects written; %s names it\n", published.Name, len(published.Manifest.Chunks), published.Manifest.Compressed, published.Uploaded, manifestFile)
	if previous != "" && previous != published.Name {
		fmt.Fprintf(stdout, "the gate inputs moved from %.12s: every unit key the planner makes from its next pull moves with them\n", previous)
	}
	return 0
}
