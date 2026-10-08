package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/system-inc/loom/protocol"
)

// A wireClient makes the coordinator's calls to the wire: the plan, the verdict, the cache and blob checks.
// Events go through a poster.Poster instead, as they happen.
type wireClient struct {
	url    string // the Worker's origin, such as https://loom-wire.kirk-ouimet.workers.dev
	client *http.Client
}

// errNotFound is a 404: no cache entry, no blob.
var errNotFound = errors.New("not found")

func (wire *wireClient) call(callContext context.Context, method string, path string, token string, body []byte) ([]byte, error) {
	var lastError error
	for attempt := range 3 {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		request, err := http.NewRequestWithContext(callContext, method, strings.TrimSuffix(wire.url, "/")+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		response, err := wire.client.Do(request)
		if err != nil {
			lastError = err
			continue
		}
		answer, _ := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		response.Body.Close()
		switch {
		case response.StatusCode == http.StatusNotFound:
			return nil, errNotFound
		case response.StatusCode/100 == 2:
			return answer, nil
		case response.StatusCode/100 == 5 || response.StatusCode == http.StatusTooManyRequests:
			lastError = fmt.Errorf("%s %s: %s %s", method, path, response.Status, bytes.TrimSpace(answer))
		default:
			return nil, fmt.Errorf("%s %s: %s %s", method, path, response.Status, bytes.TrimSpace(answer))
		}
	}
	return nil, lastError
}

func (wire *wireClient) postPlan(callContext context.Context, run string, token string, plan protocol.Plan) error {
	body, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	_, err = wire.call(callContext, http.MethodPost, "/runs/"+run+"/plan", token, body)
	return err
}

func (wire *wireClient) postVerdict(callContext context.Context, run string, token string, verdict protocol.Verdict) error {
	body, err := json.Marshal(verdict)
	if err != nil {
		return err
	}
	_, err = wire.call(callContext, http.MethodPost, "/runs/"+run+"/verdict", token, body)
	return err
}

// hasBlob asks whether the store holds a blob, without moving its bytes.
func (wire *wireClient) hasBlob(callContext context.Context, run string, token string, sha256 string) (bool, error) {
	_, err := wire.call(callContext, http.MethodHead, "/runs/"+run+"/blobs/"+sha256, token, nil)
	if errors.Is(err, errNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (wire *wireClient) putBlob(callContext context.Context, run string, token string, sha256 string, content []byte) error {
	_, err := wire.call(callContext, http.MethodPut, "/runs/"+run+"/blobs/"+sha256, token, content)
	return err
}

// cacheEntry reads the entry for a key; a miss is (nil, nil).
func (wire *wireClient) cacheEntry(callContext context.Context, token string, key string) (*protocol.CacheEntry, error) {
	answer, err := wire.call(callContext, http.MethodGet, "/cache/"+key, token, nil)
	if errors.Is(err, errNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entry protocol.CacheEntry
	if err := protocol.Decode(bytes.NewReader(answer), &entry); err != nil {
		return nil, fmt.Errorf("cache entry %s: %w", key, err)
	}
	if entry.Key != key || protocol.CheckCacheEntry(entry) != nil {
		return nil, fmt.Errorf("cache entry %s is malformed", key)
	}
	return &entry, nil
}

// A BoardMachine is what the board shows of one machine before it counts the units running there.
type BoardMachine struct {
	Name  string `json:"name"`
	Cores int    `json:"cores"`
	Slots int    `json:"slots"`
}

func (wire *wireClient) postBoardMachines(callContext context.Context, token string, machines []BoardMachine) error {
	body, err := json.Marshal(map[string][]BoardMachine{"machines": machines})
	if err != nil {
		return err
	}
	_, err = wire.call(callContext, http.MethodPost, "/board/machines", token, body)
	return err
}

func (wire *wireClient) putCacheEntry(callContext context.Context, token string, entry protocol.CacheEntry) error {
	body, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = wire.call(callContext, http.MethodPut, "/cache/"+entry.Key, token, body)
	return err
}
