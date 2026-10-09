#!/bin/bash
# verify.sh: a feedback run of one Adamic sha on Loom's pool, for whoever asked: go build ./... and go vet ./...
# as the first unit, then the named packages' tests in units at once. The build result goes to the requester the
# moment it exists (a build failure is the likeliest first red), the red list when the tests land.
#
#	pilots/adamic-gate/verify.sh <sha> <packages regex> <requester> [<slots>] [<note for the message>]
#
# A requester of "none" sends nothing (the fast-gate server reads the results from the work directory instead).
# LOOM_VERIFY_WORK names the work directory; LOOM_VERIFY_ENV names a file of `export K=V` lines every unit runs
# first (a fast gate's env); LOOM_VERIFY_PACKAGES names a file of import paths, one a line, so a package the
# reference never saw still runs whole; LOOM_VERIFY_SELECT names a fast gate's select.json, whose only_tests
# (package -> the only tests to run) and deferred (package -> tests to skip) shape every unit.
#
# A fast gate plans budgeted exactly like main's whole gate (@system_adamic, Oct 9 07:21Z: one gate, one rule, never a
# second planner with its own timeouts): test units within 60 s, killed at 90, a kill a red; products first and run to
# completion under 10 minutes. Side work runs only on the side pool (LOOM_VERIFY_POOL, default codex-side), never the
# star's (@system_adamic, Oct 8 23:59Z); the star's own fast gate (LOOM_PRIORITY 30 and up) runs on the star's pool.
# <slots> is "auto" by default: as many as the plan has units, up to 72 on the star's pool and 20 on the side pool.
# Nothing builds on Kirk's Mac: the binaries are the pre-gate's (pregate.sh's header says how they are made on a box).
set -uo pipefail
sha=$1 packages=$2 requester=$3 slots=${4:-auto} note=${5:-} pool=${LOOM_VERIFY_POOL:-codex-side}
[ "${LOOM_PRIORITY:-0}" -ge 30 ] && [ -z "${LOOM_VERIFY_POOL:-}" ] && pool=codex
work=${LOOM_VERIFY_WORK:-${HOME}/.loom/verify/${sha:0:12}-$(date -u +%Y%m%dT%H%M%SZ)}
mkdir -p "${work}"
loom=${HOME}/.loom/bin/loom-pregate planner=${HOME}/.loom/bin/adamic-gate
state=${HOME}/.adamic-full-gate gate=${HOME}/Projects/system/adamic-gate
send() {
	[ "${requester}" = none ] && return
	(cd /Users/kirkouimet/Projects/ahra && ./node_modules/.bin/ahra os send "${requester}" --body-file "$1" > /dev/null 2>&1 || true)
}

# The newest green whole gate's record sizes the units (the pre-gate keeps it cached).
green=$(cat "${state}/last-green")
reference=${HOME}/.loom/pregate/reference-${green}.jsonl.gz
if [ ! -s "${reference}" ]; then
	ref=$(git -C "${gate}" ls-remote origin "refs/heads/gate-logs/${green:0:12}/*" | awk '$2 ~ /\/full-main$/ {print $2}' | sort | tail -1)
	git -C "${gate}" fetch -q origin "${ref}" && git -C "${gate}" show FETCH_HEAD:test.jsonl.gz > "${reference}"
fi
# The tree's own test list at the sha, Loom's own times, and the gate's selection when it names only some tests.
git -C "${gate}" fetch -q origin "${sha}" 2> /dev/null
python3 "${HOME}/.loom/bin/treetests.py" "${sha}" > "${work}/tree-tests.txt" 2> /dev/null
budget=(--budget 60 --unit-setup 10 --split-all)
[ -s "${work}/tree-tests.txt" ] && budget+=(--tree-tests "${work}/tree-tests.txt")
[ -s "${HOME}/.loom/loom-times.tsv" ] && budget+=(--loom-times "${HOME}/.loom/loom-times.tsv")
if [ -n "${LOOM_VERIFY_SELECT:-}" ] && python3 -c 'import json, sys; sys.exit(0 if json.load(open(sys.argv[1])).get("only_tests") else 1)' "${LOOM_VERIFY_SELECT}" 2> /dev/null; then
	python3 -c 'import json, sys; json.dump(json.load(open(sys.argv[1]))["only_tests"], open(sys.argv[2], "w"))' "${LOOM_VERIFY_SELECT}" "${work}/only-tests.json"
	budget+=(--only-tests "${work}/only-tests.json")
fi
"${planner}" plan --target codex --remainder --gate-inputs "$(cat "${HOME}/.loom/gate-inputs")" --reference "${reference}" --sha "${sha}" --units 12 --only "${packages}" "${budget[@]}" > "${work}/tests.json" 2> "${work}/plan.err" || { echo "verify: planning failed: $(tail -1 "${work}/plan.err")"; exit 1; }

