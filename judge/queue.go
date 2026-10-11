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

// HTTPReused reads the verdict a reused unit reuses from Queue's index, `GET /verdicts/<unitKey>` with the coordinator
// token. The index keeps each key's newest verdict, so it answers for the key, not the run the plan named: when it's a
// pass, from that run or a later one, it's the reuse (the same key is the same verdict); when it isn't, the reuse is
// unbacked.
type HTTPReused struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// Tests is the index's newest verdict for the key: its tests object, as it was posted, and its run.
func (reused HTTPReused) Tests(unitKey, run string) (ReusedVerdict, error) {
	request, err := http.NewRequest("GET", strings.TrimSuffix(reused.Base, "/")+"/verdicts/"+url.PathEscape(unitKey), nil)
	if err != nil {
		return ReusedVerdict{}, err
	}
	request.Header.Set("Authorization", "Bearer "+reused.Token)
	client := reused.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return ReusedVerdict{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return ReusedVerdict{}, err
	}
	if response.StatusCode != http.StatusOK {
		return ReusedVerdict{}, fmt.Errorf("GET /verdicts/%s: %s: %s", unitKey, response.Status, strings.TrimSpace(string(body)))
	}
	var record struct {
		Status string          `json:"status"`
		Run    string          `json:"run"`
		Tests  json.RawMessage `json:"tests"`
	}
	if err := json.Unmarshal(body, &record); err != nil {
		return ReusedVerdict{}, fmt.Errorf("GET /verdicts/%s: %w", unitKey, err)
	}
	if record.Status != Passed {
		return ReusedVerdict{Unbacked: fmt.Sprintf("the index's newest verdict for %s is %s, from run %s", unitKey, record.Status, record.Run)}, nil
	}
	var ref TestsRef
	if json.Unmarshal(record.Tests, &ref) != nil || !shaPattern64.MatchString(ref.Sha256) {
		return ReusedVerdict{}, fmt.Errorf("the index's verdict for %s carries no tests by reference: %s", unitKey, record.Tests)
	}
	return ReusedVerdict{Tests: record.Tests, Run: record.Run}, nil
}

var shaPattern64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
