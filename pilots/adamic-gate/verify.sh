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
# reference never saw still runs whole.
#
# Side work runs only on the side pool (LOOM_VERIFY_POOL, default codex-side): the star's pool never holds it
# (@system_adamic, Oct 8 23:59Z). <slots> caps how much of it one run takes (default 5). Nothing builds on
# Kirk's Mac: the binaries are the pre-gate's (pregate.sh's header says how they are made on a box).
set -uo pipefail
sha=$1 packages=$2 requester=$3 slots=${4:-5} note=${5:-} pool=${LOOM_VERIFY_POOL:-codex-side}
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
"${planner}" plan --target codex --remainder --gate-inputs "$(cat "${HOME}/.loom/gate-inputs")" --reference "${reference}" --sha "${sha}" --units 12 --only "${packages}" > "${work}/tests.json" 2> /dev/null || { echo "verify: planning failed"; exit 1; }

# The build-and-vet unit runs on the same opening as the tests, so it sees the tree they will.
python3 - "${work}" "${LOOM_VERIFY_ENV:-}" "${LOOM_VERIFY_PACKAGES:-}" <<'PY'
import json, sys
work, environment, listed = sys.argv[1], sys.argv[2], sys.argv[3]
job = json.load(open(work + "/tests.json"))
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
opening = script[:script.index("export ADAMIC_GATE_UNCACHED")]
body = '''cd "${tree}" || exit 2
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
"${loom}" run --uncached --slots none --pool "${pool}=${slots}" --record "${work}/record.jsonl" "${work}/job.json" > "${work}/run.log" 2>&1 &
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
