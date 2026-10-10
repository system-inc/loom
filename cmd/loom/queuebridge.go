package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/system-inc/loom/queuebridge"
)

// queueBridge is `loom queue-bridge`, the bridge's one pass (package queuebridge), which loom-queue-bridge.timer runs
// every 60 s on the machine whose ~/.loom/queue-bridge.conf makes it the bridge: git's facts for every unchecked change.
// It exits 0 after a pass (or when another pass holds the lock), 1 when it couldn't read the queue, 2 on a bad command
// line and 3 when its settings can't be read. `loom queue-bridge install` is the updater hook's: it installs the
// release's units and never starts them. `loom queue-bridge pins` prints a commit's pins as the bridge posts them.
func queueBridge(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) > 0 && arguments[0] == "install" {
		return queueBridgeInstall(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "pins" {
		return queueBridgePins(arguments[1:], stdout, stderr)
	}
	home, _ := os.UserHomeDir()
	flags := flag.NewFlagSet("queue-bridge", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", queuebridge.HomePaths(home).Config, "the bridge's settings: queue, repository, state, token")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: loom queue-bridge [--config <queue-bridge.conf>]")
		return 2
	}
	content, err := os.ReadFile(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "loom queue-bridge: this machine's queue-bridge settings, whose being there makes it the bridge: %v\n", err)
		return 3
	}
	config, err := queuebridge.ReadConfig(string(content), home)
	if err != nil {
		fmt.Fprintf(stderr, "loom queue-bridge: %s: %v\n", *configPath, err)
		return 3
	}
	if err := os.MkdirAll(config.State, 0o755); err != nil {
		fmt.Fprintf(stderr, "loom queue-bridge: %v\n", err)
		return 3
	}
	lock, err := os.OpenFile(filepath.Join(config.State, "lock"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(stderr, "loom queue-bridge: %v\n", err)
		return 3
	}
	defer lock.Close()
	// One pass at a time: a pass still reading git when the timer fires again is left to finish, and this one is a
	// no-op, so the bridge's readings never overlap (docs/queue.md).
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return 0
	}
	// Its own coordinator token, minted for it: the bridge never reads the wire's secret.
	held, err := os.ReadFile(config.Token)
	token := strings.TrimSpace(string(held))
	if err != nil || token == "" {
		fmt.Fprintf(stderr, "loom queue-bridge: the bridge's token (loom coordinator-token queue-bridge --days N > %s): %v\n", config.Token, err)
		return 3
	}
	log := func(text string) {
		fmt.Fprintf(stdout, "%s queue-bridge: %s\n", time.Now().UTC().Format("2006-01-02T15:04:05Z"), text)
	}
	bridge := queuebridge.Bridge{Queue: queuebridge.HTTPQueue{Base: config.Queue, Token: token}, Gate: queuebridge.Clone{Repository: config.Repository}, Log: log}
	if !bridge.Tick() {
		return 1
	}
	return 0
}

// queueBridgePins is `loom queue-bridge pins [--repository <clone>] <sha>`: the pins fact for a commit the clone holds,
// every gitlink at any depth proven by a keyless fetch, printed as the JSON the bridge posts. It exits 0 when every pin
// is fetchable, 1 when one isn't (the queue would refuse the change), 2 on a bad command line and 3 when git or a
// remote couldn't say (the bridge would wait).
func queueBridgePins(arguments []string, stdout io.Writer, stderr io.Writer) int {
	home, _ := os.UserHomeDir()
	flags := flag.NewFlagSet("queue-bridge pins", flag.ContinueOnError)
	flags.SetOutput(stderr)
	repository := flags.String("repository", queuebridge.DefaultConfig(home).Repository, "the adamic clone that holds the commit")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: loom queue-bridge pins [--repository <clone>] <sha>")
		return 2
	}
	pins, err := queuebridge.Clone{Repository: *repository}.PinsOf(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "loom queue-bridge pins: git can't say, so the facts would wait: %v\n", err)
		return 3
	}
	encoded, _ := json.MarshalIndent(pins, "", "  ")
	fmt.Fprintln(stdout, string(encoded))
	for _, pin := range pins {
		if !pin.Fetchable {
			return 1
		}
	}
	return 0
}

// queueBridgeInstall is `loom queue-bridge install`, which the updater's hook 61-queue-bridge runs after every release:
// the release's loom-queue-bridge units written where systemd reads them, never enabled or started (queuebridge.Install).
func queueBridgeInstall(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) != 0 {
		fmt.Fprintln(stderr, "usage: loom queue-bridge install")
		return 2
	}
	if runtime.GOOS != "linux" {
		fmt.Fprintf(stderr, "loom queue-bridge install: the bridge is a systemd user unit, and this is %s\n", runtime.GOOS)
		return 3
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "loom queue-bridge install: %v\n", err)
		return 3
	}
	systemctl := func(arguments ...string) (string, error) {
		output, err := exec.Command("systemctl", append([]string{"--user"}, arguments...)...).CombinedOutput()
		if err != nil {
			return string(output), fmt.Errorf("systemctl --user %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
		}
		return string(output), nil
	}
	if err := queuebridge.Install(queuebridge.HomePaths(home), home, time.Now(), systemctl, stdout); err != nil {
		fmt.Fprintf(stderr, "loom queue-bridge install: %v\n", err)
		return 1
	}
	return 0
}
