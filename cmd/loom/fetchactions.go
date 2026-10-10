package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/system-inc/loom/builder"
)

// fetchActions is a test runner's side of Builder (#egbs6we): each named action's outputs, read from the public store
// and checked hash by hash, go into the runner's ADAMIC_BUILD_CACHE_DIR, where buildcache then finds every product as
// a hit and builds nothing. One line per action; exit 1 when any is missing or doesn't check.
func fetchActions(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("fetch-actions", flag.ContinueOnError)
	flags.SetOutput(stderr)
	cache := flags.String("cache", "", "the runner's ADAMIC_BUILD_CACHE_DIR")
	read := flags.String("read", "https://adamic-store.kirkouimet.com", "the public store, read direct")
	skipNative := flags.Bool("skip-native", false, "leave out products clang built, for the runner to build (Judge's ruling until #tsn1wp8 lands)")
	if err := flags.Parse(arguments); err != nil || *cache == "" || flags.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: loom fetch-actions --cache <dir> [--read <url>] [--skip-native] <productKey>...")
		return 2
	}
	store := builder.Store{Read: strings.TrimSuffix(*read, "/"), SkipNative: *skipNative}
	failed := 0
	for _, key := range flags.Args() {
		started := time.Now()
		if err := store.Fetch(key, *cache); err != nil {
			fmt.Fprintf(stdout, "%s failed %.2f s: %v\n", key, time.Since(started).Seconds(), err)
			failed++
			continue
		}
		fmt.Fprintf(stdout, "%s fetched %.2f s\n", key, time.Since(started).Seconds())
	}
	if failed > 0 {
		return 1
	}
	return 0
}
