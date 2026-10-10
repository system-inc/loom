package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/queuebridge"
)

// queueBridge is `loom queue-bridge`, the bridge's one pass (package queuebridge), which loom-queue-bridge.timer (or the
// launchd agent on macOS) runs every 60 s on the machine whose ~/.loom/queue-bridge.conf makes it the bridge: git's
// facts for every unchecked change, then today's gate's verdicts while it decides. What it did is kept in
// <state>/memory.json between passes. It exits 0 after a pass (or when another pass holds the lock), 2 on a bad command
// line and 3 when its settings or its memory can't be read. `loom queue-bridge install` is the updater hook's: it
// installs the release's units and never starts them.
func queueBridge(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) > 0 && arguments[0] == "install" {
		return queueBridgeInstall(arguments[1:], stdout, stderr)
	}
	home, _ := os.UserHomeDir()
	flags := flag.NewFlagSet("queue-bridge", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", queuebridge.HomePaths(home).Config, "the bridge's settings: queue, repository, state, secret, push-main, requeue, decides")
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
	// One pass at a time: a pass still running when the timer fires again is left to finish, and this one is a no-op.
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return 0
	}
	secret, err := protocol.ReadTokenSecret(config.Secret)
	if err != nil {
		fmt.Fprintf(stderr, "loom queue-bridge: %v\n", err)
		return 3
	}
	// The memory keeps a verdict from being posted twice, so one that can't be read stops the pass rather than starting
	// it empty.
	memoryPath := filepath.Join(config.State, "memory.json")
	memory := queuebridge.Memory{}
	if held, err := os.ReadFile(memoryPath); err == nil {
		if err := json.Unmarshal(held, &memory); err != nil {
			fmt.Fprintf(stderr, "loom queue-bridge: %s: %v\n", memoryPath, err)
			return 3
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stderr, "loom queue-bridge: %v\n", err)
		return 3
	}
	log := func(text string) {
		fmt.Fprintf(stdout, "%s queue-bridge: %s\n", time.Now().UTC().Format("2006-01-02T15:04:05Z"), text)
	}
	bridge := queuebridge.Bridge{Queue: queuebridge.HTTPQueue{Base: config.Queue, Secret: secret},
		Gate: queuebridge.Clone{Repository: config.Repository, PushMain: config.PushMain, RequeueSh: config.Requeue}, Decides: config.Decides, Log: log}
	bridge.Tick(&memory)
	// Lists, never null, as memory.json has always held them.
	for _, list := range []*[]string{&memory.Queued, &memory.Posted, &memory.Requeued, &memory.Ruled} {
		if *list == nil {
			*list = []string{}
		}
	}
	encoded, _ := json.MarshalIndent(memory, "", " ")
	if err := os.WriteFile(memoryPath+".new", encoded, 0o644); err != nil {
		fmt.Fprintf(stderr, "loom queue-bridge: %v\n", err)
		return 3
	}
	if err := os.Rename(memoryPath+".new", memoryPath); err != nil {
		fmt.Fprintf(stderr, "loom queue-bridge: %v\n", err)
		return 3
	}
	return 0
}

// queueBridgeInstall is `loom queue-bridge install`, which the updater's hook 61-queue-bridge runs after every release:
// the release's units written where systemd or launchd reads them, never enabled, loaded or started (queuebridge.Install).
func queueBridgeInstall(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) != 0 {
		fmt.Fprintln(stderr, "usage: loom queue-bridge install")
		return 2
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
	if err := queuebridge.Install(queuebridge.HomePaths(home), home, runtime.GOOS, systemctl, stdout); err != nil {
		fmt.Fprintf(stderr, "loom queue-bridge install: %v\n", err)
		return 1
	}
	return 0
}
