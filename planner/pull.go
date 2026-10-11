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
	"slices"
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
	// Parity marks a parity run against today's gate; its Select is the box record's selection, which the plan holds
	// exactly, every unit run (Queue, 02ea607).
	Parity bool          `json:"parity"`
	Select *ParitySelect `json:"select,omitempty"`
}

// A ParitySelect is a parity run's selection: the packages it runs and, for a package run.py split, its test names.
type ParitySelect struct {
	Packages []string            `json:"packages"`
	Tests    map[string][]string `json:"tests,omitempty"`
	inputs   *ParityInputs       // what the box record ran with, set by PlanSelected
}

// A QueueClient talks to loom's planning routes with the coordinator token.
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

// ParityInputs is what a parity future's box record ran with: the gate inputs given, and its one change's paths and
// sample from the change record (GET /changes/<change>). Box gates aren't sampled, so the sample is empty.
func (client QueueClient) ParityInputs(future Future, gateInputs string) (ParityInputs, error) {
	if len(future.Changes) != 1 {
		return ParityInputs{}, fmt.Errorf("parity future %s holds %d changes, not one", future.Future, len(future.Changes))
	}
	if !Sha256Hex(gateInputs) {
		return ParityInputs{}, fmt.Errorf("a parity plan needs the gate inputs' manifest sha256, not %q", gateInputs)
	}
	paths, err := client.ChangePaths(future)
	if err != nil {
		return ParityInputs{}, err
	}
	return ParityInputs{GateInputs: gateInputs, ChangedPaths: paths}, nil
}

// ChangePaths is every path a future's changes touch, from each change's record (GET /changes/<change>).
func (client QueueClient) ChangePaths(future Future) ([]string, error) {
	paths := []string{}
	for _, change := range future.Changes {
		response, err := client.do("GET", "/changes/"+url.PathEscape(change), nil)
		if err != nil {
			return nil, err
		}
		var record struct {
			Record struct {
				Paths []string `json:"paths"`
			} `json:"record"`
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return nil, fmt.Errorf("GET /changes/%s: %s", change, response.Status)
		}
		err = json.NewDecoder(response.Body).Decode(&record)
		response.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("GET /changes/%s: %w", change, err)
		}
		paths = append(paths, record.Record.Paths...)
	}
	return paths, nil
}

// ChangeState is a change's state and the future it's tested in now (GET /changes/<change>): the placer asks it of a
// run whose future Queue stopped listing (#drrnnkh). A change Queue doesn't know is an error, never read as gone.
func (client QueueClient) ChangeState(change string) (string, string, error) {
	response, err := client.do("GET", "/changes/"+url.PathEscape(change), nil)
	if err != nil {
		return "", "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("GET /changes/%s: %s", change, response.Status)
	}
	var read struct {
		State  string  `json:"state"`
		Future *string `json:"future"`
	}
	if err := json.NewDecoder(response.Body).Decode(&read); err != nil {
		return "", "", fmt.Errorf("GET /changes/%s: %w", change, err)
	}
	if read.State == "" {
		return "", "", fmt.Errorf("GET /changes/%s: no state", change)
	}
	future := ""
	if read.Future != nil {
		future = *read.Future
	}
	return read.State, future, nil
}

// PostEmpty tells Queue a future moves no unit's key, so it has nothing to run (Queue, Oct 10 01:31Z). Queue takes it
// only when every path of the future's changes ends in .md, as a second lock beside the planner's own.
func (client QueueClient) PostEmpty(future, reason string) error {
	body, err := json.Marshal(map[string]any{"empty": true, "reason": reason})
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
		return fmt.Errorf("POST /futures/%s/plan (empty): %s: %s", future, response.Status, strings.TrimSpace(string(detail)))
	}
	return nil
}

// Unplan asks Queue to withdraw a future's plan so it's planned again (POST /futures/<tree>/unplan {by, reason}): Queue
// takes it only after a void and while no green or red stands (#0zndrgw), and logs it with who and why.
func (client QueueClient) Unplan(future, by, reason string) error {
	body, err := json.Marshal(map[string]any{"by": by, "reason": reason})
	if err != nil {
		return err
	}
	response, err := client.do("POST", "/futures/"+url.PathEscape(future)+"/unplan", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return fmt.Errorf("POST /futures/%s/unplan: %s: %s", future, response.Status, strings.TrimSpace(string(detail)))
	}
	return nil
}

