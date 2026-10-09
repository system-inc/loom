#!/bin/bash
# requeuetest.sh [<requeue.sh>]: requeue.sh against a temporary jobs directory and a stub fast.sh that records its
# arguments (#rrkfa72), one scenario per case, PASS or FAIL each. A decided job has its verdict, placed signal and stale
# selection moved aside and its tier and tools set, then fast.sh --once starts; a running job, a missing job file and a
# malformed sha or tools are refused with nothing moved. The mutant that keeps the stale select.json must fail:
#
#	pilots/adamic-gate/fastretry/requeuetest.sh                   # the repo's requeue.sh
#	sed '/aside "${work}\/select\/select.json"/d' requeue.sh > m.sh && fastretry/requeuetest.sh m.sh   # fails 2
set -u
here=$(cd "$(dirname "$0")" && pwd) failures=0
requeue=${1:-${here}/../requeue.sh}
sha=1111111111111111111111111111111111111111 tools=2222222222222222222222222222222222222222
check() { if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; sed "s/^/    /" "${T}/out.log"; failures=$((failures + 1)); fi; }
# scenario builds a jobs directory holding one decided job, a bin with the real detach.sh and a stub fast.sh that
# records its arguments and touches .running as --once does, and a log of its own: nothing reaches ~/.loom.
scenario() {
	T=$(mktemp -d)
	export LOOM_FAST_JOBS=${T}/jobs LOOM_BIN=${T}/bin LOOM_FAST_LOG=${T}/fast.log STUB_CALLS=${T}/calls
	jobs=${LOOM_FAST_JOBS} work=${T}/jobs/${sha}.work
	mkdir -p "${work}/select" "${LOOM_BIN}"
	cp "${here}/../detach.sh" "${LOOM_BIN}/detach.sh"
	cat > "${LOOM_BIN}/fast.sh" <<'STUB'
#!/bin/bash
echo "$*" >> "${STUB_CALLS}"
touch "${LOOM_FAST_JOBS}/$2.running"
STUB
	chmod +x "${LOOM_BIN}/fast.sh"
	echo '{"branch": "cloud/x", "sha": "'"${sha}"'", "packages": "select", "priority": 0, "tools": "", "gate": "3333333333333333333333333333333333333333"}' > "${jobs}/${sha}.json"
	echo "void: ${sha} stopped at its ceiling" > "${jobs}/${sha}.verdict"
	echo "3 12" > "${jobs}/${sha}.placed"
	echo tgz > "${work}/select.tgz"
	echo '{"packages": ["stale"]}' > "${work}/select/select.json"
}
# calls waits up to 3 s for the stub's record, since fast.sh starts detached and may not have run when requeue returns.
calls() {
	local waited=0
	while [ ! -s "${T}/calls" ] && [ "${waited}" -lt 30 ]; do sleep 0.1; waited=$((waited + 1)); done
	cat "${T}/calls" 2> /dev/null
}
# moved <path>: the original is gone and exactly one copy named <path>.requeued-<HHMMSS> holds its old contents.
moved() { [ ! -e "$1" ] && [ "$(find "$(dirname "$1")" -maxdepth 1 -name "$(basename "$1").requeued-[0-9][0-9][0-9][0-9][0-9][0-9]" | wc -l | tr -d ' ')" = 1 ]; }
untouched() {
	[ -e "${jobs}/${sha}.verdict" ] && [ -e "${jobs}/${sha}.placed" ] && [ -e "${work}/select.tgz" ] && [ -e "${work}/select/select.json" ] &&
		[ -z "$(find "${jobs}" -name '*.requeued-*')" ] && [ ! -e "${T}/calls" ]
}
field() { python3 -c 'import json, sys; print(json.load(open(sys.argv[1])).get(sys.argv[2]))' "${jobs}/${sha}.json" "$1"; }

