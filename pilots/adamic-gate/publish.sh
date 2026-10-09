#!/bin/bash
# publish.sh: a pool run of the whole gate as a gate-logs record, the shape the whole-gate loops and push-main read
# (cloud/full-gate-main.sh recordState): gate-logs/<sha12>/<stamp>/full-main, with status.txt's first line green,
# red or void and full.json carrying "runner": "pool" and "finished": true. One or more Loom runs make the record
# (the Go test set's, the phases', the census's); each is a job file and its record. The verdict is void if any run
# is, red if any is red, green only when every run is green. covers names what the runs held.
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
while [ $# -gt 0 ]; do
	job=$1 events=$2
	shift 2
	"${planner}" reds --job "${job}" --record "${events}" --tests "${record}/part-${index}.jsonl" > "${record}/reds-${index}.txt" 2>&1
	case $? in 0) verdicts+=(green) ;; 1) verdicts+=(red) ;; *) verdicts+=(void) ;; esac
	runs+=("$(head -1 "${record}/reds-${index}.txt" | awk '{print $2}' | tr -d :)")
	cp "${job}" "${record}/job-${index}.json"
	gzip -9c "${events}" > "${record}/record-${index}.jsonl.gz"
	index=$((index + 1))
done
cat "${record}"/part-*.jsonl | gzip -9 > "${record}/test.jsonl.gz"
rm -f "${record}"/part-*.jsonl
verdict=green
for one in "${verdicts[@]}"; do
	[ "${one}" = red ] && [ "${verdict}" = green ] && verdict=red
	[ "${one}" = void ] && verdict=void
done
summary=$(for file in "${record}"/reds-*.txt; do head -1 "${file}" | cut -d, -f2-; done | paste -sd ';' -)
echo "${verdict}: ${sha} pool gate (runner pool, ${#runs[@]} runs:${summary})" > "${record}/status.txt"
cat "${record}"/reds-*.txt | grep -E '^(FAIL|BROKEN) ' | head -1 > "${record}/first-failure.txt"
python3 - "${record}" "${sha}" "${tools}" "${verdict}" "${runs[@]}" <<'PYTHON'
import json, sys, glob
record, sha, tools, verdict, runs = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5:]
covers = set()
for path in sorted(glob.glob(record + "/job-*.json")):
    for unit in json.load(open(path))["units"]:
        name = unit["id"]
        if name.startswith("tests-"):
            covers.add("go-tests")
        elif name.startswith("phase-"):
            covers.add(unit["argv"][6])  # the phase, as run.py names it
        elif name.startswith("stage-"):
            covers.add(name[len("stage-"):])
summary = {"sha": sha, "tools_sha": tools, "runner": "pool", "finished": True, "verdict": verdict,
           "covers": sorted(covers), "runs": runs}
json.dump(summary, open(record + "/full.json", "w"), indent=1)
PYTHON
grep -q '"runner": "pool"' "${record}/full.json" && grep -q '"finished": true' "${record}/full.json" || { echo "publish: full.json lacks what recordState reads"; exit 2; }
branch=gate-logs/${sha:0:12}/${stamp}/full-main
index=$(mktemp -u)
gitDirectory=$(git -C "${gate}" rev-parse --absolute-git-dir)
tree=$(cd "${record}" && GIT_INDEX_FILE=${index} git --git-dir="${gitDirectory}" --work-tree=. add -A -f . && GIT_INDEX_FILE=${index} git --git-dir="${gitDirectory}" write-tree)
commit=$(git -C "${gate}" commit-tree "${tree}" -m "Pool gate of ${sha}: $(head -1 "${record}/status.txt")")
git -C "${gate}" push -q origin "${commit}:refs/heads/${branch}" || { echo "publish: push of ${branch} failed"; exit 2; }
echo "${branch} $(head -1 "${record}/status.txt")"
