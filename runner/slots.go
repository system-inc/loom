package runner

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/system-inc/loom/protocol"
)

// A box serve runs several units at once (#ef2rgaq), each on its share of the machine: the CPUs and memory it
// declares (protocol.Resources), or serve's defaults when it declares none, held to them by a cgroup of its own where
// systemd delegated one (cgroups_linux.go). Serve asks for another unit only while the shares in hand leave room for
// one more and the machine is under its busy target. Only a prebuilt test job runs beside others: it reads its tree's
// source, products and binaries through locked caches and keeps everything else in its own directory. A unit that
// checks out the root's one tree, or that names a runner other than serve's own, whose locking serve can't vouch for,
// runs alone.

// A share is one unit's part of the machine: its CPUs and memory, and the cgroup holding it to them (nil where there
// is none). A unit run alone has none and may use the whole machine.
type share struct {
	cpus            int
	memoryMegabytes int
	cgroup          *unitCgroup
	// inHand is how many units serve held when this one started, itself among them.
	inHand int
}

// alongside says whether a unit may run beside others on this serve: a prebuilt test job, on serve's own runner.
func alongside(unit protocol.Unit) bool {
	job := unit.Test
	return job != nil && job.Tree != "" && job.Phase == "" && (job.Runner == "" || job.Runner == selfSha256())
}

// A busyMeter reads how busy the machine's CPUs were between two reads, from /proc/stat.
type busyMeter struct {
	total, idle uint64
	at          time.Time
}

// busyWindow is the longest span a read of busy speaks for: over a longer one, the idle minutes before the units in hand
// started would hide how busy they keep the machine now.
const busyWindow = 15 * time.Second

// busy is the fraction of the machine's CPU time spent busy since the last call; ok is false on the first call, after
// more than busyWindow, or where CPU time can't be read.
func (meter *busyMeter) busy() (float64, bool) {
	total, idle, ok := cpuTimes()
	if !ok {
		return 0, false
	}
	previousTotal, previousIdle, at := meter.total, meter.idle, meter.at
	meter.total, meter.idle, meter.at = total, idle, time.Now()
	if at.IsZero() || time.Since(at) > busyWindow || total <= previousTotal {
		return 0, false
	}
	return 1 - float64(idle-previousIdle)/float64(total-previousTotal), true
}

// unitsLockName is the root's lock every test job holds while it runs: shared by a prebuilt one, which keeps to its own
// directory and the locked caches, exclusive by one that checks out the root's one tree. A trim of earlier units'
// leavings on the root runs only when it can take the lock exclusive at once, since a running unit's files aren't
// leavings.
const unitsLockName = ".loom-units.lock"

// holdRoot holds the root's units lock until release: exclusive, or shared. With trim, it first tries for the lock
// exclusive without waiting and runs trim while it holds it, then keeps it shared; with others running, trim doesn't
// run. It waits for the lock until holdContext ends.
func holdRoot(holdContext context.Context, root string, exclusive bool, trim func()) (func(), error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(root, unitsLockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	release := func() { lock.Close() }
	if trim != nil && syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
		trim()
		if exclusive {
			return release, nil
		}
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := waitForLock(holdContext, lock, how); err != nil {
		lock.Close()
		return nil, err
	}
	return release, nil
}

// trimAlone runs trim with the root's units lock held exclusive, only when no unit holds it: trimmed is false when one
// does.
func trimAlone(root string, trim func()) bool {
	lock, err := os.OpenFile(filepath.Join(root, unitsLockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return false
	}
	trim()
	return true
}

// holdGateInputs holds the gate inputs named name shared until release, so no other unit's preparation removes their
// directory while this unit's tests read it: prepare.sh removes a directory of gate inputs other than its own only when
// it can take its lock exclusive at once. The lock file outlives the directory, so every unit locks the same file.
func holdGateInputs(holdContext context.Context, root string, name string) (func(), error) {
	tools := filepath.Join(root, "adamic-tools")
	if err := os.MkdirAll(tools, 0o755); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(tools, "gate-inputs-"+name+".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := waitForLock(holdContext, lock, syscall.LOCK_SH); err != nil {
		lock.Close()
		return nil, err
	}
	return func() { lock.Close() }, nil
}

// attach starts a command in the share's cgroup, when there is one.
func (share *share) attach(attributes *syscall.SysProcAttr) {
	if share != nil {
		share.cgroup.attach(attributes)
	}
}

// packagesAtOnce is how many of a test job's packages run at once: half the unit's CPUs, its share's or the machine's.
func (share *share) packagesAtOnce(machineCpus int) int {
	cpus := machineCpus
	if share != nil {
		cpus = share.cpus
	}
	return max(1, cpus/2)
}