# The build-and-vet unit runs on the same opening as the tests, so it sees the tree they will.
python3 - "${work}" "${LOOM_VERIFY_ENV:-}" "${LOOM_VERIFY_PACKAGES:-}" "${LOOM_VERIFY_SELECT:-}" <<'PY'
import json, sys
work, environment, listed, selected = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
job = json.load(open(work + "/tests.json"))
# A fast gate runs the packages it names and nothing else. The planner's remainder carries "@unplanned=", which runs
# every package go list finds that the plan doesn't name; for a whole gate that's a package new since the reference,
# but here the plan names only the selection, so it ran about 75 packages whole in one unit (Oct 9: 6b11cbda's and
# trio 75d5288e's units ran past the 20-minute ceiling). A listed package the reference never saw is run below.
for unit in job["units"]:
    unit["argv"] = unit["argv"][:5] + [spec for spec in unit["argv"][5:] if not spec.startswith("@unplanned=")]
def quote(name):  # Go's regexp.QuoteMeta
    return "".join("\\" + c if c in "\\.+*?()|[]{}^$" else c for c in name)
selection = json.load(open(selected)) if selected else {}
# A package whose tests the gate names runs exactly those: the planner packed only them (--only-tests), so here its
# remainder spec, which would run the rest, is dropped.
for package in sorted(selection.get("only_tests") or {}):
    for unit in job["units"]:
        unit["argv"] = unit["argv"][:5] + [spec for spec in unit["argv"][5:] if not (spec.split("=", 1)[0] == package and " skip=" in spec)]
# Tests the fast gate defers are skipped in every spec of their package.
for package, tests in (selection.get("deferred") or {}).items():
    if not tests:
        continue
    names = [quote(test) for test in sorted(tests)]
    for unit in job["units"]:
        for index, spec in enumerate(unit["argv"][5:], start=5):
            if spec.split("=", 1)[0] != package:
                continue
            if " skip=^(" in spec:
                head, skipped = spec.split(" skip=^(", 1)
                unit["argv"][index] = head + " skip=^(" + "|".join(names) + "|" + skipped
            else:
                unit["argv"][index] = spec + " skip=^(" + "|".join(names) + ")$"
job["units"] = [unit for unit in job["units"] if len(unit["argv"]) > 5]
# A unit whose specs all left takes its needs with it; a unit that needed it no longer waits.
present = {unit["id"] for unit in job["units"]}
for unit in job["units"]:
    if unit.get("needs"):
        unit["needs"] = [need for need in unit["needs"] if need in present] or None
        if unit["needs"] is None:
            del unit["needs"]
# A listed package the reference never saw runs whole, as a remainder that skips nothing, on the unit with least.
covered = {spec.split("=", 1)[0] for unit in job["units"] for spec in unit["argv"][5:]}
for package in (open(listed).read().split() if listed else []):
    if package not in covered:
        min(job["units"], key=lambda unit: len(unit["argv"]))["argv"].append(package + "=. skip=^()$")
# A job's env runs first in every unit, after the line naming the gate inputs.
exports = open(environment).read() if environment else ""
if exports:
    for unit in job["units"]:
        first, rest = unit["argv"][2].split("\n", 1)
        unit["argv"][2] = first + "\n" + exports.rstrip("\n") + "\n" + rest
script = job["units"][0]["argv"][2]
# The unit body begins at its own export of the gate's environment, the last one: a fast gate's env, injected
# above the opening, can export ADAMIC_GATE_UNCACHED too (d56ae116's build unit lost its whole opening to that).
opening = script[:script.rindex("export ADAMIC_GATE_UNCACHED")]
body = '''[ -n "${tree:-}" ] && [ -d "${tree}/.git" ] && [ -n "${out:-}" ] || { echo "loom-build: no tree to build (the opening never ran): Loom's fault"; exit 2; }
cd "${tree}" || exit 2
echo "loom-build: tree $(git -C "${tree}" rev-parse HEAD) setup $(( SECONDS - started )) s"
go build ./... > "${out}/build.log" 2>&1; build=$?
go vet ./... > "${out}/vet.log" 2>&1; vet=$?
if [ "${build}" = 0 ]; then echo "loom-build: go build ./... passed"; else echo "loom-build: go build ./... FAILED (exit ${build})"; head -40 "${out}/build.log"; fi
if [ "${vet}" = 0 ]; then echo "loom-build: go vet ./... passed"; else echo "loom-build: go vet ./... FAILED (exit ${vet})"; head -40 "${out}/vet.log"; fi
[ "${build}" = 0 ] && [ "${vet}" = 0 ]
'''
unit = {"id": "build-vet", "argv": ["bash", "-c", opening + body, "adamic-build-vet", job["units"][0]["argv"][4]],
        "timeoutSeconds": 3600, "outputs": [{"glob": "loom-out/build.log"}, {"glob": "loom-out/vet.log"}], "resources": {"cpus": 4}}
