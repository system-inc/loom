package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/housecache"
)

// fetchActions is a test runner's side of Builder (#egbs6we): each named action's archive, read from the public store,
// checked against its hash and unpacked, goes into the runner's ADAMIC_BUILD_CACHE_DIR, where buildcache then finds
// every product as a hit and builds nothing. One line per action; exit 1 when any is missing or doesn't check.
func fetchActions(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("fetch-actions", flag.ContinueOnError)
	flags.SetOutput(stderr)
	cache := flags.String("cache", "", "the runner's ADAMIC_BUILD_CACHE_DIR")
	read := flags.String("read", builder.PublicRead, "the public store, read direct")
	skipNative := flags.Bool("skip-native", false, "leave out products clang built, for the runner to build (Judge's ruling until #tsn1wp8 lands)")
	houseCache := flags.String("house-cache", os.Getenv(housecache.Variable), "the house cache, http://<host>:<port>, asked first for every archive (default $"+housecache.Variable+")")
	if err := flags.Parse(arguments); err != nil || *cache == "" || flags.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: loom fetch-actions --cache <dir> [--read <url>] [--skip-native] [--house-cache <url>] <productKey>...")
		return 2
	}
	if *houseCache != "" {
		if err := housecache.CheckURL(*houseCache); err != nil {
			fmt.Fprintf(stderr, "fetch-actions: %v\n", err)
			return 2
		}
	}
	store := builder.Store{Read: strings.TrimSuffix(*read, "/"), SkipNative: *skipNative, House: *houseCache}
	failed := 0
	for _, key := range flags.Args() {
		started := time.Now()
		if err := store.FetchProduct(key, *cache); err != nil {
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