// PlannedFuture reads a planned future's base and change from Queue's planned listing (GET /futures?state=planned).
func (client QueueClient) PlannedFuture(future string) (Future, error) {
	response, err := client.do("GET", "/futures?state=planned", nil)
	if err != nil {
		return Future{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Future{}, fmt.Errorf("GET /futures?state=planned: %s", response.Status)
	}
	var listing struct {
		Futures []struct {
			Future string `json:"future"`
			Base   string `json:"base"`
			Parity bool   `json:"parity"`
			Change struct {
				Change string `json:"change"`
			} `json:"change"`
		} `json:"futures"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listing); err != nil {
		return Future{}, fmt.Errorf("GET /futures?state=planned: %w", err)
	}
	for _, planned := range listing.Futures {
		if planned.Future == future {
			return Future{Future: planned.Future, Tree: planned.Future, Base: planned.Base, Parity: planned.Parity, Changes: []string{planned.Change.Change}}, nil
		}
	}
	return Future{}, fmt.Errorf("future %s isn't in the planned listing", future)
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
// them). The clone is its caller's alone (the planner's, or Workshop's tree builder's) and checks out one future at a
// time, so one tree serves every future and its submodules' objects stay fetched. Submodules recorded over ssh are
// fetched from GitHub over https, as the runner's prepare.sh does.
//
// It is keyless: git reads no system or global configuration and no credential helper, and never prompts, so the
// tree comes only from what its origin serves to anyone, as a runner's checkout does.
func GitCheckout(repository string) Checkout {
	return func(sha string) (string, func(), error) {
		git := func(arguments ...string) error {
			if output, err := KeylessGit(append([]string{"-C", repository}, arguments...)...).CombinedOutput(); err != nil {
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

// keylessProtocols are the transports a keyless git may use, colon-separated: https alone. A test of a local clone
// adds file.
var keylessProtocols = "https"

// keylessDropped are the environment's variables a keyless git never sees: an ssh agent and ssh commands, askpass
// programs, an allowed-protocol list, and configuration given through the environment, any of which could hand git a
// key or a transport that uses one.
var keylessDropped = []string{"SSH_AUTH_SOCK", "GIT_SSH", "GIT_SSH_COMMAND", "GIT_ASKPASS", "SSH_ASKPASS", "GIT_ALLOW_PROTOCOL",
	"GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_GLOBAL"}

// KeylessGit is git with arguments, keyless, so what it fetches is only what GitHub serves anyone (review of
// tree-wiring, finding 3: the source a tree builder checks out, submodules included, is published):
//   - no system or global configuration, and none from the environment, whose url rewrites could send a fetch over ssh
//     and whose credential helpers could answer GitHub;
//   - no credential helper, askpass, ssh agent or terminal prompt;
//   - https is the only transport, for the repository and every submodule (protocol.allow=never with https allowed,
//     and GIT_ALLOW_PROTOCOL, which submodule fetches honor), and ssh is a command that fails, so an ssh:// or scp-like
//     URL in a change's own .gitmodules is refused without ssh ever running;
//   - GitHub's ssh URLs (git@github.com: and ssh://git@github.com/, as adamic records its submodules) are fetched from
//     the same repositories over https instead.
func KeylessGit(arguments ...string) *exec.Cmd {
	config := []string{"-c", "url.https://github.com/.insteadOf=git@github.com:", "-c", "url.https://github.com/.insteadOf=ssh://git@github.com/",
		"-c", "credential.helper=", "-c", "core.askPass=", "-c", "core.sshCommand=false", "-c", "protocol.allow=never"}
	for _, protocol := range strings.Split(keylessProtocols, ":") {
		config = append(config, "-c", "protocol."+protocol+".allow=always")
	}
	command := exec.Command("git", append(config, arguments...)...)
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		if !slices.Contains(keylessDropped, name) && !strings.HasPrefix(name, "GIT_CONFIG_KEY_") && !strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			command.Env = append(command.Env, variable)
		}
	}
	command.Env = append(command.Env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_ALLOW_PROTOCOL="+keylessProtocols, "GIT_SSH_COMMAND=false")
	return command
}

// PullOnce plans every unplanned future Queue serves and posts each plan back; it returns how many it planned. A
// future that fails to plan is reported and left unplanned, never posted half-made.
// With only set, it plans that future alone (a tree sha) and leaves every other to today's gate (Queue's rule until
// the judge's first live batch, 23:58Z).
//
// gateInputs is the gate inputs' manifest sha256 a parity run's box record ran with.
func PullOnce(client QueueClient, checkout Checkout, gateTools string, tools Tools, index VerdictIndex, only, gateInputs string) (int, error) {
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
		var results []PlannedResult
		var paths []string
		if future.Parity || future.Select != nil {
			// A parity plan runs what the box ran (its selection, or every tested package when it names none) with the
			// box's inputs, and reuses nothing.
			selection := ParitySelect{}
			if future.Select != nil {
				selection = *future.Select
			}
			var inputs ParityInputs
			if inputs, err = client.ParityInputs(future, gateInputs); err == nil {
				results, err = PlanSelected(tree, gateTools, tools, selection, inputs)
			}
		} else {
			// A change runs with the gate inputs and its paths, keyed (Loom, 01:40Z); a plan without the gate inputs
			// is never posted, since its corpus and pinned-TypeScript tests would skip into a pass.
			switch {
			case !Sha256Hex(gateInputs):
				err = fmt.Errorf("a change's plan needs the gate inputs' manifest sha256 (--gate-inputs-file), not %q", gateInputs)
			default:
				if paths, err = client.ChangePaths(future); err == nil {
					results, err = PlanChange(tree, gateTools, tools, index, future.Uncached, ParityInputs{GateInputs: gateInputs, ChangedPaths: paths})
				}
			}
		}
		// The phase units the box fast gate runs on this change, listed by run.py at this tree (Loom, 01:41Z).
		var phases []PlannedResult
		if err == nil {
			phaseInputs := ParityInputs{GateInputs: gateInputs, ChangedPaths: paths}
			if future.Parity || future.Select != nil {
				phaseInputs, err = client.ParityInputs(future, gateInputs)
			}
			if err == nil {
				phases, err = PhaseUnits(tree, gateTools, future.Base, future.Tree, tools, phaseInputs)
			}
		}
		if err == nil {
			err = carryTree(tree, tools.Go, phases, results)
		}
		cleanup()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", future.Future, err))
			continue
		}
		if !future.Parity && future.Select == nil {
			empty, reason, err := unmoved(client, checkout, future, gateTools, tools, ParityInputs{GateInputs: gateInputs, ChangedPaths: paths}, results)
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", future.Future, err))
				continue
			}
			if empty {
				if err := client.PostEmpty(future.Future, reason); err != nil {
					failures = append(failures, fmt.Sprintf("%s: %v", future.Future, err))
					continue
				}
				planned++
				continue
			}
		}
		if err := client.PostPlan(future.Future, append(results, phases...)); err != nil {
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

// carryTree sets the checked-out tree's key on every unit of its plan that reads the tree's build (#w7agfa9), read here
// where the tree is, by the one function `loom build-tree` keys it with, so the placer can name the build a unit runs
// and Workshop's builder knows what to build. A tree that can't be keyed isn't planned, and neither is one whose Go
// release isn't the one every unit's key names (keyParts.tools.go, the release the plan carries to the builder): its
// build would run another go than its units were keyed on.
func carryTree(tree, goRelease string, plans ...[]PlannedResult) error {
	identity, err := ReadTreeIdentity(tree)
	if err != nil {
		return fmt.Errorf("the tree's key: %w", err)
	}
	if identity.Go != goRelease {
		return fmt.Errorf("the tree's key: its build's go is %s, and its units are keyed on %s", identity.Go, goRelease)
	}
	for _, results := range plans {
		for index := range results {
			if ReadsTreeBuild(results[index].KeyParts.Kind) {
				results[index].Tree = identity.Key()
			}
		}
	}
	return nil
}

// unmoved says a future moves no unit's key: every unit keys at the future's tree as it keys at its base. That is read
// from the keys, never from file extensions, so a Markdown file a test embeds or reads still plans its readers. Only
// a change whose every path ends in .md is checked, since Queue refuses an empty plan for any other (and the base's
// keys cost a second plan).
func unmoved(client QueueClient, checkout Checkout, future Future, gateTools string, tools Tools, inputs ParityInputs, results []PlannedResult) (bool, string, error) {
	if future.Base == "" {
		return false, "", nil
	}
	paths, err := client.ChangePaths(future)
	if err != nil {
		return false, "", err
	}
	if len(paths) == 0 {
		return false, "", nil
	}
	for _, path := range paths {
		if !strings.HasSuffix(path, ".md") {
			return false, "", nil
		}
	}
	tree, cleanup, err := checkout(future.Base)
	if err != nil {
		return false, "", err
	}
	base, err := PlanChange(tree, gateTools, tools, MemoryIndex{}, true, inputs)
	cleanup()
	if err != nil {
		return false, "", fmt.Errorf("base %s: %w", future.Base, err)
	}
	baseKeys := map[string]string{}
	for _, unit := range base {
		baseKeys[unit.Name] = unit.UnitKey
	}
	if len(base) != len(results) {
		return false, "", nil
	}
	for _, unit := range results {
		if baseKeys[unit.Name] != unit.UnitKey {
			return false, "", nil
		}
	}
	return true, fmt.Sprintf("no unit's key moved: all %d units key at %s as at base %s", len(results), short(future.Tree), short(future.Base)), nil
}

// short is a sha's first 12 characters, or all of it when shorter.
func short(sha string) string {
	return sha[:min(12, len(sha))]
}
