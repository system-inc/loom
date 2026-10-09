package planner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// A Future is an unplanned future as Queue serves it at GET /futures?state=unplanned (contract v1.1), where the
// future is its tree sha (slice 1).
type Future struct {
	Future   string   `json:"future"`
	Tree     string   `json:"tree"` // the tree sha to plan
	Base     string   `json:"base"`
	Changes  []string `json:"changes"`
	Uncached bool     `json:"uncached"` // the witness and main's landing reuse nothing
}

// A QueueClient talks to loom-pipeline's planning routes with the coordinator token.
type QueueClient struct {
	Base  string
	Token string
	HTTP  *http.Client
}

func (client QueueClient) do(method, path string, body io.Reader) (*http.Response, error) {
	request, err := http.NewRequest(method, strings.TrimSuffix(client.Base, "/")+path, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+client.Token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	httpClient := client.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return httpClient.Do(request)
}

// Unplanned lists the futures waiting for a plan.
func (client QueueClient) Unplanned() ([]Future, error) {
	response, err := client.do("GET", "/futures?state=unplanned", nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /futures: %s", response.Status)
	}
	var listing struct {
		Futures []Future `json:"futures"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listing); err != nil {
		return nil, fmt.Errorf("GET /futures: %w", err)
	}
	return listing.Futures, nil
}

// PostPlan hands a future's plan to Queue as the bare unit list, and Queue writes one unit.planned event per unit. It
// recomputes each unit's key from its keyParts and refuses a mismatch, so the keyParts posted are the ones keyed.
func (client QueueClient) PostPlan(future string, results []PlannedResult) error {
	body, err := json.Marshal(results)
	if err != nil {
		return err
	}
	response, err := client.do("POST", "/futures/"+url.PathEscape(future)+"/plan", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return fmt.Errorf("POST /futures/%s/plan: %s: %s", future, response.Status, strings.TrimSpace(string(detail)))
	}
	return nil
}

// HTTPIndex reads Queue's verdict index, GET /verdicts/<unitKey>; a 404 is no decided verdict.
type HTTPIndex struct{ Client QueueClient }

func (index HTTPIndex) Latest(unitKey string) (Verdict, bool, error) {
	response, err := index.Client.do("GET", "/verdicts/"+unitKey, nil)
	if err != nil {
		return Verdict{}, false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return Verdict{}, false, nil
	}
	if response.StatusCode != http.StatusOK {
		return Verdict{}, false, fmt.Errorf("GET /verdicts/%s: %s", unitKey, response.Status)
	}
	var verdict Verdict
	if err := json.NewDecoder(response.Body).Decode(&verdict); err != nil {
		return Verdict{}, false, err
	}
	return verdict, true, nil
}

// A Checkout gives a tree at a sha and a function that removes it.
type Checkout func(sha string) (tree string, cleanup func(), err error)

// GitCheckout checks a sha out in the clone's own working tree, fetching it first if absent, with every submodule
// at its recorded commit (adamic's go.work and replaces reach into cohere and cohere's TypeScript, so go list needs
// them). The clone is the planner's alone and plans one future at a time, so one tree serves every future and its
// submodules' objects stay fetched. Submodules recorded over ssh are fetched from GitHub over https, as the runner's
// prepare.sh does.
func GitCheckout(repository string) Checkout {
	return func(sha string) (string, func(), error) {
		git := func(arguments ...string) error {
			command := exec.Command("git", append([]string{"-c", "url.https://github.com/.insteadOf=git@github.com:", "-C", repository}, arguments...)...)
			command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
			if output, err := command.CombinedOutput(); err != nil {
				return fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
			}
			return nil
		}
		if git("cat-file", "-e", sha+"^{commit}") != nil {
			if err := git("fetch", "--quiet", "origin", sha); err != nil {
				return "", nil, err
			}
		}
		for _, arguments := range [][]string{
			{"checkout", "--quiet", "--force", "--detach", sha},
			{"clean", "-q", "-ffdx"},
			{"submodule", "update", "--quiet", "--init", "--recursive", "--force"},
		} {
			if err := git(arguments...); err != nil {
				return "", nil, err
			}
		}
		return repository, func() {}, nil
	}
}

// PullOnce plans every unplanned future Queue serves and posts each plan back; it returns how many it planned. A
// future that fails to plan is reported and left unplanned, never posted half-made.
// With only set, it plans that future alone (a tree sha) and leaves every other to today's gate (Queue's rule until
// the judge's first live batch, 23:58Z).
func PullOnce(client QueueClient, checkout Checkout, gateTools string, tools Tools, index VerdictIndex, only string) (int, error) {
	futures, err := client.Unplanned()
	if err != nil {
		return 0, err
	}
	planned := 0
	var failures []string
	for _, future := range futures {
		if only != "" && future.Future != only {
			continue
		}
		tree, cleanup, err := checkout(future.Tree)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", future.Future, err))
			continue
		}
		results, err := PlanTree(tree, gateTools, tools, index, future.Uncached)
		cleanup()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", future.Future, err))
			continue
		}
		if err := client.PostPlan(future.Future, results); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", future.Future, err))
			continue
		}
		planned++
	}
	if len(failures) > 0 {
		return planned, fmt.Errorf("futures not planned: %s", strings.Join(failures, "; "))
	}
	return planned, nil
}
