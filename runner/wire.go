package runner

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
	wireBatchEvents  = 1000             // the most events one POST carries
	wirePendingLimit = 100000           // past this many unsent events the wire is given up for the unit
	wireBackoffLimit = 5 * time.Second  // the longest wait between attempts at a failing wire
	wireRequestLimit = 10 * time.Second // one POST's deadline
)

// A wire posts a unit's events, as JSON lines, to the run's events endpoint while the unit runs. It never
// decides anything: when it fails it says so in an error event and the unit carries on, because stdout
// holds the whole stream either way. The Worker drops an event it already holds, so a retried batch that
// had landed is harmless.
type wire struct {
	url      string
	token    string
	client   *http.Client
	interval time.Duration
	report   func(message string) // emits an error event; never called with mutex held

	mutex      sync.Mutex
	pending    [][]byte // encoded events not yet accepted, oldest first
	abandoned  bool
	overflowed bool

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// wireError is a POST that failed; permanent when retrying the same batch can't help.
type wireError struct {
	message   string
	permanent bool
}

func (err *wireError) Error() string { return err.message }

func newWire(url, token string, client *http.Client, interval time.Duration) *wire {
	return &wire{url: url, token: token, client: client, interval: interval, stop: make(chan struct{}), done: make(chan struct{})}
}

// enqueue takes one encoded event. The emitter calls it with its own lock held, so it never reports.
func (wire *wire) enqueue(line []byte) {
	wire.mutex.Lock()
	defer wire.mutex.Unlock()
	if wire.abandoned {
		return
	}
	if len(wire.pending) >= wirePendingLimit {
		wire.abandoned = true
		wire.overflowed = true
		wire.pending = nil
		return
	}
	wire.pending = append(wire.pending, line)
}

func (wire *wire) abandon() {
	wire.mutex.Lock()
	defer wire.mutex.Unlock()
	wire.abandoned = true
	wire.pending = nil
}

// loop posts what's pending every interval until drain stops it, backing off while the wire fails and
// reporting each run of failures once.
func (wire *wire) loop() {
	defer close(wire.done)
	failures := 0
	timer := time.NewTimer(wire.interval)
	defer timer.Stop()
	for {
		select {
		case <-wire.stop:
			return
		case <-timer.C:
		}
		wire.mutex.Lock()
		overflowed := wire.overflowed
		wire.overflowed = false
		wire.mutex.Unlock()
		if overflowed {
			wire.report(fmt.Sprintf("the wire fell %d events behind and was given up for this unit", wirePendingLimit))
			return
		}
		err := wire.send()
		var failure *wireError
		switch {
		case err == nil:
			failures = 0
		case errors.As(err, &failure) && failure.permanent:
			wire.abandon()
			wire.report("the wire refused events and was given up for this unit: " + err.Error())
			return
		default:
			failures++
			if failures == 1 {
				wire.report("posting to the wire failed, retrying: " + err.Error())
			}
		}
		timer.Reset(wire.backoff(failures))
	}
}

func (wire *wire) backoff(failures int) time.Duration {
	delay := wire.interval
	for range failures {
		delay *= 2
		if delay >= wireBackoffLimit {
			return wireBackoffLimit
		}
	}
	return delay
}

// drain stops the loop and posts everything pending, retrying until deadline.
func (wire *wire) drain(deadline time.Time) error {
	wire.stopOnce.Do(func() { close(wire.stop) })
	<-wire.done
	failures := 0
	for {
		wire.mutex.Lock()
		abandoned, remaining := wire.abandoned, len(wire.pending)
		wire.mutex.Unlock()
		if abandoned {
			return fmt.Errorf("the wire was given up earlier in the unit")
		}
		if remaining == 0 {
			return nil
		}
		err := wire.send()
		if err == nil {
			failures = 0
			continue
		}
		var failure *wireError
		if errors.As(err, &failure) && failure.permanent {
			return err
		}
		failures++
		delay := wire.backoff(failures)
		if time.Now().Add(delay).After(deadline) {
			return fmt.Errorf("%d events unsent at the deadline: %w", remaining, err)
		}
		time.Sleep(delay)
	}
}

// send posts one batch from the front of pending and drops it from pending once the wire accepts it.
func (wire *wire) send() error {
	wire.mutex.Lock()
	batch := wire.pending[:min(len(wire.pending), wireBatchEvents)]
	wire.mutex.Unlock()
	if len(batch) == 0 {
		return nil
	}
	requestContext, cancel := context.WithTimeout(context.Background(), wireRequestLimit)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, wire.url, bytes.NewReader(bytes.Join(batch, nil)))
	if err != nil {
		return &wireError{message: err.Error(), permanent: true}
	}
	request.Header.Set("Content-Type", "application/x-ndjson")
	if wire.token != "" {
		request.Header.Set("Authorization", "Bearer "+wire.token)
	}
	response, err := wire.client.Do(request)
	if err != nil {
		return &wireError{message: err.Error()}
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
	response.Body.Close()
	if response.StatusCode/100 != 2 {
		status := response.StatusCode
		return &wireError{
			message:   fmt.Sprintf("POST %s: %s %s", wire.url, response.Status, bytes.TrimSpace(body)),
			permanent: status/100 == 4 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests,
		}
	}
	wire.mutex.Lock()
	defer wire.mutex.Unlock()
	if !wire.abandoned {
		wire.pending = wire.pending[len(batch):]
	}
	return nil
}
