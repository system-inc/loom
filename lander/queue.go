// Package lander is the lander's hands on Workshop (#83m6zw8, #nvq0tc8): the only thing that moves adamic's target
// branch (main, unless push.conf names another), with the lander's deploy key (Host github-lander, ~/.ssh/loom_lander),
// never from Kirk's Mac. Each pass, `loom push` builds any block the queue opened (blocks.go), then pulls the landing
// orders and, in line order, fast-forwards the branch from the tip it just read to each order's exact tested sha:
//
//	git fetch origin +<sha>:refs/loom/land/<sha> <tip>      (the sha in a ref of its own, checked to be a commit)
//	git merge-base --is-ancestor <tip> <sha>                (or the branch moved: the change parks)
//	git push --force-with-lease=refs/heads/<branch>:<tip> origin refs/loom/land/<sha>:refs/heads/<branch>
//
// The lease is the only force, and it only ever fast-forwards: GitHub takes the push only while the branch is still at
// the tip the sha contains, so a branch that moved or was deleted since the read is refused and never created. A push
// that lands, once the branch reads back as the sha, is reported as {main, from, landed}; a branch already at the sha
// (a landing whose report was lost) is reported again with from the sha itself, which Queue takes as the same landing.
// A branch that moved is reported as {refused, main}, which parks the change. Anything else is held, never reported: a
// refusal by GitHub's rules or the key's permission (GH013, a protected branch, a read-only or deleted key) holds every
// order and stops the pass, and any other failure (the network, a sha GitHub lacks, a report the queue refused) holds
// that order and goes on to the next. The queue decided the sha; this only moves the branch to it.
package lander

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/system-inc/loom/protocol"
)

// A Queue is the Queue's coordinator seam as the lander calls it (wire/source/Queue.ts): a method, a path and a body
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

// Run is the token's run claim, which names the caller to the wire.
const Run = "pusher"

func (queue HTTPQueue) Call(method, path string, body any) (int, []byte, error) {
	token, err := protocol.MintToken(queue.Secret, protocol.TokenClaims{Run: Run, Scope: protocol.ScopeCoordinator, Expires: time.Now().Add(10 * time.Minute).Unix()})
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
	request.Header.Set("User-Agent", "loom-pusher")
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

// answerText is an answer as one log line, cut at 300 bytes.
func answerText(answer []byte) string {
	text := strings.Join(strings.Fields(string(answer)), " ")
	if len(text) > 300 {
		text = text[:300] + "..."
	}
	return text
}

// call is Call with a transport error read as status 0, its text the answer, so every caller logs one shape.
func call(queue Queue, method, path string, body any) (int, []byte) {
	status, answer, err := queue.Call(method, path, body)
	if err != nil {
		return 0, []byte(fmt.Sprintf("%s %s: %v", method, path, err))
	}
	return status, answer
}
