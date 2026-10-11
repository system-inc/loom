package treebuilder

import (
	"fmt"
	"sync"
	"time"
)

// A tree's build is stopped once nothing wants it any more (#drrnnkh's question, asked by the tree builder): no future
// Queue lists planned runs the tree and Judge asks for it no longer, as when its branch is withdrawn (landable-3's tree
// built 15 minutes past its withdrawal, ahead of landable-4's, Oct 10). Its child is killed, and it's recorded Stopped
// with why, never failed: a tree no listing wants isn't built again, and one a later future lists is built then.

// WantPoll is how often a running build's tree is asked about.
const WantPoll = 30 * time.Second

// unwanted says why nothing wants tree any more, or "" while something does. A listing or request file it can't read
// says nothing: only a listing read whole stops a build.
func (builder *Builder) unwanted(tree string) string {
	futures, err := builder.Source.Planned()
	if err != nil {
		return ""
	}
	wants, _ := Wanted(futures)
	if builder.Requests != nil {
		requests, err := builder.Requests()
		if err != nil {
			return ""
		}
		requested, _ := Requested(requests)
		wants = append(wants, requested...)
	}
	for _, want := range wants {
		if want.Tree == tree {
			return ""
		}
	}
	return fmt.Sprintf("no future Queue lists planned runs it, and Judge doesn't ask for it (of %d futures listed)", len(futures))
}

func (builder *Builder) wantPoll() time.Duration {
	if builder.WantPoll > 0 {
		return builder.WantPoll
	}
	return WantPoll
}

// A stopWatch asks every wantPoll, while a build's child runs, whether its tree is still wanted, and kills the child's
// process group once it isn't. why is what it found, "" while it hasn't stopped the build.
type stopWatch struct {
	mutex sync.Mutex
	why   string
	done  chan struct{}
	ended sync.WaitGroup
}

// watch starts asking about tree for the child pid.
func (builder *Builder) watch(tree string, pid int) *stopWatch {
	watch := &stopWatch{done: make(chan struct{})}
	if builder.Kill == nil {
		return watch
	}
	watch.ended.Add(1)
	go func() {
		defer watch.ended.Done()
		for watch.wait(builder) {
			if why := builder.unwanted(tree); why != "" {
				watch.mutex.Lock()
				watch.why = why
				watch.mutex.Unlock()
				builder.Kill(pid)
				return
			}
		}
	}()
	return watch
}

// wait waits wantPoll, through the builder's Sleep when it has one, and says whether the build still runs: the wait
// ends with the build, so a build that ends waits for no poll.
func (watch *stopWatch) wait(builder *Builder) bool {
	if builder.Sleep != nil {
		builder.Sleep(builder.wantPoll())
		select {
		case <-watch.done:
			return false
		default:
			return true
		}
	}
	timer := time.NewTimer(builder.wantPoll())
	defer timer.Stop()
	select {
	case <-watch.done:
		return false
	case <-timer.C:
		return true
	}
}

// end stops the asking and says why the build was stopped, "" when it wasn't.
func (watch *stopWatch) end() string {
	if watch == nil {
		return ""
	}
	close(watch.done)
	watch.ended.Wait()
	watch.mutex.Lock()
	defer watch.mutex.Unlock()
	return watch.why
}
