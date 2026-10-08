// Command adamic-gate is the gate pilot (#lmpilot): Adamic's whole Go test set run as Loom units beside the
// whole gate, and the two compared test by test.
//
//	go run ./pilots/adamic-gate plan --reference test.jsonl.gz --sha <main sha> [--units 10] [--only <re>] > job.json
//	go run ./pilots/adamic-gate compare --reference test.jsonl.gz --job job.json --record run.jsonl [--only <re>]
//
// The reference is the whole gate's test.jsonl.gz for the same sha (gate-logs/<sha12>/<stamp>/full-main).
// plan bin-packs its top-level tests into units, longest first by the reference's own seconds; each unit runs
// its tests package by package on the slot's own warm checkout of the sha with the gate's environment.
// compare proves the union of the units is the reference's test set less the named exclusions, then checks
// every test's verdict against the reference.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/system-inc/loom/protocol"
)

const module = "github.com/system-inc/adamic/"

// exclusions are tests the pilot leaves out, each with its reason; the union must equal the reference less these.
var exclusions = map[string]string{
	module + "internal/native TestWASI": "it needs the wasm SDK's clang first, which v1 doesn't set up",
}

// A unit's script is an opening, which readies a tree at the sha with the whole gate's environment, then
// unitBody. boxOpening uses the slot's own warm checkout on our boxes (tree-N, safe because the unit holds that
// slot's lock). Its arguments are the sha and one "<package>=<-run pattern>" per package.
const boxOpening = `set -uo pipefail
sha=$1; shift
started=${SECONDS}
suffix=$([ "${LOOM_SLOT:-1}" = 1 ] && echo "" || echo "-${LOOM_SLOT}")
tree=${HOME}/fast-gate/tree${suffix} tools=${HOME}/fast-gate/tools${suffix} out=${PWD}/loom-out
mkdir -p "${out}"
[ -f "${tools}/cloud/idle-preempt.sh" ] && bash "${tools}/cloud/idle-preempt.sh"
source "${HOME}/adamic-tools/env.sh"
export PATH="${ADAMIC_TYPESCRIPT_SOURCE:+${ADAMIC_TYPESCRIPT_SOURCE}/bin:}${HOME}/fast-gate/npm/bin:${PATH}"
mkdir -p -m 1777 "${TMPDIR:-/tmp}"
find "${tree}/.git" -maxdepth 6 -name index.lock -delete 2>/dev/null
git -C "${tree}" fetch -q origin "${sha}" && git -C "${tree}" switch -q --detach "${sha}" && git -C "${tree}" submodule update -q --init --recursive || { echo "loom-pilot: checkout of ${sha} failed"; exit 2; }
`

// codexOpening is the same unit on a Codex cloud instance: a public clone at /tmp/adamic, kept between units
// and turns, and the gate's own toolchain from cloud/setup.sh, made once per instance under a marker.
const codexOpening = `set -uo pipefail
sha=$1; shift
started=${SECONDS}
# A Codex instance reaches the internet through a proxy its own environment names; a runner from before the
# runner passed those through hands the unit none, so take them from the runner, the unit's parent.
[ -n "${HTTPS_PROXY:-}${https_proxy:-}" ] || eval "$(tr '\0' '\n' < /proc/${PPID}/environ 2>/dev/null | grep -E '^(HTTPS?_PROXY|https?_proxy|NO_PROXY|no_proxy|ALL_PROXY|all_proxy|SSL_CERT_FILE|SSL_CERT_DIR|NODE_EXTRA_CA_CERTS|REQUESTS_CA_BUNDLE|GIT_SSL_CAINFO)=' | sed -E "s/^([^=]+)=(.*)$/export \\1='\\2'/")"
tree=/tmp/adamic out=${PWD}/loom-out
mkdir -p "${out}"
# Submodules are recorded over ssh; a cloud instance reaches GitHub over HTTPS only.
git config --global url."https://github.com/".insteadOf git@github.com:
[ -d "${tree}/.git" ] || git clone -q --filter=blob:none https://github.com/system-inc/adamic.git "${tree}" || { echo "loom-pilot: clone failed"; exit 2; }
find "${tree}/.git" -maxdepth 6 -name index.lock -delete 2>/dev/null
git -C "${tree}" fetch -q origin "${sha}" && git -C "${tree}" switch -q --detach "${sha}" && git -C "${tree}" submodule update -q --init --recursive || { echo "loom-pilot: checkout of ${sha} failed"; exit 2; }
if [ ! -f /tmp/adamic-setup-done ]; then
  (cd "${tree}" && bash cloud/setup.sh --wasi-sdk > /tmp/adamic-setup.log 2>&1) && touch /tmp/adamic-setup-done || { echo "loom-pilot: setup failed"; tail -20 /tmp/adamic-setup.log; exit 2; }
fi
for environment in "${HOME}/adamic-tools/env.sh" "${HOME}/.adamic-tools/env.sh"; do [ -f "${environment}" ] && { source "${environment}"; break; }; done
export PATH="${ADAMIC_TYPESCRIPT_SOURCE:+${ADAMIC_TYPESCRIPT_SOURCE}/bin:}${PATH}"
mkdir -p -m 1777 "${TMPDIR:-/tmp}"
`

