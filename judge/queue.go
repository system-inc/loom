package judge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// HTTPQueue posts a future's verdicts to loom-pipeline's Queue at POST /futures/<tree>/verdicts with the coordinator
// token (wire/source/Queue.ts, checkJudgeBatch), the route Judge and Queue agreed on Oct 9. It replaces StubQueue.
type HTTPQueue struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// PostVerdicts posts one future's batch. Queue answers 200 or 201 when it took the batch (200 for a repeat of the
// same run), 409 when another run already decided the future, and 422 when the batch disagrees with its own checks.
func (queue HTTPQueue) PostVerdicts(future string, post FuturePost) error {
	body, err := json.Marshal(post)
	if err != nil {
		return err
	}
	request, err := http.NewRequest("POST", strings.TrimSuffix(queue.Base, "/")+"/futures/"+url.PathEscape(future)+"/verdicts", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+queue.Token)
	request.Header.Set("Content-Type", "application/json")
	client := queue.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode/100 == 2 {
		return nil
	}
	detail, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
	return fmt.Errorf("POST /futures/%s/verdicts: %s: %s", future, response.Status, strings.TrimSpace(string(detail)))
}

// HTTPBlobs puts a tests list in loom-pipeline's action store at PUT /actions/blobs/<sha256> with a build token
// (wire/source/Actions.ts). The store checks the body against the name and keeps an existing blob as it is, so a put
// is safe to repeat.
type HTTPBlobs struct {
	Base  string
	Token func() (string, error) // a build token, minted per put
	HTTP  *http.Client
}

// Put puts the list; any answer but 2xx is an error, and the record that names it isn't posted.
func (blobs HTTPBlobs) Put(sha256 string, content []byte) error {
	token, err := blobs.Token()
	if err != nil {
		return err
	}
	request, err := http.NewRequest("PUT", strings.TrimSuffix(blobs.Base, "/")+"/actions/blobs/"+url.PathEscape(sha256), bytes.NewReader(content))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	client := blobs.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode/100 == 2 {
		return nil
	}
	detail, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
	return fmt.Errorf("PUT /actions/blobs/%s: %s: %s", sha256, response.Status, strings.TrimSpace(string(detail)))
}
