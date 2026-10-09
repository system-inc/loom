#!/bin/bash
# ab.sh: the same-instance A/B for timeout and stall reds (#hjjffdf; @system_adamic, Oct 9: nobody calls a red load
# by hand again). Developer tools' trigger (adamic 1e8c4fa) writes ~/.loom/jobs/ab/<id>.json once per red and
# test: candidate, main, package, test (the leaf's anchored -run pattern), leaf (its full name), red, kind, notify.
# Each job is `adamic-gate ab`'s one unit on the side pool: the leaf timed on main, the candidate and main again on
# one instance. The verdict goes beside the job as <id>.verdict, its first line "regression <ratio>x" or
# "load <ratio>x" (the candidate against the mean of the two main runs, 2x the line between them), then each run's
# seconds and CPU; developer tools' com.adamic.ab-collect sends it on.
#
#	pilots/adamic-gate/ab.sh            # the server, as LaunchAgent com.loom.ab (a copy in ~/.loom/bin)
set -uo pipefail

jobs=${LOOM_AB_JOBS:-${HOME}/.loom/jobs/ab}
concurrent=${LOOM_AB_CONCURRENT:-2}
loom=${HOME}/.loom/bin/loom-pregate planner=${HOME}/.loom/bin/adamic-gate
mkdir -p "${jobs}"
rm -f "${jobs}"/*.running

serve() {
	local id=$1 work=${jobs}/$1.work run token
	mkdir -p "${work}"
	python3 - "${jobs}/${id}.json" "${work}" <<'PY'
import json, sys
job = json.load(open(sys.argv[1]))
for field in ("candidate", "main", "package", "leaf"):
    open(sys.argv[2] + "/" + field, "w").write(str(job.get(field, "")))
PY
	"${planner}" ab --candidate "$(cat "${work}/candidate")" --main "$(cat "${work}/main")" --package "$(cat "${work}/package")" --test "$(cat "${work}/leaf")" --gate-inputs "$(cat "${HOME}/.loom/gate-inputs")" > "${work}/job.json" 2> "${work}/plan.log" || {
		finish "${id}" "void: the A/B couldn't be planned ($(head -1 "${work}/plan.log"))"
		return
	}
	"${loom}" run --uncached --slots none --pool codex-side=1 --record "${work}/record.jsonl" "${work}/job.json" > "${work}/run.log" 2>&1
	run=$(head -1 "${work}/run.log" | awk '{print $2}' | tr -d :)
	token=$(python3 - "${run}" <<'PY'
import base64, hashlib, hmac, json, os, sys, time
secret = open(os.path.expanduser("~/.loom/token-secret")).read().strip().encode()
payload = base64.urlsafe_b64encode(json.dumps({"run": sys.argv[1], "scope": "coordinator", "expires": int(time.time()) + 600}, separators=(",", ":")).encode()).rstrip(b"=")
print((payload + b"." + base64.urlsafe_b64encode(hmac.new(secret, payload, hashlib.sha256).digest()).rstrip(b"=")).decode())
PY
)
	curl -fsS "https://loom-wire.kirk-ouimet.workers.dev/runs/${run}/events?after=0" -H "Authorization: Bearer ${token}" > "${work}/events.jsonl"
	hash=$(python3 -c "
import json, sys
for line in open(sys.argv[1]):
    event = json.loads(line)['event']
    if event['type'] == 'uploaded' and event['path'].endswith('ab.json'): print(event['sha256'])" "${work}/events.jsonl" | tail -1)
	if [ -z "${hash}" ]; then
		finish "${id}" "void: the A/B unit returned no result, Loom's fault (run ${run})"
		return
	fi
	curl -fsS "https://loom-wire.kirk-ouimet.workers.dev/runs/${run}/blobs/${hash}" -H "Authorization: Bearer ${token}" > "${work}/ab.json"
	finish "${id}" "$(python3 - "${work}/ab.json" "${run}" <<'PY'
import json, sys
result, run = json.load(open(sys.argv[1])), sys.argv[2]
runs = [result.get(label, {}) for label in ("main-before", "candidate", "main-after")]
seconds = [entry.get("seconds") for entry in runs]
lines = ["%s: %s, %s s, %s CPU-s" % (label, entry.get("action"), entry.get("seconds"), entry.get("cpuSeconds"))
         for label, entry in zip(("main before", "candidate", "main after"), runs)]
detail = "\n".join(lines + ["compared by %s; run %s" % (result.get("compareBy", "?"), run)])
if None in seconds:
    # The candidate never finishing while main does is the strongest regression there is; anything else is void.
    if seconds[1] is None and seconds[0] is not None and seconds[2] is not None:
        print("regression >%.0fx (the candidate never finished in 25 minutes)\n%s" % (1500 / max(1.0, (seconds[0] + seconds[2]) / 2), detail))
    else:
        print("void: a main run never finished, so there is no ratio\n" + detail)
else:
    ratio = seconds[1] / max(0.01, (seconds[0] + seconds[2]) / 2)
    print("%s %.1fx\n%s" % ("regression" if ratio >= 2 else "load", ratio, detail))
PY
)"
}

finish() {
	printf '%s\n' "$2" > "${jobs}/$1.verdict.partial"
	mv "${jobs}/$1.verdict.partial" "${jobs}/$1.verdict"
	rm -f "${jobs}/$1.running"
	echo "$(date -u +%H:%M:%S) $1: $(head -1 "${jobs}/$1.verdict")"
}

while true; do
	for job in "${jobs}"/*.json; do
		[ -e "${job}" ] || continue
		id=$(basename "${job}" .json)
		[ -f "${jobs}/${id}.verdict" ] || [ -f "${jobs}/${id}.running" ] && continue
		[ "$(find "${jobs}" -maxdepth 1 -name '*.running' | wc -l)" -ge "${concurrent}" ] && break
		touch "${jobs}/${id}.running"
		serve "${id}" &
	done
	sleep 10
done