job["name"] = "adamic-verify"
job["units"].insert(0, unit)
json.dump(job, open(work + "/job.json", "w"), indent=2)
PY
if [ "${slots}" = auto ]; then
	slots=$(python3 -c 'import json, sys; print(len(json.load(open(sys.argv[1]))["units"]))' "${work}/job.json")
	cap=$([ "${pool}" = codex ] && echo 72 || echo 20)
	[ "${slots}" -gt "${cap}" ] && slots=${cap}
fi
"${loom}" run --uncached --slots none --pool "${pool}=${slots}" --priority "${LOOM_PRIORITY:-0}" --record "${work}/record.jsonl" "${work}/job.json" > "${work}/run.log" 2>&1 &
coordinator=$!

# The build first, the moment its unit ends.
until grep -qE "^build-vet: (passed|failed|broken|void)" "${work}/run.log" || ! kill -0 "${coordinator}" 2> /dev/null; do sleep 5; done
run=$(head -1 "${work}/run.log" | awk '{print $2}' | tr -d :)
# Its output through curl (Cloudflare refuses Python's own client), with a coordinator token minted here.
token=$(python3 - "${run}" <<'PY'
import base64, hashlib, hmac, json, os, sys, time
secret = open(os.path.expanduser("~/.loom/token-secret")).read().strip().encode()
payload = base64.urlsafe_b64encode(json.dumps({"run": sys.argv[1], "scope": "coordinator", "expires": int(time.time()) + 600}, separators=(",", ":")).encode()).rstrip(b"=")
print((payload + b"." + base64.urlsafe_b64encode(hmac.new(secret, payload, hashlib.sha256).digest()).rstrip(b"=")).decode())
PY
)
curl -fsS "https://loom-wire.kirk-ouimet.workers.dev/runs/${run}/events?after=0" -H "Authorization: Bearer ${token}" > "${work}/events.jsonl"
python3 - "${work}" <<'PY'
import json, sys
work = sys.argv[1]
lines, code = [], None
for line in open(work + "/events.jsonl"):
    event = json.loads(line)["event"]
    if event.get("unit") != "build-vet":
        continue
    if event["type"] == "output":
        lines.append(event.get("text", "").rstrip("\n"))
    elif event["type"] == "exit":
        code = event.get("code")
text = "\n".join(lines)
# The build body exits 0 or 1; the opening exits 2 when Loom's own setup or checkout fails, before any build.
if code == 0:
    verdict = "passed"
elif code == 1 and "loom-build: go build" in text:
    verdict = "failed"
else:
    verdict = "broken"
open(work + "/build.verdict", "w").write(verdict + "\n")
open(work + "/build.txt", "w").write(text[:6000] + "\n")
PY
{
	case $(cat "${work}/build.verdict") in
		passed) echo "${sha:0:12} on Loom's pool: go build ./... and go vet ./... passed ($(grep -E '^build-vet: ' "${work}/run.log" | tail -1)):" ;;
		failed) echo "${sha:0:12} on Loom's pool: the build is RED ($(grep -E '^build-vet: ' "${work}/run.log" | tail -1)):" ;;
		*) echo "${sha:0:12} on Loom's pool: the build unit BROKE before building, Loom's setup or checkout, not your tree; it runs again ($(grep -E '^build-vet: ' "${work}/run.log" | tail -1)):" ;;
	esac
	echo
	cat "${work}/build.txt"
	echo
	echo "The packages (${packages}) are running; their red list follows when they land.${note:+ ${note}}"
} > "${work}/build-message.txt"
send "${work}/build-message.txt"

# Then the tests' whole red list.
wait "${coordinator}"
python3 -c "import json,sys; j=json.load(open(sys.argv[1])); j['units']=[u for u in j['units'] if u['id']!='build-vet']; json.dump(j,open(sys.argv[2],'w'))" "${work}/job.json" "${work}/tests-only.json"
"${planner}" reds --job "${work}/tests-only.json" --record "${work}/record.jsonl" --tests "${work}/test.jsonl" > "${work}/reds.txt" 2>&1
echo $? > "${work}/reds.exit"
{
	echo "${sha:0:12} on Loom's pool, the packages (${packages}):${note:+ ${note}}"
	echo
	head -c 10000 "${work}/reds.txt"
} > "${work}/reds-message.txt"
send "${work}/reds-message.txt"
echo "$(date -u +%H:%M:%S) ${sha:0:12}: $(head -1 "${work}/reds.txt")"
