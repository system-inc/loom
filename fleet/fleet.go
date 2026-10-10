// Package fleet is the coordinator's side of the fleet (#83911s7, Kirk Oct 10: "i should be able to add any compute to
// the fleet and have pools that participate or individual machines"). The fleet is a registry of sources, each a Codex
// fleet, a box, a Mac or anything added later, with a kind, a cap (the most units at once), a tier and an on switch,
// held in loom. A change to a source is a rule.changed event in Queue's log, so every cap change is recorded
// with who made it and when; each source's latest counts come from its arm (rearm.sh for Codex), once per pass.
//
// The cap is a ceiling, never a target. Kirk's own Mac is never a source.
package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/system-inc/loom/protocol"
)

// Kinds are what a source may be.
var Kinds = []string{"codex", "box", "mac", "other"}

// MaximumCap is the highest cap a source may carry, the wire's top priority's count of units at once.
const MaximumCap = 1000

// DefaultCodexCap is rearm's cap when neither loom nor ~/.loom/codex-ceiling answers (Oct 9: the account's
// wall is about 310 members; 80 is what the file held).
const DefaultCodexCap = 80

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// neverSources are Kirk's own Mac, by every name it goes by: it never takes part in Loom (Kirk, Oct 9 23:36Z).
var neverSources = []string{"kirk-mac", "kirks-mac", "kirk-macbook", "kirk"}

// A Source is one source as loom answers it.
type Source struct {
	Name    string  `json:"name"`
	Kind    string  `json:"kind"`
	Cap     int     `json:"cap"`
	Tier    int     `json:"tier"`
	On      bool    `json:"on"`
	Changed Changed `json:"changed"`
	Counts  *Counts `json:"counts"`
}

// Changed is the rule.changed event that last set the source.
type Changed struct {
	Seq int    `json:"seq"`
	At  string `json:"at"`
	By  string `json:"by"`
}

// Counts are a source's arm's latest look at it, posted once per pass, never per member.
type Counts struct {
	Running         int    `json:"running"`
	Asking          int    `json:"asking"`
	Warming         int    `json:"warming"`
	Idle            int    `json:"idle"`
	RefusedByReview int    `json:"refusedByReview"`
	Retired         int    `json:"retired"`
	TurnsLastHour   int    `json:"turnsLastHour"`
	At              string `json:"at,omitempty"`
}

// A Change sets a source's fields; a nil field keeps its value. By names who made it, for the log.
type Change struct {
	Kind *string `json:"kind,omitempty"`
	Cap  *int    `json:"cap,omitempty"`
	Tier *int    `json:"tier,omitempty"`
	On   *bool   `json:"on,omitempty"`
	By   string  `json:"by"`
}

// CheckName refuses a name that isn't a plain lowercase name, or that names Kirk's own Mac.
func CheckName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("source %q: lowercase letters, digits and dashes, up to 64", name)
	}
	for _, never := range neverSources {
		if name == never {
			return fmt.Errorf("source %q is Kirk's own Mac, which never takes part in Loom", name)
		}
	}
	return nil
}

// Check refuses a change no source could carry.
func (change Change) Check() error {
	if change.Kind != nil && !contains(Kinds, *change.Kind) {
		return fmt.Errorf("kind %q isn't %s", *change.Kind, strings.Join(Kinds, ", "))
	}
	if change.Cap != nil && (*change.Cap < 0 || *change.Cap > MaximumCap) {
		return fmt.Errorf("cap %d isn't a whole number from 0 to %d", *change.Cap, MaximumCap)
	}
	if change.Tier != nil && (*change.Tier < 0 || *change.Tier > 1000) {
		return fmt.Errorf("tier %d isn't 0 to 1000", *change.Tier)
	}
	if strings.TrimSpace(change.By) == "" {
		return errors.New("a change names who made it")
	}
	return nil
}

