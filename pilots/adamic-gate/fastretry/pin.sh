#!/bin/bash
# pin.sh [<fast.sh>]: a job runs to its verdict on the tools it was served with (#8xfsf4x), against a real fast server and
# promote.sh in a fake home, with stub tools. Two sets differ only in a SET file their stub verify.sh and completion.py
# report. Job 1 is served on set A and held in verify.sh; set B is promoted (promote.sh, LOOM_PROMOTE_RESTART=0) and the
# server restarted as promote.sh restarts it (its process group killed, a new one started). Job 1 must survive, never be
# served again, finish on A (its completion.py A's, run after the swap) and its record name A; job 2, served after the
# promotion, runs on B. A canary's --once, LOOM_BIN already a snapshot, pins to that snapshot. The mutants (the job
# reading the live LOOM_BIN instead of its snapshot; the job a subshell of the server, the old code) must fail:
#
#	pilots/adamic-gate/fastretry/pin.sh
#	sed '/|| exec env LOOM_BIN=/d' fast.sh > m.sh && fastretry/pin.sh m.sh                                  # fails 4
#	sed 's/^		launch "\${sha}"$/		within "${sha}" \&/' fast.sh > m.sh && fastretry/pin.sh m.sh   # fails 7
set -u
here=$(cd "$(dirname "$0")" && pwd) failures=0
fast=$(cd "$(dirname "${1:-${here}/../fast.sh}")" && pwd)/$(basename "${1:-${here}/../fast.sh}")
promote=${here}/../promote.sh
T=$(mktemp -d)
check() { if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; sed "s/^/    /" "${T}/stub.log" 2> /dev/null; failures=$((failures + 1)); fi; }
# Everything lives under the fake home: the server, the jobs, the live set and the snapshots. Nothing reaches ~/.loom.
export HOME=${T}/home STUBLOG=${T}/stub.log STUBGO=${T}/go LOOM_FAST_PUBLISH=0
unset LOOM_BIN LOOM_FAST_JOBS LOOM_TOOLS_PINNED LOOM_TOOLS_PROMOTED LOOM_LIVE_BIN
jobs=${HOME}/.loom/jobs/fast live=${HOME}/.loom/bin canary=${HOME}/.loom/canary
mkdir -p "${jobs}" "${live}" "${canary}/pass" "${STUBGO}"
# canary.sh's hash, written out again here so the record is checked against it, not against fast.sh's own.
hash() {
	python3 - "$1" <<'PY'
import hashlib, os, sys
root = sys.argv[1]
lines = []
for name in sorted(os.listdir(root)):
    path = os.path.join(root, name)
    if name.startswith(".") or name == "__pycache__" or name.endswith(".pyc") or not os.path.isfile(path):
        continue
    lines.append("%s %s\n" % (hashlib.sha256(open(path, "rb").read()).hexdigest(), name))
print(hashlib.sha256("".join(lines).encode()).hexdigest())
PY
}
# makeset <dir> <name>: the fast.sh under test with stubs. verify.sh logs its set and LOOM_BIN, then holds until
# $STUBGO/<sha> exists; completion.py, which finish runs after verify.sh, logs its own set.
makeset() {
	mkdir -p "$1" && cp "${fast}" "$1/fast.sh" && echo "$2" > "$1/SET"
	cat > "$1/verify.sh" <<'STUB'
#!/bin/bash
set=$(cat "$(dirname "$0")/SET") work=${LOOM_VERIFY_WORK} sha=$(basename "${LOOM_VERIFY_WORK}" .work)
echo "verify ${sha} set ${set} bin ${LOOM_BIN}" >> "${STUBLOG}"
for ((i = 0; i < 600; i++)); do [ -e "${STUBGO}/${sha}" ] && break; sleep 0.1; done
echo "run stub-run-${set}: 1 units on 1 slots, uncached" > "${work}/run.log"
echo passed > "${work}/build.verdict"; echo 0 > "${work}/reds.exit"; : > "${work}/reds.txt"
echo '{"Package":"p","Test":"TestA","Action":"pass"}' > "${work}/test.jsonl"
STUB
	cat > "$1/completion.py" <<'STUB'
import json, os, sys
name = open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "SET")).read().strip()
open(os.environ["STUBLOG"], "a").write("completion %s set %s\n" % (json.load(open(os.path.join(sys.argv[1], "fast.json")))["sha"], name))
STUB
	printf 'import sys\n' > "$1/placed.py"
	chmod +x "$1/fast.sh" "$1/verify.sh"
}
job() { echo "{\"branch\": \"pin/$1\", \"sha\": \"$1\", \"packages\": [\"p\"], \"priority\": 0, \"tools\": \"4444444444444444444444444444444444444444\"}" > "${jobs}/$1.json"; }
# server: the live fast.sh as launchd runs it, the leader of a process group of its own; stopping it kills that group,
# as launchctl bootout does.
server() { python3 -c 'import os, sys; os.setsid(); os.execvp(sys.argv[1], sys.argv[1:])' bash "${live}/fast.sh" > "${T}/server-$1.log" 2>&1 & }
waitfor() { local i; for ((i = 0; i < $2 * 10; i++)); do eval "$1" && return 0; sleep 0.1; done; return 1; }
record() { python3 -c 'import glob, json, sys; print(json.load(open(sorted(glob.glob(sys.argv[1] + "/record-*/fast.json"))[-1])).get(sys.argv[2], ""))' "${jobs}/$1.work" "$2" 2> /dev/null; }
one=1111111111111111111111111111111111111111 two=2222222222222222222222222222222222222222 three=3333333333333333333333333333333333333333

