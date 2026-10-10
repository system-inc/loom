package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/release"
)

// releaseCommand is the release verbs (docs/releases.md):
//
//	loom release watch                     Workshop's watcher and the boxes' report receiver (loom-release.service)
//	loom release status                    every box's version, last update, hold and services, and the watcher's state
//	loom release install                   the watcher's unit, hook and probe, where ~/.loom/release.conf exists
//	loom release mark <commit> <step>      a step the release's order names is done
//	loom release rollback                  ends a canary: every box follows the fleet's release again
//	loom release resume [--retry]          a stopped watcher releases again
//
// Each reads ~/.loom/release.conf (--config) over Workshop's defaults.
func releaseCommand(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) == 0 {
		fmt.Fprint(stderr, usage)
		return 3
	}
	verb := arguments[0]
	flags := flag.NewFlagSet("release "+verb, flag.ContinueOnError)
	flags.SetOutput(stderr)
	home, _ := os.UserHomeDir()
	configPath := flags.String("config", filepath.Join(home, ".loom", "release.conf"), "the release settings")
	wire := flags.String("wire", "https://runs.loom.system.inc", "the wire's origin, for when each box's serve last asked")
	retry := flags.Bool("retry", false, "resume: release again the commit that stopped the watcher")
	if err := flags.Parse(arguments[1:]); err != nil {
		return 3
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "loom release %s: %v\n", verb, err)
		return 1
	}
	if verb == "install" && flags.NArg() == 0 {
		paths := release.HomeInstallPaths(home)
		paths.Config = *configPath
		if err := release.Install(paths, systemctlUser, stdout); err != nil {
			return fail(err)
		}
		return 0
	}
	config, found, err := release.ReadHomeConfig(home, *configPath)
	if err != nil {
		return fail(err)
	}
	if !found {
		if verb == "watch" {
			return fail(fmt.Errorf("no %s: this machine doesn't release", *configPath))
		}
		config = release.DefaultConfig(home)
	}
	steps := release.Commands(config, stderr)
	switch {
	case verb == "watch" && flags.NArg() == 0:
		return releaseWatch(config, steps, *wire, home, stderr)
	case verb == "status" && flags.NArg() == 0:
		return releaseStatus(config, *wire, home, stdout, stderr)
	case verb == "mark" && flags.NArg() == 2:
		if err := release.Mark(config.State, flags.Arg(0), flags.Arg(1)); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "loom release: %s marked done for %s\n", flags.Arg(1), flags.Arg(0))
		return 0
	case verb == "rollback" && flags.NArg() == 0:
		if err := release.Rollback(context.Background(), config, steps, stdout); err != nil {
			return fail(err)
		}
		return 0
	case verb == "resume" && flags.NArg() == 0:
		if err := release.Resume(config, *retry, stdout); err != nil {
			return fail(err)
		}
		return 0
	}
	fmt.Fprint(stderr, usage)
	return 3
}

func systemctlUser(arguments ...string) (string, error) {
	output, err := exec.Command("systemctl", append([]string{"--user"}, arguments...)...).CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("systemctl --user %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

// poolAsked is when each host's serve last asked any of the pools, by lowercase host: a worker is named
// <host>-<six digits of its machine id> (serving.WorkerName).
func poolAsked(callContext context.Context, wire string, secret []byte, pools []string) (map[string]time.Time, error) {
	asked := map[string]time.Time{}
	for _, pool := range pools {
		if callContext.Err() != nil {
			return nil, callContext.Err()
		}
		status, err := readPoolStatus(&http.Client{Timeout: 15 * time.Second}, wire, secret, pool)
		if err != nil {
			return nil, err
		}
		for _, worker := range status.Workers {
			host := worker.Worker
			if cut := strings.LastIndexByte(host, '-'); cut > 0 {
				host = host[:cut]
			}
			at, err := time.Parse(time.RFC3339, worker.SeenAt)
			if key := strings.ToLower(host); err == nil && at.After(asked[key]) {
				asked[key] = at
			}
		}
	}
	return asked, nil
}

func releaseWatch(config release.Config, steps release.Steps, wire, home string, stderr io.Writer) int {
	if secret, err := protocol.ReadTokenSecret(filepath.Join(home, ".loom", "token-secret")); err == nil {
		steps.PoolSeen = func(callContext context.Context, host string) (time.Time, error) {
			asked, err := poolAsked(callContext, wire, secret, config.Pools)
			return asked[strings.ToLower(host)], err
		}
	} else {
		fmt.Fprintf(stderr, "loom release: no token secret (%v): the canary's serve is judged by its own report, not by the pools\n", err)
	}
	server := &http.Server{Addr: config.Listen, ReadHeaderTimeout: 10 * time.Second,
		Handler: release.Receiver{Directory: filepath.Join(config.State, "reports"), Hosts: config.Boxes, Now: time.Now}}
	served := make(chan error, 1)
	go func() { served <- server.ListenAndServe() }()
	callContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	watcher := &release.Watcher{Config: config, Steps: steps, Now: time.Now, Log: stderr}
	fmt.Fprintf(stderr, "loom release: watching %s %s/%s, canary %s, boxes %s; reports on %s\n", config.Repository, config.Remote, config.Branch,
		config.Canary, strings.Join(config.Boxes, " "), config.Listen)
	go watcher.Watch(callContext)
	select {
	case err := <-served:
		fmt.Fprintf(stderr, "loom release: the report receiver: %v\n", err)
		return 1
	case <-callContext.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
		return 0
	}
}

func releaseStatus(config release.Config, wire, home string, stdout, stderr io.Writer) int {
	callContext, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	published, err := fetchManifest(callContext, config.Base)
	if err != nil {
		fmt.Fprintf(stderr, "loom release status: %v\n", err)
		return 3
	}
	state, err := release.ReadState(config.State)
	if err != nil {
		fmt.Fprintf(stderr, "loom release status: %v\n", err)
		return 3
	}
	var asked map[string]time.Time
	if secret, err := protocol.ReadTokenSecret(filepath.Join(home, ".loom", "token-secret")); err != nil {
		fmt.Fprintf(stderr, "loom release status: no token secret, so the pools are unread: %v\n", err)
	} else if asked, err = poolAsked(callContext, wire, secret, config.Pools); err != nil {
		fmt.Fprintf(stderr, "loom release status: the pools are unread: %v\n", err)
	}
	boxes, err := release.Boxes(config, published, asked, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "loom release status: %v\n", err)
		return 3
	}
	if release.WriteStatus(stdout, published, state, boxes, time.Now()) {
		return 1
	}
	return 0
}

// fetchManifest is <base>/current.txt, what every box reads.
func fetchManifest(callContext context.Context, base string) (release.Manifest, error) {
	request, err := http.NewRequestWithContext(callContext, http.MethodGet, strings.TrimSuffix(base, "/")+"/current.txt", nil)
	if err != nil {
		return release.Manifest{}, err
	}
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return release.Manifest{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err == nil && response.StatusCode != http.StatusOK {
		err = errors.New(response.Status)
	}
	if err != nil {
		return release.Manifest{}, fmt.Errorf("%s/current.txt: %w", base, err)
	}
	manifest, err := release.ParseManifest(string(body))
	if err != nil {
		return release.Manifest{}, fmt.Errorf("%s/current.txt: %w", base, err)
	}
	return manifest, nil
}
