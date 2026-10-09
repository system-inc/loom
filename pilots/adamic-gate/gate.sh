#!/bin/bash
# gate.sh: Loom's whole gate of one sha, the landing record since the pool's promotion (@system_adamic, Oct 9 03:36Z).
# It publishes a running record at once (the whole-gate loops count a sha the pool has taken, running or finished, as
# the pool's; one not taken within ten minutes falls back to a box), then runs the Go test set (pregate.sh --once:
# by children, by Loom's times, disk-safe) beside every other stage as units through the gate's own run.py
# (--list-units, --phase), the census over the merged go test lines, and publishes the finished record (publish.sh).
#
#	pilots/adamic-gate/gate.sh <sha>       # a copy in ~/.loom/bin; main.sh runs it for every new main
#
# LOOM_PRIORITY ranks every unit of the gate on the shared pool (@system_adamic, Oct 9 03:37Z): the star's candidates
# 30 (pregate.sh), main's own gate 20 (main.sh), side candidates 0. Unset, it is 30 for a sha a cloud/land-train-*
# branch names on origin and 20 otherwise. A pool hands out the highest first, so a lower run yields each worker the
# star needs as its unit ends.
set -uo pipefail
sha=$1
[[ ${sha} =~ ^[0-9a-f]{40}$ ]] || { echo "gate: a full sha, please"; exit 2; }
bin=${HOME}/.loom/bin gate=${HOME}/Projects/system/adamic-gate work=${HOME}/.loom/gate/${sha}
if [ -z "${LOOM_PRIORITY:-}" ]; then
	git -C "${gate}" ls-remote origin 'refs/heads/cloud/land-train-*' 2> /dev/null | grep -q "^${sha}" && LOOM_PRIORITY=30 || LOOM_PRIORITY=20
fi
export LOOM_PRIORITY
planner=${bin}/adamic-gate loom=${bin}/loom-pregate
mkdir -p "${work}"
started=$(date -u +%Y%m%dT%H%M%SZ)
log() { echo "$(date -u +%H:%M:%S) gate ${sha:0:12}: $*"; }

# The running record: status.txt and a full.json with "runner": "pool" and "finished": false, pushed before any unit.
running() {
	local record index tree commit gitDirectory
	record=$(mktemp -d)
	echo "running: ${sha} pool gate since ${started} (Go set, phases, census on Loom's pool)" > "${record}/status.txt"
	printf '{"sha": "%s", "runner": "pool", "finished": false, "started": "%s"}\n' "${sha}" "${started}" > "${record}/full.json"
	index=$(mktemp -u)
	gitDirectory=$(git -C "${gate}" rev-parse --absolute-git-dir)
	tree=$(cd "${record}" && GIT_INDEX_FILE=${index} git --git-dir="${gitDirectory}" --work-tree=. add -A -f . && GIT_INDEX_FILE=${index} git --git-dir="${gitDirectory}" write-tree)
	commit=$(git -C "${gate}" commit-tree "${tree}" -m "Pool gate of ${sha}: running")
	git -C "${gate}" push -q origin "${commit}:refs/heads/gate-logs/${sha:0:12}/${started}/full-main"
}
running && log "running record gate-logs/${sha:0:12}/${started}/full-main" || log "couldn't push the running record"

