package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/housecache"
	"github.com/system-inc/loom/serving"
)

const houseCacheUsage = `usage:
  loom house-cache serve --listen <ip>:<port> --directory <dir> [--limit-gb N] [--floor-gb N] [--upstream <url>] [--tailnet] [--public]
  loom house-cache install
`

// houseCache is `loom house-cache` (docs/house-cache.md): serve runs the house cache, and install readies its systemd
// user unit on the box whose ~/.loom/house-cache.conf names it host (the updater's hook runs it after every release).
func houseCache(arguments []string, stdout, stderr io.Writer) int {
	if len(arguments) == 0 {
		fmt.Fprint(stderr, houseCacheUsage)
		return 2
	}
	switch arguments[0] {
	case "serve":
		return houseCacheServe(arguments[1:], stderr)
	case "install":
		return houseCacheInstall(arguments[1:], stdout, stderr)
	}
	fmt.Fprint(stderr, houseCacheUsage)
	return 2
}

// houseCacheServe serves the house cache until SIGTERM or SIGINT, saying each miss, refusal and eviction on stderr.
func houseCacheServe(arguments []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("house-cache serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	listen := flags.String("listen", "", "the one address to listen on, <ip>:<port>, on the house's network")
	directory := flags.String("directory", "", "where the blobs are kept")
	limitGB := flags.Uint64("limit-gb", serving.DefaultHouseCacheLimitGB, "the most gigabytes of blobs kept")
	floorGB := flags.Uint64("floor-gb", serving.DefaultHouseCacheFloorGB, "the gigabytes kept free on the directory's disk")
	upstream := flags.String("upstream", housecache.DefaultUpstream, "the store the cache fills from")
	tailnet := flags.Bool("tailnet", false, "allow a tailnet's address, in 100.64.0.0/10")
	public := flags.Bool("public", false, "allow an address that isn't on a local network")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 || *listen == "" || *directory == "" || *limitGB == 0 {
		fmt.Fprint(stderr, houseCacheUsage)
		return 2
	}
	if err := housecache.CheckListen(*listen, *public, *tailnet); err != nil {
		fmt.Fprintf(stderr, "loom house-cache: %v\n", err)
		return 2
	}
	limit, err := builder.Gigabytes(*limitGB)
	if err != nil {
		fmt.Fprintf(stderr, "loom house-cache: %v\n", err)
		return 2
	}
	floor, err := builder.Gigabytes(*floorGB)
	if err != nil {
		fmt.Fprintf(stderr, "loom house-cache: %v\n", err)
		return 2
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = time.Minute
	server := &housecache.Server{Directory: *directory, Upstream: *upstream, Limit: int64(min(limit, 1<<62)), Floor: floor, Free: builder.Free,
		Client: &http.Client{Transport: transport}, Report: stderr}
	if err := server.Open(); err != nil {
		fmt.Fprintf(stderr, "loom house-cache: %v\n", err)
		return 2
	}
	defer server.Close()
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(stderr, "loom house-cache: %v\n", err)
		return 2
	}
	httpServer := &http.Server{Handler: server, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	serveContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-serveContext.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpServer.Shutdown(shutdownContext)
	}()
	fmt.Fprintf(stderr, "loom house-cache: serving %s on %s, at most %d GB, %d GB kept free, filled from %s\n", *directory, *listen, *limitGB, *floorGB, *upstream)
	if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "loom house-cache: %v\n", err)
		return 1
	}
	return 0
}

// houseCacheInstall is `loom house-cache install`: loom-house-cache.service rendered from this box's
// house-cache.conf, installed as a systemd user unit and started or restarted; or, with no house-cache.conf, stopped
// and removed. Linux only, since only Linux boxes run it through systemd.
func houseCacheInstall(arguments []string, stdout, stderr io.Writer) int {
	if len(arguments) != 0 {
		fmt.Fprint(stderr, houseCacheUsage)
		return 2
	}
	if runtime.GOOS != "linux" {
		fmt.Fprintf(stderr, "loom house-cache: install readies a systemd user unit, and this is %s\n", runtime.GOOS)
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "loom house-cache: %v\n", err)
		return 2
	}
	if err := serving.InstallHouseCache(serving.HouseCacheHomePaths(home), serving.UserSystemctl(stderr), stdout); err != nil {
		fmt.Fprintf(stderr, "loom house-cache: %v\n", err)
		return 2
	}
	return 0
}
