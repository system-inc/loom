package main

import (
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

	"github.com/system-inc/loom/lander"
	"github.com/system-inc/loom/protocol"
)

// push is `loom push`, the lander's one pass (package lander), which loom-pusher.timer runs every 30 s on the machine
// whose ~/.loom/push.conf makes it the lander: blocks, then every landing order, fast-forward only, onto push.conf's
// branch. It exits 0 when the pass held nothing (or another pass holds the lock), 1 when it held an order or couldn't
// read the queue, 2 on a bad command line and 3 when its settings can't be read. `loom push install` is the updater
// hook's: it installs the release's units and never starts them.
func push(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) > 0 && arguments[0] == "install" {
		return pushInstall(arguments[1:], stdout, stderr)
	}
	home, _ := os.UserHomeDir()
	flags := flag.NewFlagSet("push", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", lander.HomePaths(home).Config, "the lander's settings: branch, queue, repository, state, secret")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: loom push [--config <push.conf>]")
		return 2
	}
	content, err := os.ReadFile(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "loom push: this machine's push settings, whose being there makes it the lander: %v\n", err)
		return 3
	}
	config, err := lander.ReadConfig(string(content), home)
	if err != nil {
		fmt.Fprintf(stderr, "loom push: %s: %v\n", *configPath, err)
		return 3
	}
	if err := os.MkdirAll(config.State, 0o755); err != nil {
		fmt.Fprintf(stderr, "loom push: %v\n", err)
		return 3
	}
	lock, err := os.OpenFile(filepath.Join(config.State, "lock"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(stderr, "loom push: %v\n", err)
		return 3
	}
	defer lock.Close()
	// One pass at a time: a pass still pushing when the timer fires again is left to finish, and this one is a no-op.
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return 0
	}
	secret, err := protocol.ReadTokenSecret(config.Secret)
	if err != nil {
		fmt.Fprintf(stderr, "loom push: %v\n", err)
		return 3
	}
	log := func(text string) {
		fmt.Fprintf(stdout, "%s pusher: %s\n", time.Now().UTC().Format("2006-01-02T15:04:05Z"), text)
	}
	pass := lander.Once(lander.HTTPQueue{Base: config.Queue, Secret: secret}, lander.Hands{Repository: config.Repository, Branch: config.Branch},
		lander.Chain{Repository: config.Repository, Branch: config.Branch}, log)
	if pass.Held > 0 || pass.Unread {
		return 1
	}
	return 0
}

// pushInstall is `loom push install`, which the updater's hook 60-push runs after every release: the release's
// loom-pusher units written where systemd reads them, never enabled or started (lander.Install).
func pushInstall(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) != 0 {
		fmt.Fprintln(stderr, "usage: loom push install")
		return 2
	}
	if runtime.GOOS != "linux" {
		fmt.Fprintf(stderr, "loom push install: the pusher is a systemd user unit, and this is %s\n", runtime.GOOS)
		return 3
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "loom push install: %v\n", err)
		return 3
	}
	systemctl := func(arguments ...string) (string, error) {
		output, err := exec.Command("systemctl", append([]string{"--user"}, arguments...)...).CombinedOutput()
		if err != nil {
			return string(output), fmt.Errorf("systemctl --user %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
		}
		return string(output), nil
	}
	if err := lander.Install(lander.HomePaths(home), home, systemctl, stdout); err != nil {
		fmt.Fprintf(stderr, "loom push install: %v\n", err)
		return 1
	}
	return 0
}