// unitBody runs a unit's packages on the tree its opening readied, half the CPUs running test binaries.
const unitBody = `export ADAMIC_GATE_UNCACHED=1 ADAMIC_TEST_WASI=1 ADAMIC_ORACLE_WASI=1 ADAMIC_GATE_COHERE=1
parallel=$(( $(nproc) / 2 )); [ "${parallel}" -ge 1 ] || parallel=1
echo "loom-pilot: $(hostname) slot ${LOOM_SLOT:-?} cpus ${LOOM_SLOT_CPUS:-?} tree $(git -C "${tree}" rev-parse HEAD) setup $(( SECONDS - started )) s, ${parallel} packages at a time"
export tree out
status=0
index=0
for spec in "$@"; do printf '%s\t%s\n' "${index}" "${spec}"; index=$((index + 1)); done |
  xargs -d '\n' -P "${parallel}" -I{} bash -c '
    index=${1%%	*} spec=${1#*	}
    package=${spec%%=*} pattern=${spec#*=}
    cd "${tree}" && go test -count=1 -json -timeout 3h -run "${pattern}" "${package}" > "${out}/part-${index}.jsonl" 2> "${out}/part-${index}.stderr"
    code=$?
    [ "${code}" = 0 ] || { echo "loom-pilot: ${package} exited ${code}"; tail -5 "${out}/part-${index}.stderr"; }
    exit "${code}"' _ {} || status=1
cat "${out}"/part-*.jsonl | gzip -9 > "${out}/test.jsonl.gz"
grep -h '"Action":"fail"' "${out}"/part-*.jsonl | grep -o '"Package":"[^"]*","Test":"[^"/]*"' | sort -u | sed 's/^/loom-pilot: failed /'
echo "loom-pilot: tests took $(( SECONDS - started )) s in all"
exit "${status}"
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: adamic-gate plan|compare ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "plan":
		err = plan(os.Args[2:])
	case "compare":
		var parity bool
		parity, err = compare(os.Args[2:])
		if err == nil && !parity {
			os.Exit(1)
		}
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "adamic-gate: %v\n", err)
		os.Exit(2)
	}
}

// A result is one top-level test's terminal action and its seconds.
type result struct {
	action  string
	seconds float64
}

// readTests reads go test -json lines (gzipped or not) into each top-level test's last terminal action,
// keyed "<package> <test>", and counts how many terminal actions each test had.
func readTests(reader io.Reader) (map[string]result, map[string]int, error) {
	buffered := bufio.NewReader(reader)
	if magic, err := buffered.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		decompressor, err := gzip.NewReader(buffered)
		if err != nil {
			return nil, nil, err
		}
		buffered = bufio.NewReader(decompressor)
	}
	results := map[string]result{}
	counts := map[string]int{}
	scanner := bufio.NewScanner(buffered)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for scanner.Scan() {
		var event struct {
			Action  string
			Package string
			Test    string
			Elapsed float64
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Test == "" || strings.Contains(event.Test, "/") {
			continue
		}
		if event.Action != "pass" && event.Action != "fail" && event.Action != "skip" {
			continue
		}
		key := event.Package + " " + event.Test
		results[key] = result{action: event.Action, seconds: event.Elapsed}
		counts[key]++
	}
	return results, counts, scanner.Err()
}

// readTestsFile reads the reference, keeping only packages that match only (a regular expression; empty
// keeps all), so a small trial can stand in for the whole set.
func readTestsFile(path string, only string) (map[string]result, error) {
	pattern, err := regexp.Compile(only)
	if err != nil {
		return nil, fmt.Errorf("--only: %w", err)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	results, _, err := readTests(file)
	for key := range results {
		packageName, _, _ := strings.Cut(key, " ")
		if !pattern.MatchString(packageName) {
			delete(results, key)
		}
	}
	return results, err
}

func plan(arguments []string) error {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	referencePath := flags.String("reference", "", "the whole gate's test.jsonl.gz for the same sha")
	sha := flags.String("sha", "", "the main commit to test, the reference's own")
	units := flags.Int("units", 10, "how many units")
	only := flags.String("only", "", "plan only packages matching this regular expression, for a trial")
	target := flags.String("target", "box", "where the units run: box (a gate slot's warm tree) or codex (a Codex instance)")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(*sha) || *referencePath == "" || *units < 1 {
		return fmt.Errorf("plan needs --reference, a full 40-character --sha and --units of at least 1")
	}
	reference, err := readTestsFile(*referencePath, *only)
	if err != nil {
		return err
	}
	opening := boxOpening
	if *target == "codex" {
		opening = codexOpening
	} else if *target != "box" {
		return fmt.Errorf("--target is box or codex")
	}
	type test struct {
		key     string
		seconds float64
	}
	var tests []test
	for key, outcome := range reference {
		if _, excluded := exclusions[key]; !excluded {
			tests = append(tests, test{key, outcome.seconds})
		}
	}
	// Longest first, each onto the unit with the least so far: the classic greedy packing.
	sort.Slice(tests, func(left, right int) bool {
		if tests[left].seconds != tests[right].seconds {
			return tests[left].seconds > tests[right].seconds
		}
		return tests[left].key < tests[right].key
	})
	loads := make([]float64, *units)
	assigned := make([]map[string][]string, *units) // package to test names
	for index := range assigned {
		assigned[index] = map[string][]string{}
	}
	for _, test := range tests {
		lightest := 0
		for index := range loads {
			if loads[index] < loads[lightest] {
				lightest = index
			}
		}
		loads[lightest] += test.seconds
		packageName, name, _ := strings.Cut(test.key, " ")
		assigned[lightest][packageName] = append(assigned[lightest][packageName], name)
	}
	job := protocol.Job{Name: "adamic-gate-pilot"}
	for index, packages := range assigned {
		if len(packages) == 0 {
			continue
		}
		argv := []string{"bash", "-c", opening + unitBody, "adamic-gate-unit", *sha}
		names := make([]string, 0, len(packages))
		for packageName := range packages {
			names = append(names, packageName)
		}
		sort.Strings(names)
		for _, packageName := range names {
			tests := packages[packageName]
			sort.Strings(tests)
			quoted := make([]string, len(tests))
			for position, name := range tests {
				quoted[position] = regexp.QuoteMeta(name)
			}
			argv = append(argv, packageName+"=^("+strings.Join(quoted, "|")+")$")
		}
		job.Units = append(job.Units, protocol.JobUnit{
			Id: fmt.Sprintf("tests-%02d", index), Argv: argv, TimeoutSeconds: 3*3600 + 600,
			Outputs:   []protocol.Output{{Glob: "loom-out/test.jsonl.gz"}},
			Resources: protocol.Resources{Cpus: 12},
		})
		fmt.Fprintf(os.Stderr, "tests-%02d: %d packages, %.0f s by the reference\n", index, len(packages), loads[index])
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(job)
}

// plannedTests reads the job back into each unit's "<package> <test>" keys, from its argv.
func plannedTests(job protocol.Job) (map[string][]string, error) {
	planned := map[string][]string{}
	for _, unit := range job.Units {
		if len(unit.Argv) < 5 {
			return nil, fmt.Errorf("unit %s isn't a pilot unit", unit.Id)
		}
		for _, spec := range unit.Argv[5:] {
			packageName, pattern, _ := strings.Cut(spec, "=")
			inner := strings.TrimSuffix(strings.TrimPrefix(pattern, "^("), ")$")
			for _, quoted := range strings.Split(inner, "|") {
				planned[unit.Id] = append(planned[unit.Id], packageName+" "+strings.ReplaceAll(quoted, `\`, ""))
			}
		}
	}
	return planned, nil
}

func compare(arguments []string) (bool, error) {
	flags := flag.NewFlagSet("compare", flag.ContinueOnError)
	referencePath := flags.String("reference", "", "the whole gate's test.jsonl.gz for the same sha")
	jobPath := flags.String("job", "", "the job file plan wrote")
	recordPath := flags.String("record", "", "the run's record, from loom run --record")
	wire := flags.String("wire", "https://loom-wire.kirk-ouimet.workers.dev", "the wire's origin")
	only := flags.String("only", "", "compare only packages matching this regular expression, as plan did")
	if err := flags.Parse(arguments); err != nil {
		return false, err
	}
	reference, err := readTestsFile(*referencePath, *only)
	if err != nil {
		return false, err
	}
	jobFile, err := os.Open(*jobPath)
	if err != nil {
		return false, err
	}
	var job protocol.Job
	err = protocol.Decode(jobFile, &job)
	jobFile.Close()
	if err != nil {
		return false, err
	}
	planned, err := plannedTests(job)
	if err != nil {
		return false, err
	}
	run, verdict, events, err := readRecord(*recordPath)
	if err != nil {
		return false, err
	}
	parity := true
	report := func(format string, arguments ...any) { fmt.Printf(format+"\n", arguments...) }
	report("run %s: verdict %s", run, verdict.Status)

	// The plan's union against the reference less the exclusions.
	plannedSet := map[string]string{}
	for unit, keys := range planned {
		for _, key := range keys {
			if other, twice := plannedSet[key]; twice {
				report("PLAN: %s is in both %s and %s", key, other, unit)
				parity = false
			}
			plannedSet[key] = unit
		}
	}
	for key := range reference {
		if _, excluded := exclusions[key]; !excluded && plannedSet[key] == "" {
			report("PLAN: the reference ran %s and no unit has it", key)
			parity = false
		}
	}
	for key := range plannedSet {
		if _, ran := reference[key]; !ran {
			report("PLAN: %s is planned but the reference never ran it", key)
			parity = false
		}
	}
	for key, why := range exclusions {
		if _, inReference := reference[key]; inReference {
			report("excluded: %s (%s)", key, why)
		}
	}
	if *only != "" {
		report("only packages matching %s, a trial", *only)
	}

	// Each unit's own results, fetched from the store by the hash its uploaded event names.
	home, _ := os.UserHomeDir()
	secret, err := protocol.ReadTokenSecret(filepath.Join(home, ".loom", "token-secret"))
	if err != nil {
		return false, err
	}
	token, err := protocol.MintToken(secret, protocol.TokenClaims{Run: run, Scope: protocol.ScopeCoordinator, Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		return false, err
	}
	type unitTiming struct {
		unit        string
		wall, setup float64
		machine     string
	}
	var timings []unitTiming
	loom := map[string]result{}
	seen := map[string]string{}
	var first, last time.Time
	setupPattern := regexp.MustCompile(`setup (\d+) s`)
	for _, unit := range job.Units {
		timing := unitTiming{unit: unit.Id}
		output := ""
		for _, event := range events {
			if event.Unit != unit.Id {
				continue
			}
			if at, err := time.Parse(time.RFC3339Nano, event.Time); err == nil {
				if first.IsZero() || at.Before(first) {
					first = at
				}
				if at.After(last) {
					last = at
				}
			}
			switch event.Type {
			case "started":
				timing.machine = event.Machine
			case "exit":
				timing.wall = event.WallSeconds
			case "output":
				if match := setupPattern.FindStringSubmatch(event.Text); match != nil {
					timing.setup, _ = strconv.ParseFloat(match[1], 64)
				}
			case "uploaded":
				if event.Path == "loom-out/test.jsonl.gz" {
					output = event.Sha256
				}
			}
		}
		timings = append(timings, timing)
		if output == "" {
			report("UNIT %s: no test.jsonl.gz uploaded; its tests are missing", unit.Id)
			parity = false
			continue
		}
		content, err := fetchBlob(*wire, run, token, output)
		if err != nil {
			return false, fmt.Errorf("unit %s's results: %w", unit.Id, err)
		}
		results, counts, err := readTests(bytes.NewReader(content))
		if err != nil {
			return false, fmt.Errorf("unit %s's results: %w", unit.Id, err)
		}
		mine := map[string]bool{}
		for _, key := range planned[unit.Id] {
			mine[key] = true
			if counts[key] != 1 {
				report("UNIT %s: %s reported %d times, want once", unit.Id, key, counts[key])
				parity = false
			}
		}
		for key, outcome := range results {
			if !mine[key] {
				report("UNIT %s: ran %s, which isn't its own", unit.Id, key)
				parity = false
				continue
			}
			if other, twice := seen[key]; twice {
				report("UNIT %s: %s also ran in %s", unit.Id, key, other)
				parity = false
			}
			seen[key] = unit.Id
			loom[key] = outcome
		}
	}

	// Every test's verdict both ways.
	agree, differ := 0, 0
	keys := make([]string, 0, len(plannedSet))
	for key := range plannedSet {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		want, got := reference[key], loom[key]
		if got.action == "" {
			continue // reported above as missing
		}
		if got.action == want.action {
			agree++
		} else {
			differ++
			parity = false
			report("DIFFER: %s: the whole gate %s, Loom %s", key, want.action, got.action)
		}
	}
	missing := len(keys) - agree - differ
	if missing > 0 {
		parity = false
	}
	report("tests: %d planned, %d agree, %d differ, %d missing from Loom", len(keys), agree, differ, missing)

	sort.Slice(timings, func(left, right int) bool { return timings[left].wall > timings[right].wall })
	for _, timing := range timings {
		report("  %s on %s: %.0f s, setup %.0f s", timing.unit, timing.machine, timing.wall, timing.setup)
	}
	if !first.IsZero() {
		report("wall: %.0f s from the first event to the last", last.Sub(first).Seconds())
	}
	if parity {
		report("PARITY: every planned test has the same verdict both ways, and the units' union is the reference less the exclusions")
	}
	return parity, nil
}

func readRecord(path string) (string, protocol.Verdict, []protocol.Event, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", protocol.Verdict{}, nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	var header struct {
		Run     string
		Verdict protocol.Verdict
	}
	if !scanner.Scan() || json.Unmarshal(scanner.Bytes(), &header) != nil || header.Run == "" {
		return "", protocol.Verdict{}, nil, fmt.Errorf("%s isn't a loom run --record file", path)
	}
	var events []protocol.Event
	for scanner.Scan() {
		var event protocol.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return "", protocol.Verdict{}, nil, err
		}
		events = append(events, event)
	}
	return header.Run, header.Verdict, events, scanner.Err()
}

func fetchBlob(wire string, run string, token string, sha256 string) ([]byte, error) {
	request, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(wire, "/")+"/runs/"+run+"/blobs/"+sha256, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET blob %s: %s", sha256, response.Status)
	}
	return io.ReadAll(response.Body)
}
