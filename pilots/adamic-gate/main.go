// Command adamic-gate is the gate pilot (#lmpilot): Adamic's whole Go test set run as Loom units beside the
// whole gate, and the two compared test by test.
//
//	go run ./pilots/adamic-gate plan --reference test.jsonl.gz --sha <main sha> [--units 10] [--only <re>] [--target codex --gate-inputs <manifest>] > job.json
//	go run ./pilots/adamic-gate warm --sha <main sha> [--units 25] [--gate-inputs <manifest>] > warm.json
//	go run ./pilots/adamic-gate compare --reference test.jsonl.gz --job job.json --record run.jsonl [--only <re>] [--times <file>]
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
# The pinned npm packages the tests read (stage3/api's @types/node and typescript), as the gate's npmPackages
# makes them: npm ci once per lockfile into a cache keyed by its hash, hardlinked into the tree under a marker.
lockfile="${tree}/stage3/api/package-lock.json"
if [ -f "${lockfile}" ]; then
  key=$(sha256sum "${lockfile}" | cut -c1-64)
  cache=/tmp/adamic-npm/${key} target="${tree}/stage3/api/node_modules"
  if [ ! -d "${cache}/node_modules" ]; then
    staging=${cache}.staging-$$
    mkdir -p "${staging}" && cp "${tree}/stage3/api/package.json" "${lockfile}" "${staging}/"
    (cd "${staging}" && npm_config_update_notifier=false npm ci --ignore-scripts --no-audit --no-fund --install-strategy=hoisted --registry=https://registry.npmjs.org > npm.log 2>&1) && mv "${staging}" "${cache}" || { echo "loom-pilot: npm ci of stage3/api failed"; tail -20 "${staging}/npm.log"; exit 2; }
  fi
  if [ "$(cat "${target}/.fast-gate-lockfile-sha256" 2> /dev/null)" != "${key}" ]; then
    [ -e "${target}" ] && mv "${target}" "/tmp/adamic-npm/replaced-$$-${SECONDS}"
    cp -al "${cache}/node_modules" "${target}" && echo "${key}" > "${target}/.fast-gate-lockfile-sha256"
  fi
fi
# The gate inputs a box's env.sh names (the pinned TypeScript, the checker archive, the formatters' libraries and
# corpora), made on a gate box and published by hash: a manifest of 90 MB chunks of one tar.gz and its total.
# Fetched once per instance, every hash checked, then exported exactly as the boxes export them.
tools=/tmp/adamic-tools inputs=/tmp/adamic-tools/gate-inputs
if [ -n "${gateInputs}" ] && [ "$(cat "${tools}/gate-inputs.manifest" 2> /dev/null)" != "${gateInputs}" ]; then
  fetch() { curl -fsS --retry 3 -o "$2" "https://adamic-store.kirkouimet.com/blobs/$1" && echo "$1  $2" | sha256sum -c --quiet; }
  staging=${tools}/staging-$$
  mkdir -p "${staging}" && fetch "${gateInputs}" "${staging}/manifest" || { echo "loom-pilot: gate inputs manifest ${gateInputs} unreadable"; exit 2; }
  for hash in $(grep -v '^total ' "${staging}/manifest"); do
    fetch "${hash}" "${staging}/part" && cat "${staging}/part" >> "${staging}/gate-inputs.tar.gz" || { echo "loom-pilot: gate inputs chunk ${hash} failed"; exit 2; }
  done
  echo "$(sed -n 's/^total //p' "${staging}/manifest")  ${staging}/gate-inputs.tar.gz" | sha256sum -c --quiet || { echo "loom-pilot: gate inputs total hash differs"; exit 2; }
  [ -e "${inputs}" ] && mv "${inputs}" "${staging}/replaced"
  tar -C "${tools}" -xzf "${staging}/gate-inputs.tar.gz" && echo "${gateInputs}" > "${tools}/gate-inputs.manifest" || { echo "loom-pilot: gate inputs unpack failed"; exit 2; }
  rm -f "${staging}/part" "${staging}/gate-inputs.tar.gz" "${staging}/manifest"
  rmdir "${staging}" 2> /dev/null
fi
if [ -n "${gateInputs}" ]; then
  export ADAMIC_TYPESCRIPT_SOURCE=${inputs}/typescript ADAMIC_CYCLE_LEDGER_ROOT=${inputs}/cycle-ledger
  export ADAMIC_CYCLE_LEDGER_OUTPUT=${inputs}/cycle-ledger-output.json ADAMIC_CSS_FIXTURES=${inputs}/css-fixtures
  export ADAMIC_CSSNUMBERS_LIBRARY=${inputs}/css-printer ADAMIC_CSSSTRINGS_LIBRARY=${inputs}/css-printer
  export ADAMIC_MARKDOWNINLINE_LIBRARY=${inputs}/css-printer/node_modules/prettier ADAMIC_CSS_LIBRARY=${inputs}/css
  export ADAMIC_GRAPHQL_LIBRARY=${inputs}/graphql ADAMIC_MEDIA_QUERY_LIBRARY=${inputs}/media-query
  export ADAMIC_SELECTOR_LIBRARY=${inputs}/selector ADAMIC_VALUES_LIBRARY=${inputs}/values
  export ADAMIC_GRAPHQL_PRETTIER=${inputs}/css-printer ADAMIC_JSON_PRETTIER=${inputs}/json-prettier
  export ADAMIC_CSS_PRINTER_LIBRARY=${inputs}/css-printer ADAMIC_ESTREE_LIBRARY=${inputs}/css-printer
  export ADAMIC_TS_PRETTIER=${inputs}/css-printer ADAMIC_YAML_LIBRARY=${inputs}/css-printer
  export ADAMIC_GITIGNORE_LARGEST=${inputs}/gitignore/.gitignore ADAMIC_CLANG_TSGO_ARCHIVE=${inputs}/checker/tsgo.a
  export PATH="${ADAMIC_TYPESCRIPT_SOURCE}/bin:${PATH}"
fi
# Every Go module's dependencies in the module cache first: a box's cache is warm from earlier gates, and tests
# that build a nested module (cohere's) read any "go: downloading" on stderr as a failure. Cached, this is a no-op.
git -C "${tree}" ls-files --recurse-submodules '*go.mod' | grep -v -e testdata/ -e node_modules/ | while read -r module; do
  (cd "${tree}/$(dirname "${module}")" && go mod download > /dev/null 2>&1)
done
`

// unitBody runs a unit's packages on the tree its opening readied, half the CPUs running test binaries. Each
// package is built and vetted first with nothing run (`-exec /bin/true`), and the shell's `times` after each
// step splits the package's CPU into build and test, written to cpu.tsv (package, build, test CPU-seconds).
// Build here is go's own compile, vet and link; a test that builds things inside itself counts as test.
const unitBody = `export ADAMIC_GATE_UNCACHED=1 ADAMIC_TEST_WASI=1 ADAMIC_ORACLE_WASI=1 ADAMIC_GATE_COHERE=1
parallel=$(( $(nproc) / 2 )); [ "${parallel}" -ge 1 ] || parallel=1
echo "loom-pilot: $(hostname) slot ${LOOM_SLOT:-?} cpus ${LOOM_SLOT_CPUS:-?} tree $(git -C "${tree}" rev-parse HEAD) setup $(( SECONDS - started )) s, ${parallel} packages at a time"
export tree out
status=0
index=0
for spec in "$@"; do printf '%s\t%s\n' "${index}" "${spec}"; index=$((index + 1)); done |
  xargs -d '\n' -P "${parallel}" -I{} bash -c '
    index=${1%%	*} spec=${1#*	}
    package=${spec%%=*} pattern=${spec#*=} skip=""
    case "${pattern}" in *" skip="*) skip=${pattern#* skip=} pattern=${pattern%% skip=*} ;; esac
    echo "${package}" > "${out}/part-${index}.package"
    cd "${tree}" && go test -count=1 -exec /bin/true -run "${pattern}" "${package}" > /dev/null 2>&1
    times > "${out}/part-${index}.build"
    go test -count=1 -json -timeout 3h -run "${pattern}" ${skip:+-skip "${skip}"} "${package}" > "${out}/part-${index}.jsonl" 2> "${out}/part-${index}.stderr"
    code=$?
    times > "${out}/part-${index}.total"
    [ "${code}" = 0 ] || { echo "loom-pilot: ${package} exited ${code}"; tail -5 "${out}/part-${index}.stderr"; }
    exit "${code}"' _ {} || status=1
cat "${out}"/part-*.jsonl | gzip -9 > "${out}/test.jsonl.gz"
# times prints the shell's user and system time, then its children's, as 1m2.345s; the children are go's.
for build in "${out}"/part-*.build; do
  part=${build%.build}
  awk -v package="$(cat "${part}.package")" 'FNR == 2 { split($1, userpart, "m"); split($2, systempart, "m"); seconds[FILENAME] = userpart[1] * 60 + userpart[2] + systempart[1] * 60 + systempart[2] } END { printf "%s\t%.2f\t%.2f\n", package, seconds[ARGV[1]], seconds[ARGV[2]] - seconds[ARGV[1]] }' "${build}" "${part}.total"
done > "${out}/cpu.tsv"
grep -h '"Action":"fail"' "${out}"/part-*.jsonl | grep -o '"Package":"[^"]*","Test":"[^"/]*"' | sort -u | sed 's/^/loom-pilot: failed /'
echo "loom-pilot: tests took $(( SECONDS - started )) s in all"
exit "${status}"
`

// codexPreamble names the gate inputs' manifest for codexOpening; empty fetches none.
func codexPreamble(gateInputs string) string {
	if gateInputs != "" && !protocol.Sha256Pattern.MatchString(gateInputs) {
		fmt.Fprintf(os.Stderr, "adamic-gate: --gate-inputs %q isn't a sha256\n", gateInputs)
		os.Exit(2)
	}
	return "gateInputs=" + gateInputs + "\n"
}

// warmBody readies an instance for the sha's real units without running a test: every test binary built and
// vetted (`-exec /bin/true` stands in for running it), so a run's units find the build cache warm.
const warmBody = `cd "${tree}" || exit 2
echo "loom-warm: $(hostname) setup $(( SECONDS - started )) s"
go test -count=1 -exec /bin/true ./... > "${out}/warm.log" 2>&1
code=$?
[ "${code}" = 0 ] || tail -20 "${out}/warm.log"
echo "loom-warm: built and vetted every test binary in $(( SECONDS - started )) s in all"
exit "${code}"
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: adamic-gate plan|warm|compare ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "plan":
		err = plan(os.Args[2:])
	case "warm":
		err = warm(os.Args[2:])
	case "reds":
		var verdict string
		verdict, err = reds(os.Args[2:])
		if err == nil && verdict != "green" {
			os.Exit(map[string]int{"red": 1, "void": 3}[verdict])
		}
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
	gateInputs := flags.String("gate-inputs", "", "on codex, the hash of the gate inputs' manifest in the public store")
	remainder := flags.Bool("remainder", false, "also run, per package, every test the reference doesn't name (a gate, not a parity check)")
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
		opening = codexPreamble(*gateInputs) + codexOpening
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
	// A remainder spec runs a package's tests the reference never saw (new since it), on the lightest unit:
	// -run everything, -skip every test the plan already placed. It holds no test of its own in the plan.
	remainders := make([][]string, *units)
	if *remainder {
		byPackage := map[string][]string{}
		for _, test := range tests {
			packageName, name, _ := strings.Cut(test.key, " ")
			byPackage[packageName] = append(byPackage[packageName], regexp.QuoteMeta(name))
		}
		for key := range exclusions {
			packageName, name, _ := strings.Cut(key, " ")
			if _, planned := byPackage[packageName]; planned {
				byPackage[packageName] = append(byPackage[packageName], regexp.QuoteMeta(name))
			}
		}
		names := make([]string, 0, len(byPackage))
		for packageName := range byPackage {
			names = append(names, packageName)
		}
		sort.Strings(names)
		for _, packageName := range names {
			lightest := 0
			for index := range loads {
				if loads[index] < loads[lightest] {
					lightest = index
				}
			}
			sort.Strings(byPackage[packageName])
			remainders[lightest] = append(remainders[lightest], packageName+"=. skip=^("+strings.Join(byPackage[packageName], "|")+")$")
		}
	}
	job := protocol.Job{Name: "adamic-gate-pilot"}
	for index, packages := range assigned {
		if len(packages) == 0 && len(remainders[index]) == 0 {
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
		argv = append(argv, remainders[index]...)
		job.Units = append(job.Units, protocol.JobUnit{
			Id: fmt.Sprintf("tests-%02d", index), Argv: argv, TimeoutSeconds: 3*3600 + 600,
			Outputs:   []protocol.Output{{Glob: "loom-out/test.jsonl.gz"}, {Glob: "loom-out/cpu.tsv"}},
			Resources: protocol.Resources{Cpus: 12},
		})
		fmt.Fprintf(os.Stderr, "tests-%02d: %d packages, %.0f s by the reference\n", index, len(packages), loads[index])
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(job)
}

// warm plans one warm-up unit per Codex instance in the pool: the opening (clone, the gate's setup.sh once) and
// warmBody at the sha. Each instance holds one unit at a time, so units equal to the pool's width reach every
// instance that is asking when the run starts.
func warm(arguments []string) error {
	flags := flag.NewFlagSet("warm", flag.ContinueOnError)
	sha := flags.String("sha", "", "the main commit the next runs will test")
	units := flags.Int("units", 25, "how many instances to warm")
	gateInputs := flags.String("gate-inputs", "", "the hash of the gate inputs' manifest in the public store")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(*sha) || *units < 1 {
		return fmt.Errorf("warm needs a full 40-character --sha and --units of at least 1")
	}
	job := protocol.Job{Name: "adamic-gate-warm"}
	for index := 0; index < *units; index++ {
		job.Units = append(job.Units, protocol.JobUnit{
			Id: fmt.Sprintf("warm-%02d", index), Argv: []string{"bash", "-c", codexPreamble(*gateInputs) + codexOpening + warmBody, "adamic-gate-warm", *sha},
			TimeoutSeconds: 3600, Resources: protocol.Resources{Cpus: 4},
		})
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(job)
}

// plannedTests reads the job back into each unit's "<package> <test>" keys, from its argv.
// It also names each unit's remainder packages: the package's tests no record named, found as they run.
func plannedTests(job protocol.Job) (map[string][]string, map[string]map[string]bool, error) {
	planned := map[string][]string{}
	remainders := map[string]map[string]bool{}
	for _, unit := range job.Units {
		if len(unit.Argv) < 5 {
			return nil, nil, fmt.Errorf("unit %s isn't a pilot unit", unit.Id)
		}
		for _, spec := range unit.Argv[5:] {
			packageName, pattern, _ := strings.Cut(spec, "=")
			if strings.Contains(pattern, " skip=") {
				if remainders[unit.Id] == nil {
					remainders[unit.Id] = map[string]bool{}
				}
				remainders[unit.Id][packageName] = true
				continue
			}
			inner := strings.TrimSuffix(strings.TrimPrefix(pattern, "^("), ")$")
			for _, quoted := range strings.Split(inner, "|") {
				planned[unit.Id] = append(planned[unit.Id], packageName+" "+strings.ReplaceAll(quoted, `\`, ""))
			}
		}
	}
	return planned, remainders, nil
}

func compare(arguments []string) (bool, error) {
	flags := flag.NewFlagSet("compare", flag.ContinueOnError)
	referencePath := flags.String("reference", "", "the whole gate's test.jsonl.gz for the same sha")
	jobPath := flags.String("job", "", "the job file plan wrote")
	recordPath := flags.String("record", "", "the run's record, from loom run --record")
	wire := flags.String("wire", "https://loom-wire.kirk-ouimet.workers.dev", "the wire's origin")
	only := flags.String("only", "", "compare only packages matching this regular expression, as plan did")
	timesPath := flags.String("times", "", "write every test's seconds on Loom beside the whole gate's, longest first, to this file")
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
	planned, remainders, err := plannedTests(job)
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
	// A unit's CPU is every attempt's (a preempted attempt spent it too); build and test come from its cpu.tsv.
	type unitTiming struct {
		unit             string
		wall, setup      float64
		cpu, build, test float64
		machine          string
	}
	var timings []unitTiming
	loom := map[string]result{}
	seen := map[string]string{}
	var first, last time.Time
	setupPattern := regexp.MustCompile(`setup (\d+) s`)
	for _, unit := range job.Units {
		timing := unitTiming{unit: unit.Id}
		output, cpuOutput := "", ""
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
				timing.cpu += event.UserSeconds + event.SystemSeconds
			case "output":
				if match := setupPattern.FindStringSubmatch(event.Text); match != nil {
					timing.setup, _ = strconv.ParseFloat(match[1], 64)
				}
			case "uploaded":
				switch event.Path {
				case "loom-out/test.jsonl.gz":
					output = event.Sha256
				case "loom-out/cpu.tsv":
					cpuOutput = event.Sha256
				}
			}
		}
		if cpuOutput != "" {
			content, err := fetchBlob(*wire, run, token, cpuOutput)
			if err != nil {
				return false, fmt.Errorf("unit %s's cpu.tsv: %w", unit.Id, err)
			}
			for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
				if fields := strings.Split(line, "\t"); len(fields) == 3 {
					build, _ := strconv.ParseFloat(fields[1], 64)
					test, _ := strconv.ParseFloat(fields[2], 64)
					timing.build += build
					timing.test += test
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
			packageName, _, _ := strings.Cut(key, " ")
			if !mine[key] && remainders[unit.Id][packageName] && plannedSet[key] == "" {
				plannedSet[key] = unit.Id // a test no record named, run by its package's remainder
				mine[key] = true
			}
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

	// The union against the reference less the exclusions: every test it ran, Loom ran. A test planned from an
	// older record that neither side ran is gone from this sha, not missing.
	for key := range reference {
		if _, excluded := exclusions[key]; !excluded && plannedSet[key] == "" {
			report("UNION: the reference ran %s and no unit did", key)
			parity = false
		}
	}
	gone := 0
	for key := range plannedSet {
		if _, ran := reference[key]; ran {
			continue
		}
		if _, ranHere := loom[key]; ranHere {
			report("UNION: Loom ran %s and the reference never did", key)
			parity = false
			continue
		}
		delete(plannedSet, key)
		gone++
	}
	if gone > 0 {
		report("gone: %d tests planned from an older record ran neither way at this sha", gone)
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
	if *timesPath != "" {
		if err := writeTimes(*timesPath, loom, reference, seen); err != nil {
			return false, err
		}
	}

	sort.Slice(timings, func(left, right int) bool { return timings[left].wall > timings[right].wall })
	var cpu, build, test float64
	for _, timing := range timings {
		report("  %s on %s: %.0f s, setup %.0f s, %.0f CPU-s (build %.0f, test %.0f)", timing.unit, timing.machine, timing.wall, timing.setup, timing.cpu, timing.build, timing.test)
		cpu += timing.cpu
		build += timing.build
		test += timing.test
	}
	report("cpu: %.0f CPU-seconds by the units' exits; go's build %.0f, tests %.0f, the rest (opening, setup, overhead) %.0f", cpu, build, test, cpu-build-test)
	if !first.IsZero() {
		report("wall: %.0f s from the first event to the last", last.Sub(first).Seconds())
	}
	if parity {
		report("PARITY: every planned test has the same verdict both ways, and the units' union is the reference less the exclusions")
	}
	return parity, nil
}

// writeTimes writes one line per test Loom ran, longest first: its seconds there, the whole gate's, its unit,
// its package and its name, tab-separated under a header.
func writeTimes(path string, loom map[string]result, reference map[string]result, units map[string]string) error {
	keys := make([]string, 0, len(loom))
	for key := range loom {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool {
		if loom[keys[left]].seconds != loom[keys[right]].seconds {
			return loom[keys[left]].seconds > loom[keys[right]].seconds
		}
		return keys[left] < keys[right]
	})
	var buffer bytes.Buffer
	buffer.WriteString("loom_seconds\twhole_gate_seconds\tunit\tpackage\ttest\n")
	for _, key := range keys {
		packageName, name, _ := strings.Cut(key, " ")
		fmt.Fprintf(&buffer, "%.2f\t%.2f\t%s\t%s\t%s\n", loom[key].seconds, reference[key].seconds, units[key], packageName, name)
	}
	return os.WriteFile(path, buffer.Bytes(), 0o644)
}

// reds reads a gate run's record into its verdict and, when red, every failed test with its unit and the first
// lines of its output: the whole red list at once. A unit that exited non-zero without a failed test broke (its
// opening or a package that never reported), and a unit with no results uploaded is missing: either makes the run
// void, never green and never a red the change owns. It prints the report and returns green, red or void.
func reds(arguments []string) (string, error) {
	flags := flag.NewFlagSet("reds", flag.ContinueOnError)
	jobPath := flags.String("job", "", "the job file plan wrote")
	recordPath := flags.String("record", "", "the run's record, from loom run --record")
	wire := flags.String("wire", "https://loom-wire.kirk-ouimet.workers.dev", "the wire's origin")
	lines := flags.Int("lines", 8, "output lines kept per failed test")
	if err := flags.Parse(arguments); err != nil {
		return "", err
	}
	jobFile, err := os.Open(*jobPath)
	if err != nil {
		return "", err
	}
	var job protocol.Job
	err = protocol.Decode(jobFile, &job)
	jobFile.Close()
	if err != nil {
		return "", err
	}
	run, _, events, err := readRecord(*recordPath)
	if err != nil {
		return "", err
	}
	home, _ := os.UserHomeDir()
	secret, err := protocol.ReadTokenSecret(filepath.Join(home, ".loom", "token-secret"))
	if err != nil {
		return "", err
	}
	token, err := protocol.MintToken(secret, protocol.TokenClaims{Run: run, Scope: protocol.ScopeCoordinator, Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		return "", err
	}
	var broken, failed []string
	tests := 0
	for _, unit := range job.Units {
		output, exitCode, exited, tail := "", 0, false, ""
		for _, event := range events {
			if event.Unit != unit.Id {
				continue
			}
			switch event.Type {
			case "exit":
				exited, exitCode = true, -1 // killed by a signal or the clock
				if event.Code != nil {
					exitCode = *event.Code
				}
			case "output":
				tail = event.Text
			case "uploaded":
				if event.Path == "loom-out/test.jsonl.gz" {
					output = event.Sha256
				}
			}
		}
		if !exited || output == "" {
			broken = append(broken, fmt.Sprintf("%s: no results (exited %t, last output %q)", unit.Id, exited, strings.TrimSpace(tail)))
			continue
		}
		content, err := fetchBlob(*wire, run, token, output)
		if err != nil {
			return "", fmt.Errorf("unit %s's results: %w", unit.Id, err)
		}
		texts, actions, err := readOutputs(content)
		if err != nil {
			return "", fmt.Errorf("unit %s's results: %w", unit.Id, err)
		}
		mine := 0
		keys := make([]string, 0, len(actions))
		for key := range actions {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		// Only failed leaves: a parent failing because its child did, or a package failing because a test did,
		// repeats that leaf, so it is left out of the list (still counted toward this unit's failures).
		leaf := func(key string) bool {
			packageName, test, _ := strings.Cut(key, " ")
			for other, action := range actions {
				if action != "fail" || other == key {
					continue
				}
				otherPackage, otherTest, _ := strings.Cut(other, " ")
				if otherPackage == packageName && (test == "(package)" || strings.HasPrefix(otherTest, test+"/")) {
					return false
				}
			}
			return true
		}
		for _, key := range keys {
			tests++
			if actions[key] != "fail" {
				continue
			}
			mine++
			if !leaf(key) {
				continue
			}
			kept := []string{}
			for _, line := range texts[key] {
				trimmed := strings.TrimRight(line, "\n")
				if strings.TrimSpace(trimmed) == "" || strings.HasPrefix(strings.TrimSpace(trimmed), "=== ") || strings.HasPrefix(strings.TrimSpace(trimmed), "--- ") {
					continue
				}
				if len(kept) < *lines {
					kept = append(kept, "    "+strings.TrimSpace(trimmed))
				}
			}
			failed = append(failed, fmt.Sprintf("%s (%s)\n%s", strings.Replace(key, " ", " ", 1), unit.Id, strings.Join(kept, "\n")))
		}
		if exitCode != 0 && mine == 0 {
			broken = append(broken, fmt.Sprintf("%s: exited %d with no failed test (last output %q)", unit.Id, exitCode, strings.TrimSpace(tail)))
		}
	}
	verdict := "green"
	switch {
	case len(broken) > 0:
		verdict = "void"
	case len(failed) > 0:
		verdict = "red"
	}
	fmt.Printf("run %s: %s, %d tests in %d units, %d failed, %d units broken\n", run, verdict, tests, len(job.Units), len(failed), len(broken))
	for _, line := range broken {
		fmt.Println("BROKEN " + line)
	}
	for _, line := range failed {
		fmt.Println("FAIL " + line)
	}
	return verdict, nil
}

// readOutputs reads go test -json lines (gzipped) into each test's output lines and terminal action, subtests
// included, keyed "<package> <test>". A package-level failure (a build error, a panic outside a test) is keyed
// "<package> (package)".
func readOutputs(content []byte) (map[string][]string, map[string]string, error) {
	decompressor, err := gzip.NewReader(bytes.NewReader(content))
	if err != nil {
		return nil, nil, err
	}
	texts, actions := map[string][]string{}, map[string]string{}
	scanner := bufio.NewScanner(decompressor)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for scanner.Scan() {
		var event struct {
			Action, Package, Test, Output string
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Package == "" {
			continue
		}
		key := event.Package + " " + event.Test
		if event.Test == "" {
			key = event.Package + " (package)"
		}
		switch event.Action {
		case "output":
			texts[key] = append(texts[key], event.Output)
		case "pass", "fail", "skip":
			if event.Test != "" || event.Action == "fail" {
				actions[key] = event.Action
			}
		}
	}
	return texts, actions, scanner.Err()
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
