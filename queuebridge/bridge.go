// Package queuebridge is the queue's bridge to git (#jtdwm5n, #vwsgnxm): every submitted change still unchecked gets
// git's facts from this machine's clone of adamic (the sha exists, its base is its ancestor and on main, the paths of
// base..sha, the history beyond its diff, the Python tests gate logic names, the main commit it reverts, and main's
// head with the queue's seq read before git), since no GitHub credential lives in Cloudflare. The queue decides on
// them. `loom queue-bridge` runs one pass a minute on Workshop, the machine whose ~/.loom/queue-bridge.conf makes it the
// bridge.
//
// The bridge only carries facts: Judge decides every future and the pusher lands (package lander), so nothing here
// posts a verdict or moves main (Loom, Oct 10).
package queuebridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/system-inc/loom/protocol"
)

// A Queue is the Queue's coordinator seam as the bridge calls it (wire/source/Queue.ts): a method, a path and a body
// (nil for none), answered with the status and the body's bytes. An error is a call that never got an answer.
type Queue interface {
	Call(method, path string, body any) (int, []byte, error)
}

// HTTPQueue calls the Queue at Base with a coordinator token minted from Secret for each call, good for ten minutes,
// the way the wire verifies it (wire/source/Token.ts).
type HTTPQueue struct {
	Base   string
	Secret []byte
	HTTP   *http.Client
}

func (queue HTTPQueue) Call(method, path string, body any) (int, []byte, error) {
	token, err := protocol.MintToken(queue.Secret, protocol.TokenClaims{Run: "queue-bridge", Scope: protocol.ScopeCoordinator, Expires: time.Now().Add(10 * time.Minute).Unix()})
	if err != nil {
		return 0, nil, err
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, strings.TrimSuffix(queue.Base, "/")+path, reader)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "loom-queue-bridge")
	client := queue.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, answer, nil
}

// call is Call with a transport error read as status 0, its text the answer, so every caller logs one shape.
func call(queue Queue, method, path string, body any) (int, []byte) {
	status, answer, err := queue.Call(method, path, body)
	if err != nil {
		return 0, []byte(fmt.Sprintf("%s %s: %v", method, path, err))
	}
	return status, answer
}

// answerText is an answer as one log line, cut at 300 bytes.
func answerText(answer []byte) string {
	return first(strings.Join(strings.Fields(string(answer)), " "), 300)
}

// first is text cut to at most limit bytes, never inside a character.
func first(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

// A Gate is what git says about a submitted change, mainHead among the facts. An error is git unable to say, and the
// change waits for the next pass.
type Gate interface {
	Facts(sha, base string) (map[string]any, error)
}

// A Bridge is one pass's hands: the queue, git, and where it logs.
type Bridge struct {
	Queue Queue
	Gate  Gate
	Log   func(string)
}

func (bridge Bridge) log(format string, arguments ...any) {
	bridge.Log(fmt.Sprintf(format, arguments...))
}

// Tick is one pass: git's facts for every unchecked change. It reports false when the queue couldn't be read.
func (bridge Bridge) Tick() bool {
	status, answer := call(bridge.Queue, "GET", "/submissions?state=unchecked", nil)
	var unchecked struct {
		Changes []struct {
			Change string `json:"change"`
			Sha    string `json:"sha"`
			Base   string `json:"base"`
		} `json:"changes"`
	}
	if status != 200 || json.Unmarshal(answer, &unchecked) != nil {
		bridge.log("submissions: %d %s", status, answerText(answer))
		return false
	}
	for _, submitted := range unchecked.Changes {
		facts := bridge.withHead(func() (map[string]any, error) { return bridge.Gate.Facts(submitted.Sha, submitted.Base) })
		if facts == nil {
			bridge.log("facts for %s wait for the next tick", submitted.Change)
			continue
		}
		status, answer := call(bridge.Queue, "POST", "/submissions/"+submitted.Change+"/facts", facts)
		bridge.log("facts for %s: %d %s", submitted.Change, status, answerText(answer))
	}
	return true
}

// withHead is git's facts, read after the queue's seq is noted: asOf orders their mainHead against the queue's own log,
// so a landing logged while git answered outranks the head it read. Facts that can't say main's head (the queue's seq
// or origin's main unreadable) are nil: posted, they'd check the change for good, and a witness checked without its
// head never records main.green or main.red, so the change waits for the next tick instead (#6gj7n9p).
func (bridge Bridge) withHead(read func() (map[string]any, error)) map[string]any {
	status, answer := call(bridge.Queue, "GET", "/head", nil)
	var head map[string]any
	seq := -1.0
	if status == 200 && json.Unmarshal(answer, &head) == nil {
		// A whole JSON number only: "7", true or 7.5 is no seq, and a negative one is refused below.
		if number, ok := head["seq"].(float64); ok && number == float64(int64(number)) {
			seq = number
		}
	}
	if seq < 0 {
		bridge.log("the queue's seq is unreadable (%d %s): no facts posted this tick", status, first(answerText(answer), 200))
		return nil
	}
	facts, err := read()
	if err != nil {
		bridge.log("git can't say: %v", err)
		return nil
	}
	if _, ok := facts["mainHead"]; !ok {
		bridge.log("origin/main is unreadable: no facts posted this tick")
		return nil
	}
	withSeq := map[string]any{"asOf": int64(seq)}
	for key, value := range facts {
		withSeq[key] = value
	}
	return withSeq
}
