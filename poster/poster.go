// Package poster posts event lines, as JSON lines, to a run's events endpoint on the wire, in batches, with
// backoff while it fails. The runner posts its unit's events through it; the coordinator relays a whole run's.
package poster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	batchEvents  = 1000             // the most events one POST carries
	pendingLimit = 100000           // past this many unsent events the wire is given up
	backoffLimit = 5 * time.Second  // the longest wait between attempts at a failing wire
	requestLimit = 10 * time.Second // one POST's deadline
)

// A Poster posts events while they happen. It never decides anything: when it fails it says so through
// Report and the caller carries on, because the caller keeps the whole stream either way. The Worker drops
// an event it already holds, so a retried batch that had landed is harmless.
type Poster struct {
	url      string
	token    string
	client   *http.Client
	interval time.Duration
	// Report is told of a failing wire once per run of failures; never called with the mutex held. Set it
	// before Loop starts.
	Report func(message string)

	mutex      sync.Mutex
	pending    [][]byte // encoded events not yet accepted, oldest first
	abandoned  bool
	overflowed bool

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// postError is a POST that failed; permanent when retrying the same batch can't help.
type postError struct {
	message   string
	permanent bool
}

func (err *postError) Error() string { return err.message }

// New makes a Poster for one events endpoint and token, posting what is pending at least every interval.
func New(url, token string, client *http.Client, interval time.Duration) *Poster {
	return &Poster{url: url, token: token, client: client, interval: interval, Report: func(string) {}, stop: make(chan struct{}), done: make(chan struct{})}
}

// Enqueue takes one encoded event. Callers may hold their own lock: it never reports.
func (poster *Poster) Enqueue(line []byte) {
	poster.mutex.Lock()
	defer poster.mutex.Unlock()
	if poster.abandoned {
		return
	}
	if len(poster.pending) >= pendingLimit {
		poster.abandoned = true
		poster.overflowed = true
		poster.pending = nil
		return
	}
	poster.pending = append(poster.pending, line)
}

// Abandon gives the wire up: nothing pending is posted and nothing more is taken.
func (poster *Poster) Abandon() {
	poster.mutex.Lock()
	defer poster.mutex.Unlock()
	poster.abandoned = true
	poster.pending = nil
}

// Loop posts what's pending every interval until Drain stops it, backing off while the wire fails and
// reporting each run of failures once.
func (poster *Poster) Loop() {
	defer close(poster.done)
	failures := 0
	timer := time.NewTimer(poster.interval)
	defer timer.Stop()
	for {
		select {
		case <-poster.stop:
			return
		case <-timer.C:
		}
		poster.mutex.Lock()
		overflowed := poster.overflowed
		poster.overflowed = false
		poster.mutex.Unlock()
		if overflowed {
			poster.Report(fmt.Sprintf("the wire fell %d events behind and was given up", pendingLimit))
			return
		}
		err := poster.send()
		var failure *postError
		switch {
		case err == nil:
			failures = 0
		case errors.As(err, &failure) && failure.permanent:
			poster.Abandon()
			poster.Report("the wire refused events and was given up: " + err.Error())
			return
		default:
			failures++
			if failures == 1 {
				poster.Report("posting to the wire failed, retrying: " + err.Error())
			}
		}
		timer.Reset(poster.backoff(failures))
	}
}

func (poster *Poster) backoff(failures int) time.Duration {
	delay := poster.interval
	for range failures {
		delay *= 2
		if delay >= backoffLimit {
			return backoffLimit
		}
	}
	return delay
}

// Drain stops Loop and posts everything pending, retrying until deadline. Run Loop first.
func (poster *Poster) Drain(deadline time.Time) error {
	poster.stopOnce.Do(func() { close(poster.stop) })
	<-poster.done
	failures := 0
	for {
		poster.mutex.Lock()
		abandoned, remaining := poster.abandoned, len(poster.pending)
		poster.mutex.Unlock()
		if abandoned {
			return fmt.Errorf("the wire was given up earlier in the unit")
		}
		if remaining == 0 {
			return nil
		}
		err := poster.send()
		if err == nil {
			failures = 0
			continue
		}
		var failure *postError
		if errors.As(err, &failure) && failure.permanent {
			return err
		}
		failures++
		delay := poster.backoff(failures)
		if time.Now().Add(delay).After(deadline) {
			return fmt.Errorf("%d events unsent at the deadline: %w", remaining, err)
		}
		time.Sleep(delay)
	}
}

// send posts one batch from the front of pending and drops it from pending once the wire accepts it.
func (poster *Poster) send() error {
	poster.mutex.Lock()
	batch := poster.pending[:min(len(poster.pending), batchEvents)]
	poster.mutex.Unlock()
	if len(batch) == 0 {
		return nil
	}
	requestContext, cancel := context.WithTimeout(context.Background(), requestLimit)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, poster.url, bytes.NewReader(bytes.Join(batch, nil)))
	if err != nil {
		return &postError{message: err.Error(), permanent: true}
	}
	request.Header.Set("Content-Type", "application/x-ndjson")
	if poster.token != "" {
		request.Header.Set("Authorization", "Bearer "+poster.token)
	}
	response, err := poster.client.Do(request)
	if err != nil {
		return &postError{message: err.Error()}
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
	response.Body.Close()
	if response.StatusCode/100 != 2 {
		status := response.StatusCode
		return &postError{
			message:   fmt.Sprintf("POST %s: %s %s", poster.url, response.Status, bytes.TrimSpace(body)),
			permanent: status/100 == 4 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests,
		}
	}
	poster.mutex.Lock()
	defer poster.mutex.Unlock()
	if !poster.abandoned {
		poster.pending = poster.pending[len(batch):]
	}
	return nil
}
