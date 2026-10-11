package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// cgroupRoot is where the unified cgroup hierarchy is mounted.
const cgroupRoot = "/sys/fs/cgroup"

// serveCgroupName is the leaf serve itself lives in under its delegated cgroup, as loom-serve.service's
// DelegateSubgroup= names it: cgroup v2 lets a cgroup that hands controllers to its children hold no process itself.
const serveCgroupName = "serve"

// unitCgroupPrefix names each unit's cgroup beside serve's leaf.
const unitCgroupPrefix = "unit-"

// delegatedCgroups is the cgroup systemd delegated to this serve (loom-serve.service's Delegate=yes), readied to hold
// one cgroup per unit: serve in its own leaf, the cpu and memory controllers handed down, and what a killed serve left
// there removed. An error says why there is none (not cgroup v2, not delegated: a Codex instance, a Mac, a serve
// started by hand), and units then run without a share of their own, as one at a time always has.
func delegatedCgroups() (*unitCgroups, error) {
	content, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil, err
	}
	own := ""
	for line := range strings.SplitSeq(strings.TrimSpace(string(content)), "\n") {
		if path, found := strings.CutPrefix(line, "0::"); found {
			own = filepath.Join(cgroupRoot, path)
		}
	}
	if own == "" {
		return nil, errors.New("no cgroup v2 line in /proc/self/cgroup")
	}
	// systemd makes a serve/ subgroup only for a unit it delegated (DelegateSubgroup=serve), and the user manager marks a
	// delegated cgroup with no attribute (systemd 255), so the subgroup is how serve knows. A cgroup the system manager
	// delegated says so in trusted.delegate.
	parent := own
	switch {
	case filepath.Base(own) == serveCgroupName:
		parent = filepath.Dir(own)
	case !delegated(own):
		return nil, fmt.Errorf("%s isn't delegated to this serve: not a serve/ subgroup (DelegateSubgroup=serve), nor marked delegated", own)
	}
	controllers, err := os.ReadFile(filepath.Join(parent, "cgroup.controllers"))
	if err != nil {
		return nil, err
	}
	for _, controller := range []string{"cpu", "memory"} {
		if !slices.Contains(strings.Fields(string(controllers)), controller) {
			return nil, fmt.Errorf("%s has no %s controller to hand to its units", parent, controller)
		}
	}
	if parent == own {
		// Delegated without a subgroup for serve: serve moves itself into one, its every process with it.
		leaf := filepath.Join(parent, serveCgroupName)
		if err := os.Mkdir(leaf, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(leaf, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			return nil, fmt.Errorf("moving serve into %s: %w", leaf, err)
		}
	}
	cgroups := &unitCgroups{parent: parent}
	// A serve that was killed leaves its units' cgroups, perhaps with processes in them, and they are no one's now.
	entries, _ := os.ReadDir(parent)
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), unitCgroupPrefix) {
			(&unitCgroup{path: filepath.Join(parent, entry.Name()), directory: -1}).remove()
		}
	}
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+cpu +memory"), 0o644); err != nil {
		return nil, fmt.Errorf("handing cpu and memory to %s's children: %w", parent, err)
	}
	return cgroups, nil
}

// delegated says whether systemd marked the cgroup as delegated, in user.delegate or trusted.delegate (systemd 251 and
// later; the user manager of systemd 255 writes neither).
func delegated(path string) bool {
	value := make([]byte, 8)
	for _, attribute := range []string{"user.delegate", "trusted.delegate"} {
		if size, err := syscall.Getxattr(path, attribute, value); err == nil && string(value[:size]) == "1" {
			return true
		}
	}
	return false
}

// unitCgroups makes the cgroups of a serve's units under the cgroup delegated to it.
type unitCgroups struct {
	parent string
}

// make is a new cgroup for one unit, held to its share: cpu.max at its CPUs, memory.max at its memory with no swap
// beyond it, so the kernel kills only the unit's own processes when it passes it. Its commands are started in it
// (Options.share), so everything they start is in it too.
func (cgroups *unitCgroups) make(name string, cpus int, memoryMegabytes int) (*unitCgroup, error) {
	if cgroups == nil {
		return nil, nil
	}
	path := filepath.Join(cgroups.parent, unitCgroupPrefix+name)
	if err := os.Mkdir(path, 0o755); err != nil {
		return nil, err
	}
	group := &unitCgroup{path: path, directory: -1}
	settings := [][2]string{
		{"cpu.max", fmt.Sprintf("%d 100000", cpus*100000)},
		{"memory.max", strconv.FormatInt(int64(memoryMegabytes)<<20, 10)},
		{"memory.swap.max", "0"},
	}
	for _, setting := range settings {
		if err := os.WriteFile(filepath.Join(path, setting[0]), []byte(setting[1]), 0o644); err != nil {
			group.remove()
			return nil, fmt.Errorf("writing %s's %s: %w", path, setting[0], err)
		}
	}
	directory, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		group.remove()
		return nil, err
	}
	group.directory = directory
	return group, nil
}

// A unitCgroup is one unit's cgroup.
type unitCgroup struct {
	path string
	// directory is the cgroup's directory, open, which a command is started into (SysProcAttr.CgroupFD); -1 when
	// closed.
	directory int
}

// attach starts a command in the cgroup: clone3 puts it there before it runs anything.
func (group *unitCgroup) attach(attributes *syscall.SysProcAttr) {
	if group == nil || group.directory < 0 {
		return
	}
	attributes.UseCgroupFD, attributes.CgroupFD = true, group.directory
}

// oomKills is how many of the unit's processes the kernel killed for passing its memory.
func (group *unitCgroup) oomKills() int {
	if group == nil {
		return 0
	}
	content, err := os.ReadFile(filepath.Join(group.path, "memory.events"))
	if err != nil {
		return 0
	}
	for line := range strings.SplitSeq(string(content), "\n") {
		if count, found := strings.CutPrefix(line, "oom_kill "); found {
			kills, _ := strconv.Atoi(strings.TrimSpace(count))
			return kills
		}
	}
	return 0
}

// peakMegabytes is the most memory the unit's processes held at once, 0 when unreadable (memory.peak is Linux 5.19's).
func (group *unitCgroup) peakMegabytes() int64 {
	if group == nil {
		return 0
	}
	content, err := os.ReadFile(filepath.Join(group.path, "memory.peak"))
	if err != nil {
		return 0
	}
	peak, _ := strconv.ParseInt(strings.TrimSpace(string(content)), 10, 64)
	return peak >> 20
}

// remove kills whatever is left in the cgroup and removes it: a unit's processes never outlive it.
func (group *unitCgroup) remove() {
	if group == nil {
		return
	}
	if group.directory >= 0 {
		syscall.Close(group.directory)
		group.directory = -1
	}
	os.WriteFile(filepath.Join(group.path, "cgroup.kill"), []byte("1"), 0o644)
	for range 50 {
		if err := syscall.Rmdir(group.path); err == nil || errors.Is(err, syscall.ENOENT) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