# 1. A decided job: everything that keeps it decided or feeds it a stale selection moved aside, tier and tools set as
# the JSON types serve reads (an integer priority), its other fields kept, and fast.sh --once called with its sha.
scenario
bash "${requeue}" "${sha}" --tier 30 --tools "${tools}" > "${T}/out.log" 2>&1
code=$?
check decided-exit-0 '[ "${code}" = 0 ]'
check decided-verdict-aside 'moved "${jobs}/${sha}.verdict" && grep -q "^void: " "${jobs}/${sha}.verdict".requeued-*'
check decided-placed-aside 'moved "${jobs}/${sha}.placed"'
check decided-select-tgz-aside 'moved "${work}/select.tgz"'
check decided-select-json-aside 'moved "${work}/select/select.json"'
check decided-tier-tools-set 'python3 -c "import json, sys; job = json.load(open(\"${jobs}/${sha}.json\")); sys.exit(0 if job[\"priority\"] == 30 and job[\"tools\"] == \"${tools}\" and job[\"packages\"] == \"select\" and job[\"gate\"] == \"3\" * 40 else 1)"'
check decided-once-called '[ "$(calls)" = "--once ${sha}" ]'
check decided-logs-each-action '[ "$(grep -c "^requeue: 111111111111 " "${T}/out.log")" = 7 ] && grep -q "moved ${sha}.verdict aside to ${sha}.verdict.requeued-" "${T}/out.log"'

# 2. A running job is refused, and nothing moves, changes or starts.
scenario
touch "${jobs}/${sha}.running"
bash "${requeue}" "${sha}" --tier 30 > "${T}/out.log" 2>&1
code=$?
check running-refused '[ "${code}" = 1 ] && grep -q "is running" "${T}/out.log"'
check running-nothing-moved 'untouched && [ "$(field priority)" = 0 ]'

# 3. A sha with no job file is refused.
scenario
bash "${requeue}" 4444444444444444444444444444444444444444 > "${T}/out.log" 2>&1
code=$?
check missing-job-refused '[ "${code}" = 1 ] && grep -q "has no job file" "${T}/out.log" && untouched'

# A malformed sha, tools value or tier is refused before anything moves.
scenario
bash "${requeue}" 111111111111 > "${T}/out.log" 2>&1
code=$?
check short-sha-refused '[ "${code}" = 1 ] && untouched'
scenario
bash "${requeue}" "${sha}" --tools 22222222 > "${T}/out.log" 2>&1
code=$?
check short-tools-refused '[ "${code}" = 1 ] && untouched && [ "$(field tools)" = "" ]'
scenario
bash "${requeue}" "${sha}" --tier 1001 > "${T}/out.log" 2>&1
code=$?
check tier-out-of-range-refused '[ "${code}" = 1 ] && untouched && [ "$(field priority)" = 0 ]'

# A cancelled job is an explicit ask to run it again: its .cancelled is moved aside too, and with no flags the job file
# keeps its fields.
scenario
touch "${jobs}/${sha}.cancelled"
bash "${requeue}" "${sha}" > "${T}/out.log" 2>&1
code=$?
check cancelled-aside '[ "${code}" = 0 ] && moved "${jobs}/${sha}.cancelled" && moved "${jobs}/${sha}.verdict" && [ "$(field priority)" = 0 ] && [ "$(calls)" = "--once ${sha}" ]'

# A second requeue in the same second never overwrites the first one's copy: copies stand at this second's name and the
# next two (macOS date), so requeue's stamp meets one wherever the clock is when it runs.
scenario
for offset in 0 1 2; do echo "earlier" > "${jobs}/${sha}.verdict.requeued-$(date -u -v+${offset}S +%H%M%S)"; done
bash "${requeue}" "${sha}" > "${T}/out.log" 2>&1
code=$?
check same-second-kept '[ "${code}" = 0 ] && [ ! -e "${jobs}/${sha}.verdict" ] && [ "$(cat "${jobs}/${sha}".verdict.requeued-* | sort | uniq -c | tr -s " " | tr "\n" "|")" = " 3 earlier| 1 void: ${sha} stopped at its ceiling|" ]'
echo "failures: ${failures}"
exit $((failures > 0))
