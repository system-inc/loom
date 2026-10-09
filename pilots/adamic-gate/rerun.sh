#!/bin/bash
# rerun.sh: a whole gate at a later sha that runs only what moved, on top of a finished run's kept verdicts (Kirk, Oct
# 9 03:17Z; #apf5vdb). A test killed at 90 s is fixed or split on a test-only change, or a green star candidate gains
# main's test-only landings: the units whose input hash moved (inputs.py, loom-inputs-v1) and the units that weren't
# green run again at the new sha, every other unit keeps the base run's verdict, a fresh census reads the kept and the
# rerun units' go test lines, and the record (publish.sh) names its base as rerun_of. push-main accepts it only when
# every unit not rerun has the base's hash, every rerun unit is green and the new sha descends from the base's
# (developer tools' #hpjftdj).
#
#	pilots/adamic-gate/rerun.sh <base record ref> <sha>    # the ref is gate-logs/<sha12>/<stamp>/full-main; a copy in ~/.loom/bin
#
# LOOM_PRIORITY ranks the units on the pool, 30 (the star's) unless set.
set -uo pipefail
base=$1 sha=$2
[[ ${sha} =~ ^[0-9a-f]{40}$ ]] || { echo "rerun: a full sha, please"; exit 2; }
[[ ${base} =~ ^gate-logs/[0-9a-f]{12}/[0-9TZ]+/full-main$ ]] || { echo "rerun: the base is a gate-logs/<sha12>/<stamp>/full-main ref"; exit 2; }
bin=${HOME}/.loom/bin gate=${HOME}/Projects/system/adamic-gate work=${HOME}/.loom/rerun/${sha}
planner=${bin}/adamic-gate loom=${bin}/loom-pregate priority=${LOOM_PRIORITY:-30}
mkdir -p "${work}/base"
log() { echo "$(date -u +%H:%M:%S) rerun ${sha:0:12}: $*"; }

# The base record, whole: its full.json, its jobs and its runs' records.
git -C "${gate}" fetch -q origin "refs/heads/${base}" && git -C "${gate}" archive FETCH_HEAD | tar -x -C "${work}/base" || { log "can't read ${base}"; exit 2; }
baseSha=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["sha"])' "${work}/base/full.json")
tools=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["tools_sha"])' "${work}/base/full.json")
git -C "${gate}" fetch -q origin "${sha}" "${baseSha}" || { log "can't fetch ${sha}"; exit 2; }
for record in "${work}"/base/record-*.jsonl.gz; do gzip -dc "${record}" > "${record%.gz}"; done

# The base's units and verdicts, by its own record where it lists them, else read again from its runs' finished
# events the way publish.sh reads them; its census job aside (the census reads every kept and rerun unit's lines).
python3 - "${work}" <<'PYTHON' || { log "the base record is not one a rerun can keep"; exit 2; }
import glob, json, os, sys
work = sys.argv[1]
summary = json.load(open(work + "/base/full.json"))
if summary.get("verdict") == "void":
    sys.exit("a void base has no verdicts to keep")
verdicts, jobs, census = {}, [], []
for path in sorted(glob.glob(work + "/base/job-*.json"), key=lambda path: int(path.rsplit("-", 1)[1].split(".")[0])):
    index = path.rsplit("-", 1)[1].split(".")[0]
    job = json.load(open(path))
    isCensus = all(unit["argv"][3:4] == ["adamic-gate-phase"] and unit["argv"][6:7] == ["census"] for unit in job["units"])
    (census if isCensus else jobs).append(index)
    for line in open("%s/base/record-%s.jsonl" % (work, index)):
        event = json.loads(line)
        if event.get("type") == "finished" and any(unit["id"] == event["unit"] for unit in job["units"]):
            verdicts[event["unit"]] = "killed" if event.get("timedOut") else event.get("status")
for unit in summary.get("units") or []:
    verdicts[unit["id"]] = unit["verdict"]
json.dump(verdicts, open(work + "/base-verdicts.json", "w"))
open(work + "/base-jobs.txt", "w").write(" ".join(jobs) + "\n")
open(work + "/base-census.txt", "w").write(" ".join(census) + "\n")
PYTHON
read -r -a jobs < "${work}/base-jobs.txt"
read -r -a census < "${work}/base-census.txt"
jobFiles=() && for index in "${jobs[@]}"; do jobFiles+=("${work}/base/job-${index}.json"); done
python3 "${bin}/inputs.py" rerun-plan --base-sha "${baseSha}" --sha "${sha}" --verdicts "${work}/base-verdicts.json" --out "${work}/plan" "${jobFiles[@]}" || { log "rerun-plan refused"; exit 2; }
log "$(grep -c ' rerun' "${work}/plan/plan.txt") units run again on ${base}'s kept verdicts, priority ${priority}"