func contains(list []string, item string) bool {
	for _, value := range list {
		if value == item {
			return true
		}
	}
	return false
}

// A Client talks to loom's fleet routes: reads with any token it's given, writes with a coordinator token.
type Client struct {
	Pipeline string
	HTTP     *http.Client
	Token    func() string
}

// NewClient mints its own coordinator tokens from the secret, as the owner bridge does.
func NewClient(pipeline string, secret []byte, run string) *Client {
	return &Client{Pipeline: strings.TrimSuffix(pipeline, "/"), HTTP: http.DefaultClient, Token: func() string {
		token, _ := protocol.MintToken(secret, protocol.TokenClaims{Run: run, Scope: protocol.ScopeCoordinator, Expires: nowPlusHour()})
		return token
	}}
}

func (client *Client) call(callContext context.Context, method string, path string, body any) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(callContext, method, client.Pipeline+path, reader)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+client.Token())
	request.Header.Set("Content-Type", "application/json")
	response, err := client.HTTP.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, response.StatusCode, err
	}
	if response.StatusCode/100 != 2 {
		return answer, response.StatusCode, fmt.Errorf("%s %s: %d %s", method, path, response.StatusCode, bytes.TrimSpace(answer))
	}
	return answer, response.StatusCode, nil
}

// List is every source with its cap and latest counts.
func (client *Client) List(callContext context.Context) ([]Source, error) {
	answer, _, err := client.call(callContext, http.MethodGet, "/fleet", nil)
	if err != nil {
		return nil, err
	}
	var listed struct {
		Sources []Source `json:"sources"`
	}
	if err := protocol.Decode(bytes.NewReader(answer), &listed); err != nil {
		return nil, fmt.Errorf("the fleet: %w", err)
	}
	return listed.Sources, nil
}

// Set changes a source (and registers it, given a kind), after checking the change here first.
func (client *Client) Set(callContext context.Context, name string, change Change) (Source, error) {
	if err := CheckName(name); err != nil {
		return Source{}, err
	}
	if err := change.Check(); err != nil {
		return Source{}, err
	}
	answer, _, err := client.call(callContext, http.MethodPost, "/fleet/sources/"+url.PathEscape(name), change)
	if err != nil {
		return Source{}, err
	}
	var set struct {
		Source Source `json:"source"`
	}
	if err := protocol.Decode(bytes.NewReader(answer), &set); err != nil {
		return Source{}, fmt.Errorf("source %s: %w", name, err)
	}
	return set.Source, nil
}

// PostCounts is an arm's one write per pass.
func (client *Client) PostCounts(callContext context.Context, name string, counts Counts) error {
	if err := CheckName(name); err != nil {
		return err
	}
	counts.At = ""
	_, _, err := client.call(callContext, http.MethodPost, "/fleet/sources/"+url.PathEscape(name)+"/counts", counts)
	return err
}

// CapOf is the cap an arm honors this pass: the source's, when loom answers and the source is on (an off
// source's cap is 0); else the number in the fallback file; else def. It says which it used, for the arm's log line.
func (client *Client) CapOf(callContext context.Context, name string, fallbackFile string, def int) (int, string) {
	if sources, err := client.List(callContext); err == nil {
		for _, source := range sources {
			if source.Name == name {
				if !source.On {
					return 0, fmt.Sprintf("loom (%s is off, seq %d by %s)", name, source.Changed.Seq, source.Changed.By)
				}
				return source.Cap, fmt.Sprintf("loom (seq %d by %s)", source.Changed.Seq, source.Changed.By)
			}
		}
	}
	if text, err := os.ReadFile(fallbackFile); err == nil {
		if value, err := strconv.Atoi(strings.TrimSpace(string(text))); err == nil && value >= 0 && value <= MaximumCap {
			return value, fallbackFile
		}
	}
	return def, "the default"
}

func nowPlusHour() int64 { return time.Now().Add(time.Hour).Unix() }
