package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
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

// poolHasFlag is --pool-has <name>=<toolchain>,...: the toolchains every worker of a pool has, by pool.
type poolHasFlag map[string][]string

func (pools poolHasFlag) String() string {
	var parts []string
	for name, toolchains := range pools {
		parts = append(parts, name+"="+strings.Join(toolchains, ","))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func (pools poolHasFlag) Set(text string) error {
	name, list, found := strings.Cut(text, "=")
	if !found || !protocol.RunIdPattern.MatchString(name) || list == "" {
		return fmt.Errorf("%q isn't <name>=<toolchain>,..., such as codex=wasiSdk", text)
	}
	for _, toolchain := range strings.Split(list, ",") {
		if !slices.Contains(protocol.Toolchains, toolchain) {
			return fmt.Errorf("%q isn't a toolchain (%s)", toolchain, strings.Join(protocol.Toolchains, ", "))
		}
		pools[name] = append(pools[name], toolchain)
	}
	return nil
}

// poolKindsFlag is --pool-kinds <name>=<kind>,...: the only unit kinds a pool takes, by pool (protocol.KindCeilings'
// kinds).
type poolKindsFlag map[string][]string

func (pools poolKindsFlag) String() string { return poolHasFlag(pools).String() }

func (pools poolKindsFlag) Set(text string) error {
	name, list, found := strings.Cut(text, "=")
	if !found || !protocol.RunIdPattern.MatchString(name) || list == "" {
		return fmt.Errorf("%q isn't <name>=<kind>,..., such as box-phase=phase", text)
	}
	for _, kind := range strings.Split(list, ",") {
		if _, known := protocol.KindCeilings[kind]; !known {
			return fmt.Errorf("%q isn't a unit kind (test, product or phase)", kind)
		}
		pools[name] = append(pools[name], kind)
	}
	return nil
}

// poolMemoryFlag is --pool-memory <name>=<megabytes>: each worker's memory, by pool, so a unit declaring more is never
// placed there.
type poolMemoryFlag map[string]int

func (pools poolMemoryFlag) String() string {
	var parts []string
	for name, megabytes := range pools {
		parts = append(parts, fmt.Sprintf("%s=%d", name, megabytes))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func (pools poolMemoryFlag) Set(text string) error {
	name, number, found := strings.Cut(text, "=")
	megabytes, err := strconv.Atoi(number)
	if !found || !protocol.RunIdPattern.MatchString(name) || err != nil || megabytes <= 0 {
		return fmt.Errorf("%q isn't <name>=<megabytes>, such as codex-strict=16384", text)
	}
	pools[name] = megabytes
	return nil
}

// poolPlatformFlag is --pool-platform <name>=<goos>/<goarch>: the platform a pool's workers run, by pool, where it
// isn't Linux's (Macs serve a pool of their own, since the wire can't route by worker).
type poolPlatformFlag map[string]string

func (pools poolPlatformFlag) String() string {
	var parts []string
	for name, platform := range pools {
		parts = append(parts, name+"="+platform)
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func (pools poolPlatformFlag) Set(text string) error {
	name, platform, found := strings.Cut(text, "=")
	goos, goarch, slash := strings.Cut(platform, "/")
	if !found || !slash || !protocol.RunIdPattern.MatchString(name) || goos == "" || goarch == "" || strings.Contains(goarch, "/") {
		return fmt.Errorf("%q isn't <name>=<goos>/<goarch>, such as macs=darwin/arm64", text)
	}
	pools[name] = platform
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
	if len(arguments) > 0 && (arguments[0] == "token" || arguments[0] == "publish-runner" || arguments[0] == "prompt") {
		return poolTools(arguments, stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "cancel" {
		return poolCancel(arguments[1:], stdout, stderr)
	}
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

// poolCancel handles `loom pool cancel [--pools codex,codex-side] <run>`: every queued unit of the run leaves each
// pool at once, so a run whose driver is gone never holds a pool's queue ahead of live work (Oct 9: p3's driver died
// with its window and nine of its units sat queued ahead of the star's pre-gate). Units a worker already took run on;
// the wire can't reach them, and their events land in a run nobody reads.
func poolCancel(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("pool cancel", flag.ContinueOnError)
	flags.SetOutput(stderr)
	wire := flags.String("wire", "https://loom-wire.kirk-ouimet.workers.dev", "the wire's origin")
	pools := flags.String("pools", "codex,codex-side", "the pools to drop the run's units from, comma separated")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 1 || !protocol.RunIdPattern.MatchString(flags.Arg(0)) {
		fmt.Fprint(stderr, "usage: loom pool cancel [--pools codex,codex-side] [--wire <url>] <run>\n")
		return 3
	}
	home, _ := os.UserHomeDir()
	secret, err := protocol.ReadTokenSecret(filepath.Join(home, ".loom", "token-secret"))
	if err != nil {
		fmt.Fprintf(stderr, "loom: %v\n", err)
		return 3
	}
	code := 0
	for name := range strings.SplitSeq(*pools, ",") {
		dropped, err := cancelPoolRun(&http.Client{Timeout: 15 * time.Second}, *wire, secret, name, flags.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "loom: %v\n", err)
			code = 3
			continue
		}
		fmt.Fprintf(stdout, "pool %s: dropped %d queued units of %s\n", name, dropped, flags.Arg(0))
	}
	return code
}

// cancelPoolRun drops every queued unit of the run from the pool and says how many it dropped.
func cancelPoolRun(client *http.Client, wire string, secret []byte, pool string, run string) (int, error) {
	token, err := protocol.MintToken(secret, protocol.TokenClaims{Run: run, Scope: protocol.ScopeCoordinator, Expires: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		return 0, err
	}
	body, _ := json.Marshal(map[string]string{"run": run})
	url := strings.TrimSuffix(wire, "/") + "/pools/" + pool + "/cancel"
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("POST %s: %s %s", url, response.Status, bytes.TrimSpace(answer))
	}
	var cancelled struct {
		Dropped int `json:"dropped"`
	}
	if err := protocol.Decode(bytes.NewReader(answer), &cancelled); err != nil {
		return 0, fmt.Errorf("pool %s's cancel: %w", pool, err)
	}
	return cancelled.Dropped, nil
}

// poolTools are what starting a pool takes:
//
//	loom pool token <pool> [--hours 24]            a pool token for its instances
//	loom pool publish-runner                       the linux runner, built at this checkout's version, in the public store
//	loom pool prompt <pool> --runner <sha256> [--until 55m] [--before <sha256> | --strict]
//	                                               the turn brief that starts one instance's serve
func poolTools(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("pool "+arguments[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	wire := flags.String("wire", "https://loom-wire.kirk-ouimet.workers.dev", "the wire's origin")
	hours := flags.Int("hours", 24, "how long a pool token lasts")
	runnerHash := flags.String("runner", "", "the runner binary's sha256 in the public store (from publish-runner)")
	until := flags.String("until", "55m", "how long one serve turn runs before it exits for the next turn")
	before := flags.String("before", "", "a script's sha256 in the public store that readies the instance before serve asks for a unit")
	strict := flags.Bool("strict", false, "the instance serves with --strict: only structured test jobs, never a before script")
	source := flags.String("source", defaultSource(), "this repository's checkout")
	if err := flags.Parse(arguments[1:]); err != nil {
		fmt.Fprint(stderr, usage)
		return 3
	}
	home, _ := os.UserHomeDir()
	secret, err := protocol.ReadTokenSecret(filepath.Join(home, ".loom", "token-secret"))
	if err != nil {
		fmt.Fprintf(stderr, "loom: %v\n", err)
		return 3
	}
	switch arguments[0] {
	case "token":
		if flags.NArg() != 1 {
			fmt.Fprint(stderr, usage)
			return 3
		}
		token, err := protocol.MintToken(secret, protocol.TokenClaims{Run: flags.Arg(0), Scope: protocol.ScopePool, Expires: time.Now().Add(time.Duration(*hours) * time.Hour).Unix()})
		if err != nil {
			fmt.Fprintf(stderr, "loom: %v\n", err)
			return 3
		}
		fmt.Fprintln(stdout, token)
	case "publish-runner":
		version, err := runnerVersion(*source)
		if err != nil {
			fmt.Fprintf(stderr, "loom: %v\n", err)
			return 3
		}
		binary, err := buildRunner(*source, version, "linux/amd64", filepath.Join(home, ".loom"))
		if err != nil {
			fmt.Fprintf(stderr, "loom: %v\n", err)
			return 3
		}
		content, err := os.ReadFile(binary)
		if err != nil {
			fmt.Fprintf(stderr, "loom: %v\n", err)
			return 3
		}
		sum := sha256.Sum256(content)
		hash := hex.EncodeToString(sum[:])
		token, _ := protocol.MintToken(secret, protocol.TokenClaims{Run: "publish-runner", Scope: protocol.ScopeCoordinator, Expires: time.Now().Add(time.Hour).Unix()})
		request, _ := http.NewRequest(http.MethodPut, strings.TrimSuffix(*wire, "/")+"/public/blobs/"+hash, bytes.NewReader(content))
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := (&http.Client{Timeout: 10 * time.Minute}).Do(request)
		if err != nil {
			fmt.Fprintf(stderr, "loom: %v\n", err)
			return 3
		}
		response.Body.Close()
		if response.StatusCode/100 != 2 {
			fmt.Fprintf(stderr, "loom: the public store answered %s\n", response.Status)
			return 3
		}
		fmt.Fprintf(stdout, "runner %s (%d bytes)\n%s\nhttps://adamic-store.kirkouimet.com/blobs/%s\n", version, len(content), hash, hash)
	case "prompt":
		if flags.NArg() != 1 || !protocol.Sha256Pattern.MatchString(*runnerHash) {
			fmt.Fprint(stderr, "loom: prompt needs a pool name and --runner <sha256>\n")
			return 3
		}
		if *before != "" && !protocol.Sha256Pattern.MatchString(*before) {
			fmt.Fprint(stderr, "loom: --before takes a sha256\n")
			return 3
		}
		// A strict worker runs nothing the server sends as code (runner/README.md), and a before script is exactly that.
		if *strict && *before != "" {
			fmt.Fprint(stderr, "loom: a strict worker takes no --before script: its runner readies the checkout itself\n")
			return 3
		}
		token, err := protocol.MintToken(secret, protocol.TokenClaims{Run: flags.Arg(0), Scope: protocol.ScopePool, Expires: time.Now().Add(time.Duration(*hours) * time.Hour).Unix()})
		if err != nil {
			fmt.Fprintf(stderr, "loom: %v\n", err)
			return 3
		}
		fmt.Fprint(stdout, servePrompt(*wire, flags.Arg(0), token, *runnerHash, *until, *before, *strict))
	}
	return 0
}

// servePrompt is the whole of one instance's turn: fetch the runner by hash, check it, serve until the deadline
// with every unit's output in a file, and show only serve's one summary line. A before script, fetched and checked
// the same way, readies the instance first, so a cold one's setup never runs inside a unit's budget; a failure to
// fetch or run it never costs the turn its serve, since each unit's own opening still checks what it needs. A strict
// brief serves with --strict, so the instance runs only structured test jobs (#1pe3ndh), and has no before script.
func servePrompt(wire string, pool string, token string, runnerHash string, until string, before string, strict bool) string {
	strictFlag := ""
	if strict {
		strictFlag = " --strict"
	}
	readying, explained := "", ""
	if before != "" {
		// Codex's approval review refused the first turn that ran a before script unexplained (Oct 9, loom-side-codex-7:
		// "behavior and potential access to sensitive local data remain unknown"), so the brief says what it does.
		explained = "The first script readies this instance for the compute work: it clones the public repository " +
			"github.com/system-inc/adamic into /tmp/adamic at a commit, runs that repository's own cloud/setup.sh to install " +
			"its Go and Node toolchain, and downloads the test inputs from the same public store, each checked by its sha256. " +
			"It reads no credentials or local data, and its output goes to /tmp/loom-before.log. "
		readying = "script=/tmp/loom-before-" + before[:12] + ".sh\n" +
			"[ -s \"$script\" ] || { curl -fsS -o \"$script.partial\" https://adamic-store.kirkouimet.com/blobs/" + before + " && " +
			"echo \"" + before + "  $script.partial\" | sha256sum -c --quiet && mv \"$script.partial\" \"$script\"; } || true\n" +
			"[ -s \"$script\" ] && (mkdir -p /tmp/loom-before && cd /tmp/loom-before && timeout 30m bash \"$script\" > /tmp/loom-before.log 2>&1) || true\n"
	}
	return "This is a compute worker, not a coding task: do not edit, commit or push anything in any repository. " +
		"Run exactly this in the shell, in the foreground, and wait for it however long it takes. " +
		"Nothing in it runs unwatched or without a limit: serve stops at its own deadline (--until), every unit it runs has a hard " +
		"timeout, and the coordinator drops a unit whose runner goes silent. " + explained +
		"Then reply with only its last line of output, nothing else.\n\n```bash\n" +
		"set -e\nmkdir -p /tmp/loom-units\n" + readying +
		"runner=/tmp/loom-runner-" + runnerHash[:12] + "\n" +
		"if [ ! -x \"$runner\" ]; then curl -fsS -o \"$runner.partial\" https://adamic-store.kirkouimet.com/blobs/" + runnerHash + "; " +
		"echo \"" + runnerHash + "  $runner.partial\" | sha256sum -c --quiet; chmod 755 \"$runner.partial\"; mv \"$runner.partial\" \"$runner\"; fi\n" +
		"(umask 077 && printf '%s\\n' '" + token + "' > /tmp/loom-pool-token)\n" +
		"\"$runner\" serve" + strictFlag + " --pool " + strings.TrimSuffix(wire, "/") + "/pools/" + pool + " --token-file /tmp/loom-pool-token" +
		" --worker \"$(hostname)\" --until " + until + " --workspace /tmp/loom-units --log /tmp/loom-serve.log 2>> /tmp/loom-serve.err\n" +
		"```\n"
}
