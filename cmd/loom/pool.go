package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/system-inc/loom/protocol"
)

// poolPlatform is what a pool's workers run on: Codex cloud instances are linux/amd64.
const poolPlatform = "linux/amd64"

// A poolSlots is one --pool flag: a pool's name and how many of its units may run at once.
type poolSlots struct {
	name  string
	slots int
}

// poolSlotsFlag collects every --pool <name>=<slots>.
type poolSlotsFlag []poolSlots

func (pools *poolSlotsFlag) String() string {
	var parts []string
	for _, pool := range *pools {
		parts = append(parts, pool.name+"="+strconv.Itoa(pool.slots))
	}
	return strings.Join(parts, " ")
}

func (pools *poolSlotsFlag) Set(text string) error {
	name, count, found := strings.Cut(text, "=")
	slots, err := strconv.Atoi(count)
	// A pool's name is the run a pool token names, so it must be what the wire takes as a run id.
	if !found || !protocol.RunIdPattern.MatchString(name) || err != nil || slots < 1 {
		return fmt.Errorf("%q isn't <name>=<slots>, such as codex=4", text)
	}
	*pools = append(*pools, poolSlots{name: name, slots: slots})
	return nil
}

// poolStatus is what GET /pools/<pool> answers: the queue's length and every worker seen in the last ten
// minutes. took is the unit a worker last took, shown as the wire gives it.
type poolStatus struct {
	Queued  int `json:"queued"`
	Workers []struct {
		Worker string          `json:"worker"`
		Cpus   int             `json:"cpus"`
		SeenAt string          `json:"seenAt"`
		Took   json.RawMessage `json:"took"`
	} `json:"workers"`
}

// pool handles `loom pool status <name>`, read with a board token, which watches and changes nothing.
func pool(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) == 0 || arguments[0] != "status" {
		fmt.Fprint(stderr, usage)
		return 3
	}
	flags := flag.NewFlagSet("pool status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	wire := flags.String("wire", "https://loom-wire.kirk-ouimet.workers.dev", "the wire's origin")
	if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 1 {
		fmt.Fprint(stderr, usage)
		return 3
	}
	home, _ := os.UserHomeDir()
	secret, err := protocol.ReadTokenSecret(filepath.Join(home, ".loom", "token-secret"))
	if err != nil {
		fmt.Fprintf(stderr, "loom: %v\n", err)
		return 3
	}
	status, err := readPoolStatus(&http.Client{Timeout: 15 * time.Second}, *wire, secret, flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "loom: %v\n", err)
		return 3
	}
	writePoolStatus(stdout, flags.Arg(0), status, time.Now())
	return 0
}

func readPoolStatus(client *http.Client, wire string, secret []byte, name string) (poolStatus, error) {
	var status poolStatus
	token, err := protocol.MintToken(secret, protocol.TokenClaims{Run: protocol.BoardRun, Scope: protocol.ScopeBoard, Expires: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		return status, err
	}
	url := strings.TrimSuffix(wire, "/") + "/pools/" + name
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return status, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		return status, err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if response.StatusCode != http.StatusOK {
		return status, fmt.Errorf("GET %s: %s %s", url, response.Status, bytes.TrimSpace(body))
	}
	if err := protocol.Decode(bytes.NewReader(body), &status); err != nil {
		return status, fmt.Errorf("pool %s's status: %w", name, err)
	}
	return status, nil
}

// writePoolStatus prints a line for the pool, then one per worker: its CPUs, how long since it last asked,
// and the unit it last took.
func writePoolStatus(writer io.Writer, name string, status poolStatus, now time.Time) {
	fmt.Fprintf(writer, "pool %s: %d queued, %d workers\n", name, status.Queued, len(status.Workers))
	for _, worker := range status.Workers {
		seen := worker.SeenAt
		if at, err := time.Parse(time.RFC3339, worker.SeenAt); err == nil {
			seen = fmt.Sprintf("%.0f s ago", now.Sub(at).Seconds())
		}
		took := "nothing yet"
		var unit string
		if json.Unmarshal(worker.Took, &unit) == nil && unit != "" {
			took = unit
		} else if len(worker.Took) > 0 && string(worker.Took) != "null" {
			took = string(worker.Took)
		}
		fmt.Fprintf(writer, "  %s  %d cpus  asked %s  took %s\n", worker.Worker, worker.Cpus, seen, took)
	}
}