makeset "${T}/A" A && makeset "${T}/B" B
hashA=$(hash "${T}/A") hashB=$(hash "${T}/B")
cp -p "${T}/A"/* "${live}/"
# Job 1 is served on A and held in its verify.sh.
job "${one}"
server 1
first=$! && disown
waitfor 'grep -q "^verify ${one} " "${STUBLOG}" 2> /dev/null' 20
check served-on-As-snapshot 'grep -qx "verify ${one} set A bin ${canary}/tools-${hashA}" "${STUBLOG}"'
# B is promoted under it, canaried as promote.sh requires: a pass and its snapshot.
cp -Rp "${T}/B" "${canary}/tools-${hashB}" && echo '{"verdict": "green: stub canary", "mutants": {"stub": "ok"}}' > "${canary}/pass/${hashB}"
LOOM_PROMOTE_RESTART=0 bash "${promote}" "${T}/B" > "${T}/promote.log" 2>&1
check promoted-B '[ "$(cat "${live}/SET")" = B ] && [ "$(cut -d" " -f1 "${live}/.promoted")" = "${hashB}" ] && [ "$(hash "${live}")" = "${hashB}" ]'
# The restart: the old server's group killed, then a new server, with job 2 waiting for it.
kill -TERM -- "-${first}" 2> /dev/null
waitfor '! kill -0 "${first}" 2> /dev/null' 10
sleep 1
check job-1-outlives-the-server 'pgrep -f "fast\.sh --served ${one}" > /dev/null && [ -e "${jobs}/${one}.running" ]'
job "${two}" && touch "${STUBGO}/${two}"
server 2
second=$! && disown
waitfor '[ -e "${jobs}/${two}.verdict" ]' 30
touch "${STUBGO}/${one}"
waitfor '[ -e "${jobs}/${one}.verdict" ]' 30
check job-1-served-once '[ "$(grep -c "^verify ${one} " "${STUBLOG}")" = 1 ]'
check job-1-finishes-on-A 'grep -qx "completion ${one} set A" "${STUBLOG}" && ! grep -q "completion ${one} set B" "${STUBLOG}" && grep -q "^green: ${one} " "${jobs}/${one}.verdict"'
check job-1-record-names-A '[ "$(record "${one}" loom_tools)" = "${hashA}" ] && [ "$(cut -d" " -f1 "${jobs}/${one}.work/loom-tools")" = "${hashA}" ] && [ "$(record "${one}" gate_tools)" = 4444444444444444444444444444444444444444 ]'
check job-2-on-B 'grep -qx "verify ${two} set B bin ${canary}/tools-${hashB}" "${STUBLOG}" && grep -qx "completion ${two} set B" "${STUBLOG}" && grep -q "^green: ${two} " "${jobs}/${two}.verdict"'
check job-2-record-names-B '[ "$(record "${two}" loom_tools)" = "${hashB}" ] && [ "$(record "${two}" loom_tools_promoted)" = "${hashB}" ]'
kill -TERM -- "-${second}" 2> /dev/null
# A canary's --once: LOOM_BIN is already A's snapshot, so it runs there and makes no new one.
[ -d "${canary}/tools-${hashA}" ] || cp -Rp "${T}/A" "${canary}/tools-${hashA}"
snapshots=$(find "${canary}" -maxdepth 1 -name 'tools-*' | wc -l | tr -d ' ')
job "${three}" && touch "${STUBGO}/${three}"
LOOM_BIN=${canary}/tools-${hashA} perl -e 'alarm 30; exec @ARGV' bash "${canary}/tools-${hashA}/fast.sh" --once "${three}" > "${T}/once.log" 2>&1
check canary-once-pins-to-itself 'grep -qx "verify ${three} set A bin ${canary}/tools-${hashA}" "${STUBLOG}" && [ "$(record "${three}" loom_tools)" = "${hashA}" ] && [ "$(find "${canary}" -maxdepth 1 -name "tools-*" | wc -l | tr -d " ")" = "${snapshots}" ]'
pkill -TERM -f "${T}/home/" 2> /dev/null
echo "failures: ${failures}"
exit $((failures > 0))