# The units that run again, each base job's own, side by side on the star's pool.
pairs=() pids=()
for index in "${jobs[@]}"; do
	pairs+=("${work}/base/job-${index}.json" "${work}/base/record-${index}.jsonl")
done
baseJobs=${#jobs[@]}
for index in "${!jobs[@]}"; do
	job=${work}/plan/rerun-${index}.json
	[ "$(python3 -c 'import json, sys; print(len(json.load(open(sys.argv[1]))["units"]))' "${job}")" = 0 ] && continue
	units=$(python3 -c 'import json, sys; print(len(json.load(open(sys.argv[1]))["units"]))' "${job}")
	"${loom}" run --uncached --slots none --pool "codex=$((units < 60 ? units : 60))" --priority "${priority}" --record "${work}/rerun-${index}.record.jsonl" "${job}" > "${work}/rerun-${index}.log" 2>&1 &
	pids+=($!)
	pairs+=("${job}" "${work}/rerun-${index}.record.jsonl")
done
for pid in ${pids[@]+"${pids[@]}"}; do wait "${pid}"; done

# The census: over the kept and the rerun test units' lines when anything ran again, else the base's own stands.
if [ ${#pids[@]} = 0 ]; then
	for index in "${census[@]}"; do pairs+=("${work}/base/job-${index}.json" "${work}/base/record-${index}.jsonl"); done
	baseJobs=$((baseJobs + ${#census[@]}))
else
	: > "${work}/merged.jsonl"
	python3 - "${work}" "${pairs[@]}" <<'PYTHON'
import json, sys
work, pairs = sys.argv[1], sys.argv[2:]
# Each job without the units a later job runs again, the way publish.sh keeps them, for its go test lines.
jobs = [json.load(open(pairs[at])) for at in range(0, len(pairs), 2)]
for index, job in enumerate(jobs):
    later = {unit["id"] for other in jobs[index + 1:] for unit in other["units"]}
    job["units"] = [unit for unit in job["units"] if unit["id"] not in later]
    json.dump(job, open("%s/kept-%d.json" % (work, index), "w"))
PYTHON
	for at in $(seq 0 $(( ${#pairs[@]} / 2 - 1 ))); do
		"${planner}" reds --job "${work}/kept-${at}.json" --record "${pairs[$((at * 2 + 1))]}" --tests "${work}/lines-${at}.jsonl" > /dev/null 2>&1
		cat "${work}/lines-${at}.jsonl" >> "${work}/merged.jsonl" 2> /dev/null
	done
	hash=$(shasum -a 256 "${work}/merged.jsonl" | cut -c1-64)
	token=$(python3 - <<'PYTHON'
import base64, hashlib, hmac, json, os, time
secret = open(os.path.expanduser("~/.loom/token-secret")).read().strip().encode()
payload = base64.urlsafe_b64encode(json.dumps({"run": "census-input", "scope": "coordinator", "expires": int(time.time()) + 600}, separators=(",", ":")).encode()).rstrip(b"=")
print((payload + b"." + base64.urlsafe_b64encode(hmac.new(secret, payload, hashlib.sha256).digest()).rstrip(b"=")).decode())
PYTHON
)
	curl -fsS -X PUT --data-binary @"${work}/merged.jsonl" -H "Authorization: Bearer ${token}" "https://loom-wire.kirk-ouimet.workers.dev/public/blobs/${hash}" > /dev/null || { log "census input upload failed"; exit 2; }
	echo "census ${hash}" > "${work}/census-units.txt"
	reference=$(ls -t "${HOME}"/.loom/pregate/reference-*.jsonl.gz | head -1)
	"${planner}" plan --target codex --gate-inputs "$(cat "${HOME}/.loom/gate-inputs")" --reference "${reference}" --sha "${sha}" --units 1 --only '^nothing-matches$' \
		--phases "${tools}" --phase-units "${work}/census-units.txt" 2> /dev/null | python3 -c "
import json, sys
job = json.load(sys.stdin); job['name'] = 'adamic-gate-census'; json.dump(job, open(sys.argv[1], 'w'))" "${work}/census.json"
	"${loom}" run --uncached --slots none --pool codex=1 --priority "${priority}" --record "${work}/census-record.jsonl" "${work}/census.json" > "${work}/census.log" 2>&1
	pairs+=("${work}/census.json" "${work}/census-record.jsonl")
fi

# The record: the base's jobs first, then the reruns standing in for their units by id, then the census.
LOOM_PUBLISH_RERUN_OF=${base} LOOM_PUBLISH_RERUN_BASE_JOBS=${baseJobs} "${bin}/publish.sh" "${sha}" "${tools}" "${pairs[@]}" > "${work}/publish.log" 2>&1
log "$(tail -1 "${work}/publish.log" | cut -c1-240)"