# The tools are devtools/fast-gate's tip now; the phase list comes from run.py itself on plain worktrees.
tools=$(git -C "${gate}" ls-remote origin refs/heads/devtools/fast-gate | cut -f1)
git -C "${gate}" fetch -q origin "${sha}" "${tools}"
for pair in "tree:${sha}" "tools:${tools}"; do
	name=${pair%%:*} commit=${pair#*:}
	[ -d "${work}/${name}" ] || git -C "${gate}" worktree add -q --detach "${work}/${name}" "${commit}"
done
python3 "${work}/tools/cloud/fast-gate/run.py" --full --list-units --tree "${work}/tree" > "${work}/units.txt" 2> "${work}/list-units.err" || { log "run.py --list-units failed: $(tail -1 "${work}/list-units.err")"; }
reference=$(ls -t "${HOME}"/.loom/pregate/reference-*.jsonl.gz | head -1)
"${planner}" plan --target codex --gate-inputs "$(cat "${HOME}/.loom/gate-inputs")" --reference "${reference}" --sha "${sha}" --units 1 --only '^nothing-matches$' \
	--phases "${tools}" --phase-units "${work}/units.txt" 2> "${work}/phases-plan.err" | python3 -c "
import json, sys
job = json.load(sys.stdin); job['name'] = 'adamic-gate-phases'; json.dump(job, open(sys.argv[1], 'w'))" "${work}/phases.json"

# The Go set and the phases side by side, each on the star's pool.
log "Go set and $(wc -l < "${work}/units.txt") phase units, tools ${tools:0:12}, priority ${LOOM_PRIORITY}"
LOOM_PREGATE_WHOLE=1 "${bin}/pregate.sh" --once "${sha}" > "${work}/goset.out" 2>&1 &
goset=$!
"${loom}" run --uncached --slots none --pool codex=40 --priority "${LOOM_PRIORITY}" --record "${work}/phases-record.jsonl" "${work}/phases.json" > "${work}/phases.log" 2>&1
wait "${goset}"

# The census over the merged go test lines, by hash from the public store.
pregate=${HOME}/.loom/pregate
gzip -dc "${pregate}/${sha}.tests.jsonl.gz" > "${work}/merged.jsonl" 2> /dev/null ||
	"${planner}" reds --job "${pregate}/${sha}.job.json" --record "${pregate}/${sha}.record.jsonl" --tests "${work}/merged.jsonl" > /dev/null 2>&1
hash=$(shasum -a 256 "${work}/merged.jsonl" | cut -c1-64)
token=$(python3 - <<'PYTHON'
import base64, hashlib, hmac, json, os, time
secret = open(os.path.expanduser("~/.loom/token-secret")).read().strip().encode()
payload = base64.urlsafe_b64encode(json.dumps({"run": "census-input", "scope": "coordinator", "expires": int(time.time()) + 600}, separators=(",", ":")).encode()).rstrip(b"=")
print((payload + b"." + base64.urlsafe_b64encode(hmac.new(secret, payload, hashlib.sha256).digest()).rstrip(b"=")).decode())
PYTHON
)
curl -fsS -X PUT --data-binary @"${work}/merged.jsonl" -H "Authorization: Bearer ${token}" "https://loom-wire.kirk-ouimet.workers.dev/public/blobs/${hash}" > /dev/null
echo "census ${hash}" > "${work}/census-units.txt"
"${planner}" plan --target codex --gate-inputs "$(cat "${HOME}/.loom/gate-inputs")" --reference "${reference}" --sha "${sha}" --units 1 --only '^nothing-matches$' \
	--phases "${tools}" --phase-units "${work}/census-units.txt" 2> /dev/null | python3 -c "
import json, sys
job = json.load(sys.stdin); job['name'] = 'adamic-gate-census'; json.dump(job, open(sys.argv[1], 'w'))" "${work}/census.json"
"${loom}" run --uncached --slots none --pool codex=1 --priority "${LOOM_PRIORITY}" --record "${work}/census-record.jsonl" "${work}/census.json" > "${work}/census.log" 2>&1

# The finished record, the runs' verdicts merged (a red stays red). The Go set's units pregate.sh placed again, broken
# for Loom's own reasons, stand in for their first attempts.
again=()
[ -s "${pregate}/${sha}.again-record.jsonl" ] && again=("${pregate}/${sha}.again.json" "${pregate}/${sha}.again-record.jsonl")
"${bin}/publish.sh" "${sha}" "${tools}" "${pregate}/${sha}.job.json" "${pregate}/${sha}.record.jsonl" ${again[@]+"${again[@]}"} \
	"${work}/phases.json" "${work}/phases-record.jsonl" "${work}/census.json" "${work}/census-record.jsonl" > "${work}/publish.log" 2>&1
log "$(tail -1 "${work}/publish.log" | cut -c1-200) (wall $(( $(date +%s) - $(date -j -u -f %Y%m%dT%H%M%SZ "${started}" +%s) )) s)"
git -C "${gate}" worktree remove --force "${work}/tree" 2> /dev/null
git -C "${gate}" worktree remove --force "${work}/tools" 2> /dev/null
