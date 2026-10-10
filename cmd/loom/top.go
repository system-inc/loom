package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

// top is `loom top`: what Loom is doing on this box and across the house, redrawn every second until q, btop's way.
// It reads, and writes nothing but the terminal: serve's, its runner's and the tree builder's live status files
// (package livestatus), the machine's CPUs, memory and disks against Loom's floors, serve's kept blobs and sources, the
// systemd user units Loom runs, and, with a token this box already holds, the pools' status and Queue's line from the
// Workers: a board token minted from ~/.loom/token-secret where there is one (Workshop, the Mac), else serve's own pool
// token, which reads only its pool. No token is ever printed. --board draws the board of gate lines, as before.
func top(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if slices.Contains(arguments, "--board") {
		return topBoard(slices.DeleteFunc(slices.Clone(arguments), func(argument string) bool { return argument == "--board" }), stdout, stderr)
	}
	home, _ := os.UserHomeDir()
	flags := flag.NewFlagSet("top", flag.ContinueOnError)
	flags.SetOutput(stderr)
	once := flags.Bool("once", false, "draw one frame, plain, and exit")
	width := flags.Int("width", 0, "the frame's width with --once (default the terminal's, or 80)")
	height := flags.Int("height", 0, "the frame's height with --once (default the terminal's, or 24)")
	serveRoot := flags.String("serve-root", filepath.Join(home, "loom-serve", "root"), "serve's root, where its live status is")
	trees := flags.String("trees", filepath.Join(home, "loom-builder", "trees"), "the tree builder's cache, where its live status is")
	ledger := flags.String("ledger", filepath.Join(home, "loom-trees", "trees.jsonl"), "the tree builder's ledger")
	wire := flags.String("wire", "https://runs.loom.system.inc", "the wire's origin, where the pools are")
	queue := flags.String("queue", "https://loom.system.inc", "loom's origin, where Queue is")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		fmt.Fprint(stderr, usage)
		return 3
	}
	reader := newTopReader(home, *serveRoot, *trees, *ledger, *wire, *queue)
	runContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	terminal, isTerminal := openTerminal(stdout)
	if *once || !isTerminal {
		// One frame: the CPUs' load needs two readings, a quarter second apart.
		reader.readMachine()
		reader.slow(runContext, true, true, true)
		time.Sleep(250 * time.Millisecond)
		columns, rows := 80, 24
		if isTerminal {
			columns, rows = terminal.size()
		}
		if *width > 0 {
			columns = *width
		}
		if *height > 0 {
			rows = *height
		}
		fmt.Fprintln(stdout, drawTop(reader.fast(time.Now()), columns, rows, false))
		return 0
	}
	go reader.background(runContext)
	keys := terminal.start()
	defer terminal.stop()
	resized := make(chan os.Signal, 1)
	signal.Notify(resized, syscall.SIGWINCH)
	defer signal.Stop(resized)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		columns, rows := terminal.size()
		frame := drawTop(reader.fast(time.Now()), columns, rows, true)
		// Home, then each row over the last frame's, each cleared to its end, and everything below cleared.
		fmt.Fprint(stdout, "\033[H"+lineEnds.Replace(frame)+"\033[K\033[J")
		select {
		case <-runContext.Done():
			return 0
		case key, open := <-keys:
			if !open || key == 'q' || key == 'Q' || key == 3 {
				return 0
			}
		case <-resized:
		case <-ticker.C:
		}
	}
}
