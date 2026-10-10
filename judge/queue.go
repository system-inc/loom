package judge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// HTTPQueue posts a future's verdicts to loom's Queue at POST /futures/<tree>/verdicts with the coordinator
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

// HTTPBlobs puts a tests list in loom's action store at PUT /actions/blobs/<sha256> with a build token
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

// HTTPReused reads the verdict a reused unit reuses from Queue's index, `GET /verdicts/<unitKey>` with the coordinator
// token, and holds it to what Queue checked when it took the plan: passed, and from the reused run when one is named.
type HTTPReused struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// Tests is that verdict's tests object, as it was posted.
func (reused HTTPReused) Tests(unitKey, run string) (json.RawMessage, error) {
	request, err := http.NewRequest("GET", strings.TrimSuffix(reused.Base, "/")+"/verdicts/"+url.PathEscape(unitKey), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+reused.Token)
	client := reused.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /verdicts/%s: %s: %s", unitKey, response.Status, strings.TrimSpace(string(body)))
	}
	var record struct {
		Status string          `json:"status"`
		Run    string          `json:"run"`
		Tests  json.RawMessage `json:"tests"`
	}
	if err := json.Unmarshal(body, &record); err != nil {
		return nil, fmt.Errorf("GET /verdicts/%s: %w", unitKey, err)
	}
	var ref TestsRef
	switch {
	case record.Status != Passed:
		return nil, fmt.Errorf("the index's verdict for %s is %s, not passed", unitKey, record.Status)
	case run != "reused" && record.Run != run:
		return nil, fmt.Errorf("the index's verdict for %s is from run %s, not the reused %s", unitKey, record.Run, run)
	case json.Unmarshal(record.Tests, &ref) != nil || !shaPattern64.MatchString(ref.Sha256):
		return nil, fmt.Errorf("the index's verdict for %s carries no tests by reference: %s", unitKey, record.Tests)
	}
	return record.Tests, nil
}

var shaPattern64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
