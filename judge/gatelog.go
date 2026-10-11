package judge

// Reading the gate's mutants back from Queue's log (#4rtjr81): every parity future's decision and its unit records
// are verdict.decided events, so the suite's readings are a pure function of the log, and GateTheGate judges them
// the same way whoever reads it.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// A LogEvent is one event of Queue's hash-chained log, as `GET /log` serves it.
type LogEvent struct {
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	Subject LogSubject      `json:"subject"`
	Data    json.RawMessage `json:"data"`
}

// LogSubject names what an event is about.
type LogSubject struct {
	Change  string `json:"change"`
	Future  string `json:"future"`
	UnitKey string `json:"unitKey"`
	Run     string `json:"run"`
}

// DecidedBatches rebuilds each future's newest decided batch from the log: its decision and the unit records of the
// run that decided it. A change.red's kicks are attached to their decision. Futures never decided are absent.
func DecidedBatches(events []LogEvent) (map[string]FuturePost, error) {
	type decided struct {
		seq  int64
		post FuturePost
	}
	newest := map[string]decided{}
	records := map[string][]json.RawMessage{} // keyed by future + " " + run
	kicks := map[string]map[string]Kick{}     // the same
	for _, event := range events {
		key := event.Subject.Future + " " + event.Subject.Run
		switch {
		case event.Type == "verdict.decided" && event.Subject.UnitKey != "":
			var data struct {
				Verdict json.RawMessage `json:"verdict"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return nil, fmt.Errorf("event %d: %w", event.Seq, err)
			}
			records[key] = append(records[key], data.Verdict)
		case event.Type == "verdict.decided" && event.Subject.Future != "":
			var data struct {
				Decision RunVerdict `json:"decision"`
				Rule     string     `json:"rule"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return nil, fmt.Errorf("event %d: %w", event.Seq, err)
			}
			newest[event.Subject.Future] = decided{seq: event.Seq, post: FuturePost{Change: event.Subject.Change, Run: event.Subject.Run, Rule: data.Rule,
				Decision: PostDecision{RunVerdict: data.Decision}}}
		case event.Type == "change.red":
			var data struct {
				Kicks map[string]Kick `json:"kicks"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return nil, fmt.Errorf("event %d: %w", event.Seq, err)
			}
			kicks[key] = data.Kicks
		}
	}
	batches := map[string]FuturePost{}
	for future, found := range newest {
		post := found.post
		key := future + " " + post.Run
		post.Verdicts = records[key]
		post.Decision.Kicks = kicks[key]
		batches[future] = post
	}
	return batches, nil
}

// HTTPLog reads Queue's log page by page (`GET /log?after=<seq>`, a coordinator or board token).
type HTTPLog struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// Events reads the whole log.
func (log HTTPLog) Events() ([]LogEvent, error) {
	return log.EventsAfter(0)
}

// EventsAfter reads the log's events after seq after, every page of them.
func (log HTTPLog) EventsAfter(after int64) ([]LogEvent, error) {
	client := log.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	events := []LogEvent{}
	for {
		request, err := http.NewRequest("GET", fmt.Sprintf("%s/log?after=%d", strings.TrimSuffix(log.Base, "/"), after), nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", "Bearer "+log.Token)
		response, err := client.Do(request)
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusOK {
			detail, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
			response.Body.Close()
			return nil, fmt.Errorf("GET /log: %s: %s", response.Status, strings.TrimSpace(string(detail)))
		}
		page := 0
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 1<<20), 4<<20)
		for scanner.Scan() {
			var event LogEvent
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				response.Body.Close()
				return nil, fmt.Errorf("GET /log after %d: %w", after, err)
			}
			events, after, page = append(events, event), event.Seq, page+1
		}
		err = scanner.Err()
		response.Body.Close()
		if err != nil {
			return nil, err
		}
		if page == 0 {
			return events, nil
		}
	}
}
