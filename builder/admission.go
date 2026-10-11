package builder

import (
	"sync"
	"sync/atomic"
	"time"
)

// Seen Oct 10 (#b0yn1pw): main's tree build sat in its products stage at load ~14 of Workshop's 64 threads, with 4 to 8
// product tests running at a time. Most were store hits, each a go process's start and a round trip to the store, not
// CPU, and admitted ran them jobs() at a time and one start to a reading of the machine, two a second (ProcGauge reads
// over half a second), so the box idled. A tree's product tests are paced instead: the machine is read all along, each
// reading sets how many may run, and a job that ends is replaced at once, without waiting for one.

// An admission is how paced holds a phase's jobs: the most at once while the gauge reads (ceiling), the fixed count
// while it can't or there is none (blind), and how many more than are running a reading of busy under the target
// lets start (starts; nil, or a count under one: one).
type admission struct {
	ceiling, blind int
	starts         func(busy, target float64) int
}

// startsOn is how many more than are running may start on a reading of busy under target, and always at least one,
// so a reading under the target moves the build as admitted's does.
func (pace admission) startsOn(busy, target float64) int {
	if pace.starts == nil {
		return 1
	}
	return max(1, pace.starts(busy, target))
}

// productAdmission paces a tree's product tests. Its ceiling is ProductJobs (the hard bound, so a reading that lags
// behind a burst of starts can't fork thousands of go processes), and a gauge that can't read gets jobs(), the fixed
// count every phase had before. A reading under the target lets start as many as the headroom holds counting each one
// at its whole share of compile, the most its go process compiles at once: a build-heavy tree whose product tests each
// fill their share stays under the target, and a hit-heavy one, whose go processes use far less, reads low again and
// grows by the next headroom.
func (build TreeBuild) productAdmission() admission {
	threads, share := float64(build.compile()), float64(build.share())
	return admission{ceiling: build.productJobs(), blind: build.jobs(), starts: func(busy, target float64) int {
		return int((target - busy) * threads / share)
	}}
}

// productJobs is ProductJobs, or compile (every thread but four): more go processes at once than the threads a build
// may fill would be a reading lagging behind its starts, not the box allowing them.
func (build TreeBuild) productJobs() int {
	if build.ProductJobs > 0 {
		return build.ProductJobs
	}
	return build.compile()
}

// paced runs work for each index, as many at once as the machine holds. A sampler reads gauge back to back while the
// jobs run, and each reading sets room, the most that may run until the next: one under the target and with a fifth
// of memory available lets pace.startsOn more than are running then start, under pace.ceiling; one at or over the
// target, or short of memory, lets none start; one that can't read sets pace.blind, as does no gauge at all. A job that
// ends is replaced at once on the last reading's room, so a slow reading (ProcGauge's half second) never holds the
// build back, and each reading counts the jobs running when it lands, replacements and all. It always lets one run,
// so other load on the machine slows a build and never stops it, and it reads the disk before each start as admitted
// does (diskWait), refusing what can't start the same way.
func paced(count int, pace admission, gauge Gauge, target float64, disk func() error, work func(index int), refuse func(index int, err error)) {
	var group sync.WaitGroup
	var running, room atomic.Int64
	// changed wakes a start waiting for room when a job ends or a reading lands.
	changed := make(chan struct{}, 1)
	wake := func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}
	done := make(chan struct{})
	defer close(done)
	room.Store(int64(max(1, pace.blind)))
	if gauge != nil {
		room.Store(1)
		go func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				busy, available, ok := gauge()
				switch {
				case !ok:
					room.Store(int64(max(1, pace.blind)))
				case busy < target && available >= 0.2:
					room.Store(min(int64(max(1, pace.ceiling)), running.Load()+int64(pace.startsOn(busy, target))))
				default:
					room.Store(0)
				}
				wake()
				if !ok {
					// A gauge that can't read now likely can't a moment later either: it is asked again only now and then.
					select {
					case <-done:
						return
					case <-time.After(admissionPoll):
					}
				}
			}
		}()
	}
	for index := range count {
		for running.Load() > 0 && running.Load() >= room.Load() {
			<-changed
		}
		if short := diskWait(disk, &running); short != nil {
			refuse(index, short)
			continue
		}
		running.Add(1)
		group.Add(1)
		go func() {
			defer group.Done()
			work(index)
			running.Add(-1)
			wake()
		}()
	}
	group.Wait()
}
