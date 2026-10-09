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
	"math"
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
freeMegabytes() { df -Pm "${HOME}" /tmp | awk 'NR > 1 {print $4}' | sort -n | head -1; }
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
# A full disk must never read as a test red (Oct 9: cf04e18f's and e3f2be21's reds were all "no space left on device"
# under ~/.cache/adamic/runtime/.build-* and ~/.cache/go-build). A Codex instance's disk is 8.8 GB, about 4 GB free
# with the tree and the gate inputs on it. An instance runs one unit at a time, so the runtime's build directories
# left from earlier units are no one's; go's build cache is dropped whenever under 3 GB is free; under 1.5 GB after
# that, the unit doesn't start: exit 2, Loom's fault, placed again elsewhere.
freeMegabytes() { df -Pm "${HOME}" /tmp | awk 'NR > 1 {print $4}' | sort -n | head -1; }
rm -rf "${HOME}/.cache/adamic/runtime"/.build-* 2> /dev/null
# What an earlier unit left in /tmp that no later one reads: go's and the tests' temporary directories (a unit killed at
# its budget never removes them), npm trees replaced by a new lockfile, a half-made npm cache, and other units'
# workspaces. One unit runs at a time, so none of it is in use (Oct 9: an instance at 92% of its 8.8 GB /tmp failed
# 236 units in a row this way).
rm -rf /tmp/go-build* /tmp/Test* /tmp/adamic-npm/replaced-* /tmp/adamic-npm/*.staging-* 2> /dev/null
find /tmp -maxdepth 1 -name 'loom-unit-*' ! -path "$(dirname "${PWD}")" ! -path "${PWD}" -exec rm -rf {} + 2> /dev/null
[ "$(freeMegabytes)" -ge 3000 ] || rm -rf "${HOME}/.cache/go-build"
free=$(freeMegabytes)
[ "${free:-0}" -ge "${LOOM_MINIMUM_FREE_MB:-1500}" ] || { echo "loom-pilot: only ${free} MB free on the instance after trimming its caches: Loom's fault"; df -h "${HOME}" /tmp; du -xsh /tmp/* 2> /dev/null | sort -h | tail -8; exit 2; }
# Submodules are recorded over ssh; a cloud instance reaches GitHub over HTTPS only. The rewrite rides in the
# environment, never ~/.gitconfig: some instances mount it read-only, and there every cohere checkout went to ssh and
# failed (V3 8880e6bc's pre-gate, Oct 9: four units broken on "could not lock config file").
export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=url.https://github.com/.insteadOf GIT_CONFIG_VALUE_0=git@github.com:
# GitHub turns away anonymous fetches when many instances check out at once (15 side instances at 00:10Z on
# Oct 9 all read "could not read Username"; the same fetch a minute later passed), so each step retries with
# backoff and jitter before the unit gives up, and never prompts.
export GIT_TERMINAL_PROMPT=0
retry() { local attempt; for attempt in 1 2 3 4; do "$@" && return 0; sleep $(( attempt * 10 + RANDOM % 10 )); done; return 1; }
[ -d "${tree}/.git" ] || retry git clone -q --filter=blob:none https://github.com/system-inc/adamic.git "${tree}" || { echo "loom-pilot: clone failed"; exit 2; }
find "${tree}/.git" -maxdepth 6 -name index.lock -delete 2>/dev/null
retry git -C "${tree}" fetch -q origin "${sha}" && retry git -C "${tree}" switch -q --detach "${sha}" && retry git -C "${tree}" submodule update -q --init --recursive || { echo "loom-pilot: checkout of ${sha} failed"; exit 2; }
# A download cut short can leave a Go toolchain that runs but lacks much of its standard library (Oct 9, the side
# pool: "package fmt is not in std"), which setup.sh then keeps as installed and fails on. Before setup, an installed
# Go that can't list these packages moves aside so setup installs it fresh; after setup the same probe must pass.
# The probe names packages, since go list std lists only the ones a partial install has, and passes.
stdProbe="fmt context time syscall runtime hash flag unicode testing go/format go/parser text/tabwriter regexp/syntax math/rand/v2"
for tools in "${HOME}/.adamic-tools" "${HOME}/adamic-tools"; do
  if [ -x "${tools}/go/bin/go" ] && ! (cd / && GOROOT="${tools}/go" GOTOOLCHAIN=local "${tools}/go/bin/go" list ${stdProbe} > /dev/null 2>&1); then
    echo "loom-pilot: ${tools}/go lacks its standard library; moving it aside so setup installs Go again"
    mv "${tools}/go" "${tools}/go.broken-$$"
    rm -f /tmp/adamic-setup-done
  fi
done
if [ ! -f /tmp/adamic-setup-done ]; then
  (cd "${tree}" && bash cloud/setup.sh --wasi-sdk > /tmp/adamic-setup.log 2>&1) && touch /tmp/adamic-setup-done || { echo "loom-pilot: setup failed"; tail -20 /tmp/adamic-setup.log; exit 2; }
fi
for environment in "${HOME}/adamic-tools/env.sh" "${HOME}/.adamic-tools/env.sh"; do [ -f "${environment}" ] && { source "${environment}"; break; }; done
(cd / && go list ${stdProbe} > /dev/null 2>&1) || { echo "loom-pilot: the Go toolchain lacks its standard library after setup"; rm -f /tmp/adamic-setup-done; exit 2; }
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
# Killed at its budget (SIGTERM, then SIGKILL 5 s later), the unit names the leaves still running from the go test
# lines written so far, so a kill says which test cooked (#2en3b4t (f)); reds reads these lines.
atKill() {
  trap - TERM
  cat "${out}"/part-*.jsonl 2> /dev/null | awk '{
    action = ""; package = ""; test = ""
    if (match($0, /"Action":"[^"]*"/)) action = substr($0, RSTART + 10, RLENGTH - 11)
    if (match($0, /"Package":"[^"]*"/)) package = substr($0, RSTART + 11, RLENGTH - 12)
    if (match($0, /"Test":"[^"]*"/)) test = substr($0, RSTART + 8, RLENGTH - 9)
    if (test == "") next
    key = package " " test
    if (action == "run" || action == "cont") live[key] = 1
    else if (action == "pass" || action == "fail" || action == "skip" || action == "pause") delete live[key]
  } END {
    for (key in live) { leaf = 1; for (other in live) if (index(other, key "/") == 1) leaf = 0; if (leaf) print "loom-pilot: running at the kill: " key }
  }' | head -20
  exit 143
}
trap atKill TERM
# @unplanned=<package,...>: every package go list names on this tree that the list doesn't, run whole. A package new
# since the reference the plan was made from is in no other spec (8161285a's internal/buildcache, Oct 9).
specs=()
for spec in "$@"; do
  case "${spec}" in
    @unplanned=*)
      listing=$(cd "${tree}" && go list ./... 2> "${out}/go-list.stderr") || { echo "loom-pilot: go list ./... failed on the tree"; tail -5 "${out}/go-list.stderr"; exit 1; }
      known=",${spec#@unplanned=},"
      for package in ${listing}; do
        case "${known}" in *",${package},"*) ;; *) specs+=("${package}=."); echo "loom-pilot: ${package} is new since the reference; running it whole" ;; esac
      done
      ;;
    *) specs+=("${spec}") ;;
  esac
done
index=0
for spec in "${specs[@]}"; do printf '%s\t%s\n' "${index}" "${spec}"; index=$((index + 1)); done |
  xargs -d '\n' -P "${parallel}" -I{} bash -c '
    index=${1%%	*} spec=${1#*	}
    package=${spec%%=*} pattern=${spec#*=} skip=""
    case "${pattern}" in *" skip="*) skip=${pattern#* skip=} pattern=${pattern%% skip=*} ;; esac
    # TestWASI runs alone with the WASI SDK'"'"'s clang first on PATH, as the gate'"'"'s wasi phase runs it; every other
    # test keeps the native clang (internal/native/wasm_test.go says so).
    [ "${pattern}" = "^(TestWASI)\$" ] && [ -n "${WASI_SYSROOT:-}" ] && export PATH="${WASI_SYSROOT%/share/wasi-sysroot}/bin:${PATH}"
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
# A test that ran out of disk proved nothing about the change: the unit is broken (exit 2), never red. A disk full
# enough can't keep the error in the logs either, so a disk still under 512 MB free after a red counts too.
if [ "${status}" != 0 ] && { cat "${out}"/part-*.jsonl "${out}"/part-*.stderr 2> /dev/null | grep -q "no space left on device" || [ "$(freeMegabytes)" -lt 512 ]; }; then
  echo "loom-pilot: the instance's disk filled during the tests (no space left on device): Loom's fault, not the change's"
  df -h "${HOME}" /tmp
  exit 2
fi
exit "${status}"
`

// abBody times one test on main, then the candidate, then main again, all on this one instance, so a warming
// instance can't fake a ratio (the rule for timeout and stall reds, @system_adamic, Oct 9). The opening readied
// the candidate; each run checks its commit out, builds the test binary untimed, then runs it and keeps go
// test's own -json. The seconds are the named test's Elapsed; the CPU is the shell's children's, from times.
const abBody = `main=$1 package=$2 pattern=$3 name=$4 candidate=${sha}
export ADAMIC_GATE_UNCACHED=1 ADAMIC_TEST_WASI=1 ADAMIC_ORACLE_WASI=1 ADAMIC_GATE_COHERE=1
# times in this shell, never in a subshell: its second line is the children's user and system time.
seconds() { awk 'FNR == 2 { split($1, userpart, "m"); split($2, systempart, "m"); printf "%.2f", userpart[1] * 60 + userpart[2] + systempart[1] * 60 + systempart[2] }' "$1"; }
run() {
  local label=$1 commit=$2
  retry git -C "${tree}" fetch -q origin "${commit}" && retry git -C "${tree}" switch -q --detach "${commit}" && retry git -C "${tree}" submodule update -q --init --recursive || { echo "loom-ab: checkout of ${commit} failed"; exit 2; }
  (cd "${tree}" && go test -count=1 -exec /bin/true -run "${pattern}" "${package}" > /dev/null 2>&1)
  times > "${out}/${label}.before"
  local started=${SECONDS}
  (cd "${tree}" && timeout 1500 go test -count=1 -json -timeout 24m -run "${pattern}" "${package}" > "${out}/${label}.jsonl" 2> "${out}/${label}.stderr")
  times > "${out}/${label}.after"
  echo "${label} ${commit} $(awk -v before="$(seconds "${out}/${label}.before")" -v after="$(seconds "${out}/${label}.after")" 'BEGIN { printf "%.2f", after - before }') $(( SECONDS - started ))" >> "${out}/cpu.txt"
}
run main-before "${main}"
run candidate "${candidate}"
run main-after "${main}"
python3 - "${out}" "${name}" <<'PYTHON'
import json, sys
out, name = sys.argv[1], sys.argv[2]
cpu = {line.split()[0]: float(line.split()[2]) for line in open(out + "/cpu.txt")}
wall = {line.split()[0]: float(line.split()[3]) for line in open(out + "/cpu.txt")}
result = {"test": name}
# A parent's Elapsed leaves out its parallel subtests (tsprinter's TestMutants read 19 s while its 30 subtests took
# about 50 s each, Oct 9), so a test with subtests is compared by the go test run's own wall time instead.
nested = False
for label in ("main-before", "candidate", "main-after"):
    for line in open(out + "/" + label + ".jsonl"):
        try:
            nested = nested or json.loads(line).get("Test", "").startswith(name + "/")
        except ValueError:
            pass
result["compareBy"] = "wall of the go test run (the test has subtests)" if nested else "the test's own Elapsed"
for label in ("main-before", "candidate", "main-after"):
    action, seconds = "missing (timed out or never ran)", None
    for line in open(out + "/" + label + ".jsonl"):
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if event.get("Test") == name and event.get("Action") in ("pass", "fail", "skip"):
            action, seconds = event["Action"], event.get("Elapsed")
    result[label] = {"action": action, "seconds": wall.get(label) if nested else seconds, "elapsed": seconds, "wallSeconds": wall.get(label), "cpuSeconds": cpu.get(label)}
json.dump(result, open(out + "/ab.json", "w"), indent=2)
print("loom-ab: " + json.dumps(result))
PYTHON
`

// ab plans the one-unit A/B job: the candidate checked out by the opening, then abBody.
func ab(arguments []string) error {
	flags := flag.NewFlagSet("ab", flag.ContinueOnError)
	candidate := flags.String("candidate", "", "the red commit")
	mainSha := flags.String("main", "", "the main commit to compare against")
	packageName := flags.String("package", "", "the test's package, an import path")
	test := flags.String("test", "", "the test's full name, subtests after slashes (TestMutants/yield_loses_delegation)")
	gateInputs := flags.String("gate-inputs", "", "the hash of the gate inputs' manifest in the public store")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	shaPattern := regexp.MustCompile(`^[0-9a-f]{40}$`)
	if !shaPattern.MatchString(*candidate) || !shaPattern.MatchString(*mainSha) || *packageName == "" || *test == "" {
		return fmt.Errorf("ab needs a full --candidate and --main, a --package and a --test")
	}
	parts := strings.Split(*test, "/")
	for index, part := range parts {
		parts[index] = "^" + regexp.QuoteMeta(part) + "$"
	}
	job := protocol.Job{Name: "adamic-ab", Units: []protocol.JobUnit{{
		Id:             "ab",
		Argv:           []string{"bash", "-c", codexPreamble(*gateInputs) + codexOpening + abBody, "adamic-ab", *candidate, *mainSha, *packageName, strings.Join(parts, "/"), *test},
		TimeoutSeconds: 3*1500 + 900,
		Outputs:        []protocol.Output{{Glob: "loom-out/ab.json"}},
		Resources:      protocol.Resources{Cpus: 4},
	}}}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(job)
}

// oneUnit plans a job of one unit: the Codex opening readies the sha with the gate's environment, then the
// body (a bash file) runs on that tree, with any arguments after the flags as its positional parameters.
type outputGlobs []string

func (globs *outputGlobs) String() string        { return strings.Join(*globs, ",") }
func (globs *outputGlobs) Set(glob string) error { *globs = append(*globs, glob); return nil }

func oneUnit(arguments []string) error {
	flags := flag.NewFlagSet("unit", flag.ContinueOnError)
	sha := flags.String("sha", "", "the commit the opening checks out")
	gateInputs := flags.String("gate-inputs", "", "the hash of the gate inputs' manifest in the public store")
	bodyPath := flags.String("body", "", "a bash file run after the opening, on its tree")
	id := flags.String("id", "unit", "the unit's id")
	timeout := flags.Int("timeout", 3600, "the unit's timeout in seconds")
	var outputs outputGlobs
	flags.Var(&outputs, "output", "an output glob under the unit's directory (repeatable)")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(*sha) || *bodyPath == "" {
		return fmt.Errorf("unit needs a full --sha and a --body")
	}
	body, err := os.ReadFile(*bodyPath)
	if err != nil {
		return err
	}
	unit := protocol.JobUnit{
		Id: *id, Argv: append([]string{"bash", "-c", codexPreamble(*gateInputs) + codexOpening + string(body), "adamic-unit", *sha}, flags.Args()...),
		TimeoutSeconds: *timeout, Resources: protocol.Resources{Cpus: 4},
	}
	for _, glob := range outputs {
		unit.Outputs = append(unit.Outputs, protocol.Output{Glob: glob})
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(protocol.Job{Name: "adamic-" + *id, Units: []protocol.JobUnit{unit}})
}

// codexPreamble names the gate inputs' manifest for codexOpening; empty fetches none.
func codexPreamble(gateInputs string) string {
	if gateInputs != "" && !protocol.Sha256Pattern.MatchString(gateInputs) {
		fmt.Fprintf(os.Stderr, "adamic-gate: --gate-inputs %q isn't a sha256\n", gateInputs)
		os.Exit(2)
	}
	return "gateInputs=" + gateInputs + "\n"
}

// buildVetBody is the gate's build and vet as one stage unit: go build ./... and go vet ./... on the tree the
// opening readied. A stage unit reports no tests: compare and reds judge it by its exit, 0 green, 1 the change's
// red, anything else Loom's.
const buildVetBody = `[ -n "${tree:-}" ] && [ -d "${tree}/.git" ] && [ -n "${out:-}" ] || { echo "loom-build: no tree to build (the opening never ran): Loom's fault"; exit 2; }
cd "${tree}" || exit 2
go build ./... > "${out}/build.log" 2>&1; build=$?
go vet ./... > "${out}/vet.log" 2>&1; vet=$?
if [ "${build}" = 0 ]; then echo "loom-stage: go build ./... passed"; else echo "loom-stage: go build ./... FAILED (exit ${build})"; head -40 "${out}/build.log"; fi
if [ "${vet}" = 0 ]; then echo "loom-stage: go vet ./... passed"; else echo "loom-stage: go vet ./... FAILED (exit ${vet})"; head -40 "${out}/vet.log"; fi
[ "${build}" = 0 ] && [ "${vet}" = 0 ]
`

// phaseBody runs one phase of the whole gate, or one unit of a phase, through the gate's own run.py from the tools
// commit (run.py --full --phase, developer tools' file), so Loom never keeps a copy of the gate's logic. The tools
// tree is a worktree of the instance's clone at the tools sha, made once per sha. run.py's out directory comes back
// whole as phase.tar.gz; its exit is the unit's, and only a missing tools tree is Loom's fault (exit 2). The phase
// census takes the sha256 of the pool's merged go test record in the public store as its unit name.
const phaseBody = `toolsSha=$1 phase=$2 unitName=${3:-}
[ -n "${tree:-}" ] && [ -d "${tree}/.git" ] && [ -n "${out:-}" ] || { echo "loom-phase: no tree (the opening never ran): Loom's fault"; exit 2; }
gateTools=/tmp/adamic-gate-tools/${toolsSha:0:12}
if [ ! -f "${gateTools}/cloud/fast-gate/run.py" ]; then
  mkdir -p /tmp/adamic-gate-tools
  git -C "${tree}" worktree prune
  staging=/tmp/adamic-gate-tools/staging-$$
  retry git -C "${tree}" fetch -q origin "${toolsSha}" && git -C "${tree}" worktree add -q --detach "${staging}" "${toolsSha}" && mv "${staging}" "${gateTools}" || { echo "loom-phase: tools checkout of ${toolsSha} failed: Loom's fault"; exit 2; }
fi
export ADAMIC_GATE_UNCACHED=1 ADAMIC_TEST_WASI=1 ADAMIC_ORACLE_WASI=1 ADAMIC_GATE_COHERE=1
echo "loom-phase: $(hostname) ${phase}${unitName:+ ${unitName}} at ${sha:0:12}, tools ${toolsSha:0:12}, setup $(( SECONDS - started )) s"
if [ "${phase}" = census ]; then
  # census <sha256>: the whole gate's census over the merged go test record of the pool's runs, fetched by hash.
  curl -fsS --retry 3 -o "${out}/merged.jsonl" "https://adamic-store.kirkouimet.com/blobs/${unitName}" && echo "${unitName}  ${out}/merged.jsonl" | sha256sum -c --quiet || { echo "loom-phase: merged record ${unitName} unreadable: Loom's fault"; exit 2; }
  python3 "${gateTools}/cloud/fast-gate/run.py" --full --census "${out}/merged.jsonl" --tree "${tree}" --sha "${sha}" --base "${sha}" --tools "${gateTools}" --out "${out}/phase" > "${out}/phase.log" 2>&1
else
  python3 "${gateTools}/cloud/fast-gate/run.py" --full --phase "${phase}" ${unitName:+--unit "${unitName}"} --tree "${tree}" --sha "${sha}" --base "${sha}" --tools "${gateTools}" --out "${out}/phase" > "${out}/phase.log" 2>&1
fi
code=$?
tar -C "${out}" -czf "${out}/phase.tar.gz" phase phase.log
echo "loom-phase: $(head -1 "${out}/phase/status.txt" 2> /dev/null || echo "no status.txt") (exit ${code}, $(( SECONDS - started )) s in all)"
[ "${code}" = 0 ] || tail -20 "${out}/phase.log"
if [ "${code}" != 0 ] && { grep -rqs "no space left on device" "${out}/phase.log" "${out}/phase" || [ "$(freeMegabytes)" -lt 512 ]; }; then
  echo "loom-phase: the instance's disk filled (no space left on device): Loom's fault, not the change's"
  exit 2
fi
exit "${code}"
`

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
	case "ab":
		err = ab(os.Args[2:])
	case "unit":
		err = oneUnit(os.Args[2:])
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

// auditedParents are the tests whose direct subtests the whole gate's own planner runs as separate units, each
// selectable alone (adamic's cmd/adamic-gate children(), which owns the list; keep the two in step). A test whose
// owner declared shards (const <testName>Shards = N, children shard-NNN) is split the same way. A parent is split
// only when it, or its children together, take longer than splitSeconds, and its children are the plain names a
// -run can select.
var auditedParents = map[string]bool{
	module + "stage1/cohere/typeaware TestVolumeAgreementAndMutants": true,
	module + "internal/oracle TestNativeAgreesWithNode":              true,
	module + "internal/oracle TestInputAgreesWithNode":               true,
	module + "internal/oracle TestFreshWriteProbesStayRefused":       true,
	module + "stage1/cohere/css TestCSSPrinterAgreesWithGo":          true,
	module + "stage1/cohere/lint TestMutants":                        true,
	module + "stage1/cohere/lint TestVolumeMutants":                  true,
	module + "internal/native TestNormalizeMatchesNode":              true,
	module + "internal/native TestStringIndexMatchesNode":            true,
}

const splitSeconds = 30

var shardName = regexp.MustCompile(`^shard-\d{3,}$`)

// splitParents names the reference's top-level tests to plan child by child, each with its direct children
// and their seconds: audited or sharded, over splitSeconds, with at least two children.
// splitAll (plan --split-all, @system_adamic, Oct 9) splits any parent with subtests, audited or not; each run's
// compare checks every child against the reference, so a child that leans on a sibling shows as a mismatch, and
// that parent goes back to whole (unsplittable lists it).
var splitAll bool

var unsplittable = map[string]bool{}

func splitParents(reference map[string]result) map[string]map[string]float64 {
	children := map[string]map[string]float64{}
	for key, outcome := range reference {
		packageName, test, _ := strings.Cut(key, " ")
		parent, child, nested := strings.Cut(test, "/")
		if !nested || strings.Contains(child, "/") {
			continue
		}
		parentKey := packageName + " " + parent
		if children[parentKey] == nil {
			children[parentKey] = map[string]float64{}
		}
		children[parentKey][child] = outcome.seconds
	}
	split := map[string]map[string]float64{}
	for parentKey, names := range children {
		// A parent whose children run in parallel reports less than they took, so their sum counts too.
		within := 0.0
		for _, seconds := range names {
			within += seconds
		}
		if len(names) < 2 || max(reference[parentKey].seconds, within) <= splitSeconds {
			continue
		}
		sharded := true
		for name := range names {
			sharded = sharded && shardName.MatchString(name)
		}
		if (auditedParents[parentKey] || sharded || splitAll) && !unsplittable[parentKey] {
			split[parentKey] = names
		}
	}
	return split
}

// A result is one top-level test's terminal action and its seconds.
type result struct {
	action  string
	seconds float64
}

// readTests reads go test -json lines (gzipped or not) into each top-level test's last terminal action,
// keyed "<package> <test>", and counts how many terminal actions each test had.
func readTests(reader io.Reader) (map[string]result, map[string]int, error) {
	results, counts, err := readAll(reader)
	for key := range results {
		if _, test, _ := strings.Cut(key, " "); strings.Contains(test, "/") {
			delete(results, key)
			delete(counts, key)
		}
	}
	return results, counts, err
}

// readAll is readTests with every subtest kept too, keyed "<package> <test>/<subtest>". A test's seconds are the
// larger of its Elapsed and its wall from its run (or its last cont) to its end: a parent's Elapsed leaves out its parallel
// subtests (tsprinter's TestMutants read 19 s and took 678 s, Oct 9), so its wall is its cost.
func readAll(reader io.Reader) (map[string]result, map[string]int, error) {
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
	started := map[string]time.Time{}
	scanner := bufio.NewScanner(buffered)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for scanner.Scan() {
		var event struct {
			Time    time.Time
			Action  string
			Package string
			Test    string
			Elapsed float64
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Test == "" {
			continue
		}
		key := event.Package + " " + event.Test
		// A parallel test pauses after it starts until its package's serial tests finish; its wall runs from
		// its cont, not its run, or every top-level parallel test would carry the whole package's wait.
		if event.Action == "run" || event.Action == "cont" {
			started[key] = event.Time
			continue
		}
		if event.Action != "pass" && event.Action != "fail" && event.Action != "skip" {
			continue
		}
		seconds := event.Elapsed
		if start, ok := started[key]; ok && !event.Time.IsZero() {
			seconds = max(seconds, event.Time.Sub(start).Seconds())
		}
		results[key] = result{action: event.Action, seconds: seconds}
		counts[key]++
	}
	return results, counts, scanner.Err()
}

// readTestsFile reads the reference, every subtest included, keeping only packages that match only (a regular
// expression; empty keeps all), so a small trial can stand in for the whole set.
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
	results, _, err := readAll(file)
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
	units := flags.Int("units", 10, "how many units, unless --budget sets the count")
	budget := flags.Float64("budget", 0, "seconds a unit may take, its setup included: units are as many as fit it, each killed at budget plus a half")
	unitSetup := flags.Float64("unit-setup", 10, "with --budget, seconds a unit spends before its first test (warm instance)")
	only := flags.String("only", "", "plan only packages matching this regular expression, for a trial")
	target := flags.String("target", "box", "where the units run: box (a gate slot's warm tree) or codex (a Codex instance)")
	gateInputs := flags.String("gate-inputs", "", "on codex, the hash of the gate inputs' manifest in the public store")
	remainder := flags.Bool("remainder", false, "also run, per package, every test the reference doesn't name (a gate, not a parity check)")
	stages := flags.Bool("stages", false, "also run the gate's other stages as units (today: go build and go vet)")
	phasesTools := flags.String("phases", "", "also run the whole gate's other phases as units, through run.py --phase at this tools sha")
	loomTimes := flags.String("loom-times", "", "size each test by its seconds on Loom's own units (compare --times), where it has them")
	phaseUnits := flags.String("phase-units", "", "with --phases, the units to run: one line each, <phase> or <phase> <unit> (run.py --list-units)")
	treeTests := flags.String("tree-tests", "", "the tree's own top-level tests at the sha, \"<package> <test>\" per line (treetests.py): the plan's test list")
	flags.BoolVar(&splitAll, "split-all", false, "split every test with subtests over 30 s, not only the gate's audited parents")
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
	// A test Loom has timed is packed by that time: the box's wall on 64 cores understates a parallel test on 4 CPUs
	// (8161285a's tsprinter TestMutants: 304 s on the Threadripper, 963 s on a Codex instance).
	timed := map[string]float64{}
	if *loomTimes != "" {
		timed, err = readLoomTimes(*loomTimes)
		if err != nil {
			return err
		}
		for key, seconds := range timed {
			if outcome, known := reference[key]; known {
				outcome.seconds = seconds
				reference[key] = outcome
			}
		}
	}
	if *treeTests != "" {
		if err := fromTheTree(reference, *treeTests, *only, timed); err != nil {
			return err
		}
	}
	// A subtest Loom has timed under a test the plan holds joins the plan, so a test split into subtests since the
	// reference (tsprinter's TestExpressionsAgainstGoAndPrettier into shard-000 and on) is packed child by child
	// instead of whole at its old time.
	joined := 0
	for key, seconds := range timed {
		packageName, test, _ := strings.Cut(key, " ")
		top, _, isSubtest := strings.Cut(test, "/")
		if _, held := reference[key]; held || !isSubtest {
			continue
		}
		if _, parent := reference[packageName+" "+top]; parent {
			reference[key] = result{action: "pass", seconds: seconds}
			joined++
		}
	}
	if joined > 0 {
		fmt.Fprintf(os.Stderr, "times: %d subtests Loom has timed joined the plan under their tests\n", joined)
	}
	opening := boxOpening
	if *target == "codex" {
		opening = codexPreamble(*gateInputs) + codexOpening
	} else if *target != "box" {
		return fmt.Errorf("--target is box or codex")
	}
	// The items to pack: every top-level test whole, except a split parent, which goes child by child. A unit
	// holding any of a parent's children also runs the parent's own time outside them (its setup), once.
	split := splitParents(reference)
	type item struct {
		key, parent, child string
		seconds            float64
	}
	var items []item
	setup := map[string]float64{}
	for key, outcome := range reference {
		if _, test, _ := strings.Cut(key, " "); strings.Contains(test, "/") {
			continue
		}
		if _, excluded := exclusions[key]; excluded && *target == "box" {
			continue // a Codex instance has the wasm SDK from setup.sh --wasi-sdk; a box slot may not
		}
		children, isSplit := split[key]
		if !isSplit {
			items = append(items, item{key: key, seconds: outcome.seconds})
			continue
		}
		within := 0.0
		for _, seconds := range children {
			within += seconds
		}
		// Children that ran in parallel each carry their siblings' contention in their wall, so together they can
		// far exceed the parent's own wall (tsprinter's 30 mutants on Home): then they're scaled to sum to it, the
		// work actually measured. Children that ran in turn sum below it, and the rest is the parent's setup.
		scale := 1.0
		if within > outcome.seconds && within > 0 {
			scale = outcome.seconds / within
		}
		for child, seconds := range children {
			items = append(items, item{key: key, parent: key, child: child, seconds: seconds * scale})
		}
		setup[key] = max(0, outcome.seconds-within)
		fmt.Fprintf(os.Stderr, "split %s: %d children, %.0f s of its own beside them\n", key, len(children), setup[key])
	}
	// Longest first, each onto the unit it leaves lightest: the classic greedy packing.
	sort.Slice(items, func(left, right int) bool {
		if items[left].seconds != items[right].seconds {
			return items[left].seconds > items[right].seconds
		}
		return items[left].key+"/"+items[left].child < items[right].key+"/"+items[right].child
	})
	// With a budget, an item that alone can't fit it is the burn-down: it gets a unit of its own, run to the end with
	// the long timeout so its verdict still counts, and is named on stderr. The rest pack into as few units as fit
	// the budget, setup included, each killed at budget plus a quarter (cooked, never failed).
	capacity := *budget - *unitSetup
	var alone []item
	if *budget > 0 {
		if capacity <= 0 {
			return fmt.Errorf("--budget must exceed --unit-setup")
		}
		kept := items[:0]
		for _, candidate := range items {
			if candidate.seconds+setup[candidate.parent] > capacity {
				alone = append(alone, candidate)
				// The cost that didn't fit: the item's own seconds and, for a split parent's child, the parent's own time
				// outside its children, which any unit holding a child pays.
				fmt.Fprintf(os.Stderr, "over budget: %s%s, %.0f s by Loom's times (%.0f s its own, %.0f s its parent's), a unit of its own\n", candidate.key, map[bool]string{true: "/" + candidate.child, false: ""}[candidate.parent != ""], candidate.seconds+setup[candidate.parent], candidate.seconds, setup[candidate.parent])
				continue
			}
			kept = append(kept, candidate)
		}
		items = kept
	}
	var loads []float64
	var assigned, childrenOf []map[string][]string // per unit: package to whole test names; split parent key to child names
	cost := func(index int, candidate item) float64 {
		if candidate.parent != "" && childrenOf[index][candidate.parent] == nil {
			return candidate.seconds + setup[candidate.parent]
		}
		return candidate.seconds
	}
	place := func(index int, candidate item) {
		loads[index] += cost(index, candidate)
		if candidate.parent != "" {
			childrenOf[index][candidate.parent] = append(childrenOf[index][candidate.parent], candidate.child)
			return
		}
		packageName, name, _ := strings.Cut(candidate.key, " ")
		assigned[index][packageName] = append(assigned[index][packageName], name)
	}
	pack := func(count int) float64 {
		loads = make([]float64, count)
		assigned = make([]map[string][]string, count)
		childrenOf = make([]map[string][]string, count)
		for index := range assigned {
			assigned[index] = map[string][]string{}
			childrenOf[index] = map[string][]string{}
		}
		heaviest := 0.0
		for _, candidate := range items {
			best := 0
			for index := range loads {
				if loads[index]+cost(index, candidate) < loads[best]+cost(best, candidate) {
					best = index
				}
			}
			place(best, candidate)
			heaviest = max(heaviest, loads[best])
		}
		return heaviest
	}
	count := *units
	if *budget > 0 {
		total := 0.0
		for _, candidate := range items {
			total += candidate.seconds
		}
		// The fewest units whose packing fits: doubled until one fits, then bisected, each try a whole packing, and
		// packed once more at the count chosen. Counting up one at a time packed 23,000 items hundreds of times once
		// the tree's own tests joined the plan (Oct 9).
		low := max(1, int(math.Ceil(total/capacity)))
		high := low
		for pack(high) > capacity && high < len(items) {
			low = high + 1
			high = min(len(items), high*2)
		}
		for low < high {
			middle := (low + high) / 2
			if pack(middle) <= capacity {
				high = middle
			} else {
				low = middle + 1
			}
		}
		count = high
		pack(count)
		fmt.Fprintf(os.Stderr, "budget %.0f s: %d units within %.0f s of tests each, %d over budget alone\n", *budget, count, capacity, len(alone))
	} else {
		pack(count)
	}
	packed := len(loads)
	for _, candidate := range alone {
		loads = append(loads, 0)
		assigned = append(assigned, map[string][]string{})
		childrenOf = append(childrenOf, map[string][]string{})
		place(len(loads)-1, candidate)
	}
	// A packed unit under a budget is killed at budget plus a half (90 s for 60, Kirk, Oct 9 03:17Z), and that kill is
	// a red; an unbudgeted or over-budget unit runs long.
	timeout := func(index int) int {
		if *budget > 0 && index < packed {
			return int(math.Ceil(*budget * 1.5))
		}
		return 3*3600 + 600
	}
	// Under a budget the remainder (tests no record timed) goes to a unit of its own with the long timeout, so an
	// untimed test can't cook a packed unit; without one, to the lightest unit.
	remainderUnit := -1
	if *budget > 0 && *remainder {
		loads = append(loads, 0)
		assigned = append(assigned, map[string][]string{})
		childrenOf = append(childrenOf, map[string][]string{})
		remainderUnit = len(loads) - 1
	}
	lightest := func() int {
		if remainderUnit >= 0 {
			return remainderUnit
		}
		least := 0
		for index := range loads {
			if loads[index] < loads[least] {
				least = index
			}
		}
		return least
	}
	// A remainder spec runs what no record named (new since it), on the lightest unit: per package, -run
	// everything and -skip every top-level test the plan placed; per split parent, its children the same way.
	// It holds no test of its own in the plan.
	remainders := make([][]string, len(loads))
	if *remainder {
		byPackage := map[string][]string{}
		for key := range reference {
			packageName, test, _ := strings.Cut(key, " ")
			if !strings.Contains(test, "/") {
				byPackage[packageName] = append(byPackage[packageName], regexp.QuoteMeta(test))
			}
		}
		for key := range exclusions {
			packageName, name, _ := strings.Cut(key, " ")
			if _, planned := byPackage[packageName]; planned && *target == "box" {
				byPackage[packageName] = append(byPackage[packageName], regexp.QuoteMeta(name))
			}
		}
		names := make([]string, 0, len(byPackage))
		for packageName := range byPackage {
			names = append(names, packageName)
		}
		sort.Strings(names)
		// Each remainder rides with tests it shares a test binary with: a package's on a unit already running some of
		// its tests, a split parent's on a unit running some of its children, so no unit builds every package (Oct 9:
		// e3f2be21's tests-17 held all 80 remainders and 6 new packages, the run's 20-minute tail). A remainder with
		// no such unit goes to the lightest, which then carries its own package's tests.
		holding := func(packageName string, parentKey string) int {
			best := -1
			for index := range loads {
				if index >= packed && remainderUnit < 0 {
					continue // an over-budget unit runs long already; leave it to its test
				}
				if (parentKey == "" && len(assigned[index][packageName]) > 0) || (parentKey != "" && len(childrenOf[index][parentKey]) > 0) {
					if best < 0 || loads[index] < loads[best] {
						best = index
					}
				}
			}
			if best < 0 {
				return lightest()
			}
			return best
		}
		for _, packageName := range names {
			sort.Strings(byPackage[packageName])
			index := holding(packageName, "")
			remainders[index] = append(remainders[index], packageName+"=. skip=^("+strings.Join(byPackage[packageName], "|")+")$")
		}
		// A package the reference never ran (new since it) is named by no spec above; the unit asks go list for them.
		unplannedUnit := lightest()
		remainders[unplannedUnit] = append(remainders[unplannedUnit], "@unplanned="+strings.Join(names, ","))
		parents := make([]string, 0, len(split))
		for parentKey := range split {
			parents = append(parents, parentKey)
		}
		sort.Strings(parents)
		for _, parentKey := range parents {
			packageName, parent, _ := strings.Cut(parentKey, " ")
			quoted := []string{}
			for child := range split[parentKey] {
				quoted = append(quoted, regexp.QuoteMeta(child))
			}
			sort.Strings(quoted)
			prefix := "^" + regexp.QuoteMeta(parent) + "$/"
			index := holding(packageName, parentKey)
			remainders[index] = append(remainders[index], packageName+"="+prefix+". skip="+prefix+"^("+strings.Join(quoted, "|")+")$")
		}
	}
	job := protocol.Job{Name: "adamic-gate-pilot"}
	if *phasesTools != "" {
		units, err := phaseJobUnits(opening, *sha, *phasesTools, *phaseUnits)
		if err != nil {
			return err
		}
		job.Units = append(job.Units, units...)
	}
	if *stages {
		job.Units = append(job.Units, protocol.JobUnit{
			Id: "stage-build-vet", Argv: []string{"bash", "-c", opening + buildVetBody, "adamic-gate-stage", *sha}, TimeoutSeconds: 3600,
			Outputs:   []protocol.Output{{Glob: "loom-out/build.log"}, {Glob: "loom-out/vet.log"}},
			Resources: protocol.Resources{Cpus: 4},
		})
	}
	for index, packages := range assigned {
		if len(packages) == 0 && len(childrenOf[index]) == 0 && len(remainders[index]) == 0 {
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
			// TestWASI needs the WASI clang, which the unit body puts on PATH for its spec alone.
			for position, name := range tests {
				if packageName+" "+name == module+"internal/native TestWASI" {
					tests = append(tests[:position:position], tests[position+1:]...)
					argv = append(argv, packageName+"=^(TestWASI)$")
					break
				}
			}
			if len(tests) == 0 {
				continue
			}
			sort.Strings(tests)
			quoted := make([]string, len(tests))
			for position, name := range tests {
				quoted[position] = regexp.QuoteMeta(name)
			}
			argv = append(argv, packageName+"=^("+strings.Join(quoted, "|")+")$")
		}
		parents := make([]string, 0, len(childrenOf[index]))
		for parentKey := range childrenOf[index] {
			parents = append(parents, parentKey)
		}
		sort.Strings(parents)
		for _, parentKey := range parents {
			packageName, parent, _ := strings.Cut(parentKey, " ")
			children := childrenOf[index][parentKey]
			sort.Strings(children)
			quoted := make([]string, len(children))
			for position, child := range children {
				quoted[position] = regexp.QuoteMeta(child)
			}
			argv = append(argv, packageName+"=^"+regexp.QuoteMeta(parent)+"$/^("+strings.Join(quoted, "|")+")$")
		}
		argv = append(argv, remainders[index]...)
		job.Units = append(job.Units, protocol.JobUnit{
			Id: fmt.Sprintf("tests-%02d", index), Argv: argv, TimeoutSeconds: timeout(index),
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

// readLoomTimes reads compare --times' file into each test's seconds on Loom, keyed "<package> <test>".
// fromTheTree makes the tree's own top-level tests the plan's list (#2en3b4t): a test the reference holds that the tree
// no longer has is dropped, its subtests with it, and a test the tree has that the reference doesn't is sized and
// packed like any other, instead of riding in a remainder unit unsized. The last green record is hours and dozens of
// test-only landings behind main (Oct 9: 7 of the 18 tests over 180 s in a plan were already split away). A new test
// is sized by Loom's own time for it; else, in a package that lost tests, by an even share of the lost seconds with
// 1.5x headroom, since a split keeps the work it divides; else by twice its package's median, or 30 s in a new package.
func fromTheTree(reference map[string]result, path string, only string, timed map[string]float64) error {
	pattern, err := regexp.Compile(only)
	if err != nil {
		return fmt.Errorf("--only: %w", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	present := map[string]bool{}
	for number, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		packageName, test, found := strings.Cut(line, " ")
		if !found || !strings.HasPrefix(test, "Test") || strings.Contains(test, "/") {
			return fmt.Errorf("%s:%d: want \"<package> <top-level test>\"", path, number+1)
		}
		if pattern.MatchString(packageName) {
			present[line] = true
		}
	}
	if len(present) == 0 {
		return fmt.Errorf("%s names no test the plan covers", path)
	}
	lost := map[string]float64{}
	known := map[string][]float64{}
	dropped := 0
	for key, outcome := range reference {
		packageName, test, _ := strings.Cut(key, " ")
		top, _, _ := strings.Cut(test, "/")
		if present[packageName+" "+top] {
			if top == test {
				known[packageName] = append(known[packageName], outcome.seconds)
			}
			continue
		}
		if top == test {
			lost[packageName] += outcome.seconds
			dropped++
		}
		delete(reference, key)
	}
	fresh := map[string][]string{}
	for key := range present {
		if _, held := reference[key]; !held {
			packageName, _, _ := strings.Cut(key, " ")
			fresh[packageName] = append(fresh[packageName], key)
		}
	}
	added, byTimes := 0, 0
	for packageName, keys := range fresh {
		median := 0.0
		if seconds := known[packageName]; len(seconds) > 0 {
			sort.Float64s(seconds)
			median = seconds[len(seconds)/2]
		}
		for _, key := range keys {
			seconds, wasTimed := timed[key]
			switch {
			case wasTimed:
				byTimes++
			case lost[packageName] > 0:
				seconds = 1.5 * lost[packageName] / float64(len(keys))
			case median > 0:
				seconds = 2 * median
			default:
				seconds = 30
			}
			reference[key] = result{action: "pass", seconds: seconds}
			added++
		}
	}
	fmt.Fprintf(os.Stderr, "tree: %d top-level tests; %d the reference held are gone and dropped, %d new are sized (%d by Loom's times)\n", len(present), dropped, added, byTimes)
	return nil
}

func readLoomTimes(path string) (map[string]float64, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	timed := map[string]float64{}
	for number, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		fields := strings.Split(line, "\t")
		if number == 0 && len(fields) > 0 && fields[0] == "loom_seconds" {
			continue
		}
		seconds, err := strconv.ParseFloat(fields[0], 64)
		if len(fields) != 5 || err != nil {
			return nil, fmt.Errorf("%s:%d: want loom_seconds, whole_gate_seconds, unit, package, test", path, number+1)
		}
		timed[fields[3]+" "+fields[4]] = seconds
	}
	return timed, nil
}

// phaseJobUnits is one unit per line of the phase list: run.py's own phase, or one unit of it, on the candidate.
func phaseJobUnits(opening string, sha string, toolsSha string, listPath string) ([]protocol.JobUnit, error) {
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(toolsSha) {
		return nil, fmt.Errorf("--phases takes the tools commit, a full 40-character sha")
	}
	content, err := os.ReadFile(listPath)
	if err != nil {
		return nil, fmt.Errorf("--phase-units: %w", err)
	}
	var units []protocol.JobUnit
	seen := map[string]bool{}
	for number, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) > 2 || !phaseName.MatchString(fields[0]) || (len(fields) == 2 && (!unitName.MatchString(fields[1]) || strings.Contains(fields[1], ".."))) {
			return nil, fmt.Errorf("%s:%d: a line is <phase> or <phase> <unit>: a phase of letters, digits, dots, dashes and underscores, a unit of those and slashes", listPath, number+1)
		}
		// A job's unit ids are lowercase letters, digits and dashes; the unit's own name rides in its argv as listed.
		id := "phase-" + strings.Trim(nonIdCharacters.ReplaceAllString(strings.ToLower(strings.Join(fields, "-")), "-"), "-")
		if seen[id] {
			return nil, fmt.Errorf("%s:%d: %s listed twice", listPath, number+1, strings.Join(fields, " "))
		}
		seen[id] = true
		argv := append([]string{"bash", "-c", opening + phaseBody, "adamic-gate-phase", sha, toolsSha}, fields...)
		units = append(units, protocol.JobUnit{Id: id, Argv: argv, TimeoutSeconds: 3600,
			Outputs: []protocol.Output{{Glob: "loom-out/phase.tar.gz"}}, Resources: protocol.Resources{Cpus: 4}})
	}
	if len(units) == 0 {
		return nil, fmt.Errorf("%s lists no phase", listPath)
	}
	return units, nil
}

// phaseName is a phase as run.py names it; unitName one of its units, which may be a tree path (a wasi fixture,
// internal/load/testdata/0.1/compile/01_hello.ts) or a number (a catalog entry).
var phaseName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
var nonIdCharacters = regexp.MustCompile(`[^a-z0-9]+`)
var unitName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,159}$`)

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

// A plannedJob is a job read back from its argv: each unit's planned keys ("<package> <test>", or
// "<package> <parent>/<child>" for a split parent), each unit's remainders (a package's, or a split parent's: the
// tests no record named, found as they run), and the split parents.
type plannedJob struct {
	tests             map[string][]string
	remainderPackages map[string]map[string]bool
	remainderParents  map[string]map[string]bool
	split             map[string]bool
	// unplanned is, per unit holding an @unplanned spec, the packages it knows: any other package's tests it ran
	// were new since the reference, and are that unit's.
	unplanned map[string]map[string]bool
}

var childSpec = regexp.MustCompile(`^\^(.+)\$/\^\((.*)\)\$$`)

// unquoteAlternation reads back an alternation of regexp.QuoteMeta'd names: it splits on each unescaped |, and a
// backslash keeps the character after it, so a name holding | or \ itself (cohere's selector cases, a NUL key
// test) comes back whole.
func unquoteAlternation(quoted string) []string {
	var names []string
	var name strings.Builder
	escaped := false
	for _, character := range quoted {
		switch {
		case escaped:
			name.WriteRune(character)
			escaped = false
		case character == '\\':
			escaped = true
		case character == '|':
			names = append(names, name.String())
			name.Reset()
		default:
			name.WriteRune(character)
		}
	}
	return append(names, name.String())
}

func plannedTests(job protocol.Job) (plannedJob, error) {
	planned := plannedJob{tests: map[string][]string{}, remainderPackages: map[string]map[string]bool{}, remainderParents: map[string]map[string]bool{}, split: map[string]bool{},
		unplanned: map[string]map[string]bool{}}
	unquote := func(quoted string) string { return strings.Join(unquoteAlternation(quoted), "|") }
	for _, unit := range job.Units {
		if strings.HasPrefix(unit.Id, "stage-") || strings.HasPrefix(unit.Id, "phase-") {
			continue // a stage or phase unit runs no tests of the plan
		}
		if len(unit.Argv) < 5 {
			return planned, fmt.Errorf("unit %s isn't a pilot unit", unit.Id)
		}
		for _, spec := range unit.Argv[5:] {
			if known, isUnplanned := strings.CutPrefix(spec, "@unplanned="); isUnplanned {
				planned.unplanned[unit.Id] = map[string]bool{}
				for _, packageName := range strings.Split(known, ",") {
					planned.unplanned[unit.Id][packageName] = true
				}
				continue
			}
			packageName, pattern, _ := strings.Cut(spec, "=")
			if run, _, isRemainder := strings.Cut(pattern, " skip="); isRemainder {
				if parent, isParent := strings.CutSuffix(run, "$/."); isParent {
					if planned.remainderParents[unit.Id] == nil {
						planned.remainderParents[unit.Id] = map[string]bool{}
					}
					planned.remainderParents[unit.Id][packageName+" "+unquote(strings.TrimPrefix(parent, "^"))] = true
					continue
				}
				if planned.remainderPackages[unit.Id] == nil {
					planned.remainderPackages[unit.Id] = map[string]bool{}
				}
				planned.remainderPackages[unit.Id][packageName] = true
				continue
			}
			if match := childSpec.FindStringSubmatch(pattern); match != nil {
				parent := unquote(match[1])
				planned.split[packageName+" "+parent] = true
				for _, name := range unquoteAlternation(match[2]) {
					planned.tests[unit.Id] = append(planned.tests[unit.Id], packageName+" "+parent+"/"+name)
				}
				continue
			}
			inner := strings.TrimSuffix(strings.TrimPrefix(pattern, "^("), ")$")
			for _, name := range unquoteAlternation(inner) {
				planned.tests[unit.Id] = append(planned.tests[unit.Id], packageName+" "+name)
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
	timesPath := flags.String("times", "", "write every test's seconds on Loom beside the whole gate's, longest first, to this file")
	var rerunFlags outputGlobs
	flags.Var(&rerunFlags, "rerun", "<job>:<record> of a later run whose units stand in for the same units of this one (a lost unit run again); repeatable")
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
	// What the reference ran that a plan must cover: every top-level test, except that a split parent is
	// covered by its direct children (and its own verdict checked across the units that ran them).
	expected := map[string]result{}
	for key, outcome := range reference {
		packageName, test, _ := strings.Cut(key, " ")
		parent, child, nested := strings.Cut(test, "/")
		switch {
		case !nested && !planned.split[key]:
			expected[key] = outcome
		case nested && !strings.Contains(child, "/") && planned.split[packageName+" "+parent]:
			expected[key] = outcome
		}
	}
	run, verdict, events, err := readRecord(*recordPath)
	if err != nil {
		return false, err
	}
	parity := true
	report := func(format string, arguments ...any) { fmt.Printf(format+"\n", arguments...) }
	report("run %s: verdict %s", run, verdict.Status)

	plannedSet := map[string]string{}
	for unit, keys := range planned.tests {
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
	// A unit a later run ran again (its worker lost it) takes its events and results from that run.
	type rerunOf struct {
		run, token string
		events     []protocol.Event
	}
	reruns := map[string]rerunOf{}
	for _, pair := range rerunFlags {
		rerunJob, rerunRecord, found := strings.Cut(pair, ":")
		if !found {
			return false, fmt.Errorf("--rerun takes <job>:<record>")
		}
		content, err := os.ReadFile(rerunJob)
		if err != nil {
			return false, err
		}
		var later protocol.Job
		if err := protocol.Decode(bytes.NewReader(content), &later); err != nil {
			return false, err
		}
		laterRun, _, laterEvents, err := readRecord(rerunRecord)
		if err != nil {
			return false, err
		}
		laterToken, err := protocol.MintToken(secret, protocol.TokenClaims{Run: laterRun, Scope: protocol.ScopeCoordinator, Expires: time.Now().Add(time.Hour).Unix()})
		if err != nil {
			return false, err
		}
		for _, unit := range later.Units {
			reruns[unit.Id] = rerunOf{run: laterRun, token: laterToken, events: laterEvents}
			report("RERUN %s: from run %s", unit.Id, laterRun)
		}
	}
	var timings []unitTiming
	loom := map[string]result{}
	parents := map[string]result{}
	seen := map[string]string{}
	var first, last time.Time
	setupPattern := regexp.MustCompile(`setup (\d+) s`)
	for _, unit := range job.Units {
		events, run, token := events, run, token
		if rerun, ok := reruns[unit.Id]; ok {
			events, run, token = rerun.events, rerun.run, rerun.token
		}
		timing := unitTiming{unit: unit.Id}
		output, cpuOutput := "", ""
		exitCode := -1
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
				if event.Code != nil {
					exitCode = *event.Code
				}
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
		if strings.HasPrefix(unit.Id, "stage-") || strings.HasPrefix(unit.Id, "phase-") {
			if exitCode != 0 {
				report("STAGE %s: exited %d", unit.Id, exitCode)
				parity = false
			}
			continue
		}
		if output == "" {
			report("UNIT %s: no test.jsonl.gz uploaded; its tests are missing", unit.Id)
			parity = false
			continue
		}
		content, err := fetchBlob(*wire, run, token, output)
		if err != nil {
			return false, fmt.Errorf("unit %s's results: %w", unit.Id, err)
		}
		results, counts, err := readAll(bytes.NewReader(content))
		if err != nil {
			return false, fmt.Errorf("unit %s's results: %w", unit.Id, err)
		}
		mine := map[string]bool{}
		for _, key := range planned.tests[unit.Id] {
			mine[key] = true
			if counts[key] != 1 {
				report("UNIT %s: %s reported %d times, want once", unit.Id, key, counts[key])
				parity = false
			}
		}
		for key, outcome := range results {
			packageName, test, _ := strings.Cut(key, " ")
			parent, child, nested := strings.Cut(test, "/")
			parentKey := packageName + " " + parent
			if !nested && planned.split[key] {
				// A split parent reports in every unit that ran some of its children: one verdict, failed if any.
				if previous, ok := parents[key]; !ok || outcome.action == "fail" || previous.action == "skip" {
					parents[key] = outcome
				}
				continue
			}
			if nested && (!planned.split[parentKey] || strings.Contains(child, "/")) {
				continue // a subtest inside a whole test, or deeper inside a child: its test's verdict carries it
			}
			known, unplanned := planned.unplanned[unit.Id]
			if !mine[key] && plannedSet[key] == "" && ((!nested && planned.remainderPackages[unit.Id][packageName]) || (nested && planned.remainderParents[unit.Id][parentKey]) ||
				(!nested && unplanned && !known[packageName])) {
				plannedSet[key] = unit.Id // a test no record named, run by its remainder
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
	for key := range expected {
		if _, excluded := exclusions[key]; !excluded && plannedSet[key] == "" {
			report("UNION: the reference ran %s and no unit did", key)
			parity = false
		}
	}
	for key := range planned.split {
		want, got := reference[key], parents[key]
		switch {
		case got.action == "":
			report("SPLIT: %s ran in no unit", key)
			parity = false
		case got.action != want.action:
			report("DIFFER: %s (split): the whole gate %s, Loom %s", key, want.action, got.action)
			parity = false
		}
	}
	gone := 0
	for key := range plannedSet {
		if _, ran := expected[key]; ran {
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
	testsPath := flags.String("tests", "", "also write every unit's go test -json lines, in unit order, to this file")
	if err := flags.Parse(arguments); err != nil {
		return "", err
	}
	var combined bytes.Buffer
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
	var broken, failed, cooked []string
	tests, killedUnits := 0, 0
	for _, unit := range job.Units {
		output, exitCode, exited, tail, timedOut := "", 0, false, "", false
		var running []string // the leaves its kill trap named, "<package> <test>"
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
				timedOut = event.TimedOut
			case "output":
				tail = event.Text
				for _, line := range strings.Split(event.Text, "\n") {
					if leaf, found := strings.CutPrefix(strings.TrimSpace(line), "loom-pilot: running at the kill: "); found {
						running = append(running, leaf)
					}
				}
			case "uploaded":
				if event.Path == "loom-out/test.jsonl.gz" {
					output = event.Sha256
				}
			}
		}
		// A stage or phase unit has no go test lines: run.py's (or go build's and vet's) exit is its verdict.
		if strings.HasPrefix(unit.Id, "stage-") || strings.HasPrefix(unit.Id, "phase-") {
			switch {
			case exited && exitCode == 0:
			case exited && exitCode == 1:
				failed = append(failed, fmt.Sprintf("%s (stage)\n    %s", unit.Id, strings.ReplaceAll(strings.TrimSpace(tail), "\n", "\n    ")))
			default:
				broken = append(broken, fmt.Sprintf("%s: the stage broke (exited %t, code %d, last output %q)", unit.Id, exited, exitCode, strings.TrimSpace(tail)))
			}
			continue
		}
		// Killed at its budget's kill (90 s for 60): a red, P0 for its slowest leaf's owner (Kirk, Oct 9 03:17Z, in place
		// of the 75 s cooked rule). The next plan splits it smaller.
		if timedOut {
			killedUnits++
			if len(running) == 0 {
				failed = append(failed, fmt.Sprintf("%s (killed at %d s, over budget)\n    last output %q", unit.Id, unit.TimeoutSeconds, strings.TrimSpace(tail)))
				cooked = append(cooked, unit.Id)
				continue
			}
			// Each leaf still running at the kill is the cooked one, named for its owner (#2en3b4t (f)).
			failed = append(failed, fmt.Sprintf("%s (killed at %d s, over budget)\n    running at the kill: %s", unit.Id, unit.TimeoutSeconds, strings.Join(running, "; ")))
			for _, leaf := range running {
				cooked = append(cooked, unit.Id+" "+leaf)
			}
			continue
		}
		if !exited || output == "" {
			broken = append(broken, fmt.Sprintf("%s: no results (exited %t, last output %q)", unit.Id, exited, strings.TrimSpace(tail)))
			continue
		}
		// Exit 2 is the unit's own word that the fault is Loom's (a full disk, a failed checkout): whatever its tests
		// said, it proved nothing about the change.
		if exitCode == 2 {
			broken = append(broken, fmt.Sprintf("%s: exited 2, Loom's fault (last output %q)", unit.Id, strings.TrimSpace(tail)))
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
		if *testsPath != "" {
			decompressor, err := gzip.NewReader(bytes.NewReader(content))
			if err != nil {
				return "", fmt.Errorf("unit %s's results: %w", unit.Id, err)
			}
			if _, err := io.Copy(&combined, decompressor); err != nil {
				return "", fmt.Errorf("unit %s's results: %w", unit.Id, err)
			}
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
	if *testsPath != "" {
		if err := os.WriteFile(*testsPath, combined.Bytes(), 0o644); err != nil {
			return "", err
		}
	}
	verdict := "green"
	switch {
	case len(broken) > 0:
		verdict = "void"
	case len(failed) > 0:
		verdict = "red"
	}
	fmt.Printf("run %s: %s, %d tests in %d units, %d failed, %d units broken, %d killed over budget\n", run, verdict, tests, len(job.Units), len(failed), len(broken), killedUnits)
	for _, line := range broken {
		fmt.Println("BROKEN " + line)
	}
	for _, line := range cooked {
		fmt.Println("KILLED " + line + ": over budget, P0")
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
