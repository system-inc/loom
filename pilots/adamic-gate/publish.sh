#!/bin/bash
# publish.sh: a pool run of the whole gate as a gate-logs record, the shape the whole-gate loops and push-main read
# (cloud/full-gate-main.sh recordState): gate-logs/<sha12>/<stamp>/full-main, with status.txt's first line green,
# red or void and full.json carrying "runner": "pool" and "finished": true. One or more Loom runs make the record
# (the Go test set's, the phases', the census's); each is a job file and its record. A phase or stage unit a later
# run ran again stands in for the same unit of an earlier run only where the earlier one never finished, as a
# re-placement does inside one run (Oct 9: three of 8161285a's phase units sat on instances gone silent). A red is
# never replaced by a later green unless LOOM_PUBLISH_RERUN_RED names the unit and LOOM_PUBLISH_RERUN_WHY says why
# (a fault of the tools, fixed), and the record keeps both. The verdict is void if any run is, red if any is red,
# green only when every run is green.
#
#	pilots/adamic-gate/publish.sh <sha> <tools sha> <job> <record> [<job> <record>]...   # a copy in ~/.loom/bin
set -uo pipefail
[ $# -ge 4 ] && [ $(( ($# - 2) % 2 )) = 0 ] || { echo "usage: publish.sh <sha> <tools sha> <job> <record> [<job> <record>]..."; exit 2; }
sha=$1 tools=$2
shift 2
[[ ${sha} =~ ^[0-9a-f]{40}$ ]] || { echo "publish: a full sha, please"; exit 2; }
planner=${HOME}/.loom/bin/adamic-gate gate=${HOME}/Projects/system/adamic-gate
stamp=$(date -u +%Y%m%dT%H%M%SZ)
record=$(mktemp -d)/full-main
mkdir -p "${record}"
verdicts=() runs=() index=0
# Each job without the units a later job names again, written beside the record.
python3 - "${record}" "${LOOM_PUBLISH_RERUN_RED:-}" "${LOOM_PUBLISH_RERUN_WHY:-}" "$@" <<'PYTHON'
import json, sys
record, rerunRed, why, pairs = sys.argv[1], set(sys.argv[2].split()), sys.argv[3], sys.argv[4:]
if rerunRed and not why:
    sys.exit("publish: LOOM_PUBLISH_RERUN_RED needs LOOM_PUBLISH_RERUN_WHY")
jobs = [json.load(open(pairs[at])) for at in range(0, len(pairs), 2)]
finished = []
for at in range(1, len(pairs), 2):
    statuses = {}
    for line in open(pairs[at]):
        event = json.loads(line)
        if event.get("type") == "finished":
            statuses[event["unit"]] = event.get("status")
    finished.append(statuses)
replaced = []
for index, job in enumerate(jobs):
    # Phase and stage units stand in for each other by id. A test unit's id is only its place in its own plan, so a
    # test unit stands in only for one with the identical command (the same plan's unit, run again).
    later = {}
    for other in jobs[index + 1:]:
        for unit in other["units"]:
            later[unit["id"]] = unit
    kept = []
    for unit in job["units"]:
        status = finished[index].get(unit["id"])
        again = later.get(unit["id"])
        same = again is not None and (unit["id"].startswith(("phase-", "stage-")) or again["argv"] == unit["argv"])
        if same and (status in (None, "broken") or (status == "failed" and unit["id"] in rerunRed)):
            replaced.append({"unit": unit["id"], "was": status or "never finished", "run": index})
            continue
        kept.append(unit)
    job["units"] = kept
    json.dump(job, open("%s/job-%d.json" % (record, index), "w"))
json.dump({"replaced": replaced, "why_red_was_rerun": why or None}, open(record + "/replaced.json", "w"), indent=1)
PYTHON
while [ $# -gt 0 ]; do
	job=${record}/job-${index}.json events=$2
	shift 2
	"${planner}" reds --job "${job}" --record "${events}" --tests "${record}/part-${index}.jsonl" > "${record}/reds-${index}.txt" 2>&1
	case $? in 0) verdicts+=(green) ;; 1) verdicts+=(red) ;; *) verdicts+=(void) ;; esac
	runs+=("$(head -1 "${record}/reds-${index}.txt" | awk '{print $2}' | tr -d :)")
	gzip -9c "${events}" > "${record}/record-${index}.jsonl.gz"
	# Each phase unit's own out directory (run.py's full.json and logs), fetched from the run's store by hash.
	run=${runs[${#runs[@]}-1]}
	token=$(python3 - "${run}" <<'PYTHON'
import base64, hashlib, hmac, json, os, sys, time
secret = open(os.path.expanduser("~/.loom/token-secret")).read().strip().encode()
payload = base64.urlsafe_b64encode(json.dumps({"run": sys.argv[1], "scope": "coordinator", "expires": int(time.time()) + 600}, separators=(",", ":")).encode()).rstrip(b"=")
print((payload + b"." + base64.urlsafe_b64encode(hmac.new(secret, payload, hashlib.sha256).digest()).rstrip(b"=")).decode())
PYTHON
)
	python3 - "${job}" "${events}" <<'PYTHON' | while read -r unit hash; do
import json, sys
units = {unit["id"] for unit in json.load(open(sys.argv[1]))["units"] if unit["id"].startswith("phase-")}
for line in open(sys.argv[2]):
    event = json.loads(line)
    if event.get("unit") in units and event.get("type") == "uploaded" and event.get("path") == "loom-out/phase.tar.gz":
        print(event["unit"], event["sha256"])
PYTHON
		mkdir -p "${record}/phases/${index}/${unit}"
		curl -fsS "https://loom-wire.kirk-ouimet.workers.dev/runs/${run}/blobs/${hash}" -H "Authorization: Bearer ${token}" | tar -xzf - -C "${record}/phases/${index}/${unit}" || echo "publish: ${unit}'s output unreadable" >> "${record}/publish-problems.txt"
	done
	index=$((index + 1))
done
# The go test lines: the Go set's units', then each phase unit's own (the wasi fixtures run TestWASI).
cat "${record}"/part-*.jsonl $(ls "${record}"/phases/*/*/phase/test.jsonl 2> /dev/null) > "${record}/test.jsonl"
rm -f "${record}"/part-*.jsonl
verdict=green
for one in "${verdicts[@]}"; do
	[ "${one}" = red ] && [ "${verdict}" = green ] && verdict=red
	[ "${one}" = void ] && verdict=void
done
summary=$(for file in "${record}"/reds-*.txt; do head -1 "${file}" | cut -d, -f2-; done | paste -sd ';' -)
cat "${record}"/reds-*.txt | grep -E '^(FAIL|BROKEN) ' | head -1 > "${record}/first-failure.txt"
# The whole gate's record: what push-main --full-gate checks of a box's, from the runs and their units (the verdict
# printed last is the record's, which can only be stricter than the runs').
merged=$(python3 - "${record}" "${sha}" "${tools}" "${verdict}" "${runs[@]}" <<'PYTHON'
import glob, gzip, json, os, sys
record, sha, tools, verdict, runs = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5:]
stages = ["coverage", "tools", "build", "vet", "tests", "wasi", "stage3", "catalog", "determinism", "census"]
steps, exits, catalogRows, problems = {}, {}, [], []
# The phases: each unit's run.py full.json carries its own stage's seconds and exit. Units of one stage ran side by
# side, so the stage's seconds are its longest unit's and its exit the worst.
for path in sorted(glob.glob(record + "/phases/*/*/phase/full.json")):
    unit = json.load(open(path))
    for stage, seconds in (unit.get("steps_seconds") or {}).items():
        steps[stage] = max(steps.get(stage, 0.0), seconds)
    for stage, code in (unit.get("stages_exit") or {}).items():
        exits[stage] = code if exits.get(stage, 0) == 0 else exits[stage]
    catalogRows += unit.get("catalog_units") or []
# The catalog proves something only if some entry caught its mutation: over every unit's rows, as the whole gate does.
if "catalog" in exits and not any(row.get("caught") and row.get("ok") for row in catalogRows):
    exits["catalog"] = 1
    problems.append("no catalog entry caught its mutation across %d catalog units" % len(catalogRows))
# The Go test set: green when its runs were, its seconds from its first event to its last.
events, testTimes = [], []
for path in sorted(glob.glob(record + "/record-*.jsonl.gz")):
    for line in gzip.open(path, "rt"):
        event = json.loads(line)
        events.append(event)
        if event.get("unit", "").startswith("tests-"):
            testTimes.append(event["time"])
def seconds(first, last):
    from datetime import datetime
    parse = lambda value: datetime.strptime(value[:19], "%Y-%m-%dT%H:%M:%S")
    return round((parse(last) - parse(first)).total_seconds(), 1)
if testTimes:
    steps["tests"] = seconds(min(testTimes), max(testTimes))
    exits["tests"] = 0 if verdict == "green" or not any(line.startswith("FAIL ") and "(stage)" not in line for path in glob.glob(record + "/reds-*.txt") for line in open(path)) else 1
# Each test once, by its last word: a split parent reports in every unit holding its children.
outcomes = {}
for line in open(record + "/test.jsonl"):
    try:
        event = json.loads(line)
    except ValueError:
        continue
    if event.get("Test") and event.get("Action") in ("pass", "fail", "skip"):
        key = (event.get("Package"), event["Test"])
        if outcomes.get(key) != "fail":
            outcomes[key] = event["Action"]
counts = {action: sum(1 for value in outcomes.values() if value == action) for action in ("pass", "fail", "skip")}
missing = [stage for stage in stages if stage not in exits]
if missing:
    problems.append("stages the runs held no unit for: " + ", ".join(missing))
green = verdict == "green" and not missing and all(exits[stage] == 0 for stage in stages) and counts["fail"] == 0
times = [event["time"] for event in events if event.get("time")]
summary = {"sha": sha, "base": sha, "tools_sha": tools, "runner": "pool", "finished": True,
           "verdict": "green" if green else ("void" if verdict == "void" or missing else "red"),
           "fail": counts["fail"], "pass": counts["pass"], "skip": counts["skip"],
           "build_ok": exits.get("build") == 0, "vet_ok": exits.get("vet") == 0, "uncached_tests": True, "packages": "all",
           "wall_seconds": seconds(min(times), max(times)) if times else 0.0,
           "steps_seconds": {stage: steps.get(stage, 0.0) for stage in stages if stage in exits},
           "stages_exit": {stage: exits[stage] for stage in stages if stage in exits},
           "planned_stages": stages, "failure": "; ".join(problems) or None, "runs": runs,
           "replaced_units": json.load(open(record + "/replaced.json"))}
if missing:
    summary["covers"] = sorted(set(stages) - set(missing))
json.dump(summary, open(record + "/full.json", "w"), indent=1)
print(summary["verdict"])
PYTHON
)
gzip -9 "${record}/test.jsonl"
echo "${merged}: ${sha} pool gate (runner pool, ${#runs[@]} runs:${summary})" > "${record}/status.txt"
grep -q '"runner": "pool"' "${record}/full.json" && grep -q '"finished": true' "${record}/full.json" || { echo "publish: full.json lacks what recordState reads"; exit 2; }
# LOOM_PUBLISH_DRY=1 builds the record and stops before the push, printing where it is.
[ "${LOOM_PUBLISH_DRY:-}" = 1 ] && { echo "dry run: ${record} $(head -1 "${record}/status.txt")"; exit 0; }
branch=gate-logs/${sha:0:12}/${stamp}/full-main
index=$(mktemp -u)
gitDirectory=$(git -C "${gate}" rev-parse --absolute-git-dir)
tree=$(cd "${record}" && GIT_INDEX_FILE=${index} git --git-dir="${gitDirectory}" --work-tree=. add -A -f . && GIT_INDEX_FILE=${index} git --git-dir="${gitDirectory}" write-tree)
commit=$(git -C "${gate}" commit-tree "${tree}" -m "Pool gate of ${sha}: $(head -1 "${record}/status.txt")")
git -C "${gate}" push -q origin "${commit}:refs/heads/${branch}" || { echo "publish: push of ${branch} failed"; exit 2; }
echo "${branch} $(head -1 "${record}/status.txt")"
