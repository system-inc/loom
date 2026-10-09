#!/bin/bash
# diskretire.sh [<diskretire.py>]: diskretire.py against planted job events in a temporary jobs directory, run --dry so
# nothing reaches the wire (#0k03wz1), one scenario per case, PASS or FAIL each. A machine that broke two units on a full
# disk in the last ten minutes is named for retirement; one break, breaks older than ten minutes, a machine already
# retired and a unit broken for another reason are not. The mutant (one break enough) must fail:
#
#	pilots/adamic-gate/fastretry/diskretire.sh
#	sed 's/^threshold = 2$/threshold = 1/' diskretire.py > m.py && fastretry/diskretire.sh m.py   # fails 1
set -u
here=$(cd "$(dirname "$0")" && pwd) failures=0
script=${1:-${here}/../diskretire.py}
T=$(mktemp -d)
export LOOM_DISKRETIRE_JOBS=${T}/jobs LOOM_DISKRETIRE_STATE=${T}/state.json
check() { if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; sed "s/^/    /" "${T}/said"; failures=$((failures + 1)); fi; }
# unit <job> <unit> <machine> <minutes ago> <text>: a unit started on the machine, then printed the text.
unit() {
	local work=${T}/jobs/fast/$1.work stamp
	mkdir -p "${work}"
	stamp=$(python3 -c 'import sys, time; print(time.strftime("%Y-%m-%dT%H:%M:%S.000Z", time.gmtime(time.time() - 60 * int(sys.argv[1]))))' "$4")
	printf '{"position":1,"event":{"run":"r-%s","unit":"%s","type":"started","machine":"%s","time":"%s"}}\n' "$1" "$2" "$3" "${stamp}" >> "${work}/events.jsonl"
	printf '{"position":2,"event":{"run":"r-%s","unit":"%s","type":"output","stream":"stdout","text":"%s","time":"%s"}}\n' "$1" "$2" "$5" "${stamp}" >> "${work}/events.jsonl"
}
full='loom-pilot: only 398 MB free on the instance after trimming its caches: Loom'"'"'s fault'
unit a product-01 hole1 1 "${full}"
unit a product-02 hole1 2 "${full}"
unit b product-07 once1 1 "${full}"
unit c product-03 old1 25 "${full}"
unit c product-04 old1 30 "${full}"
unit d tests-01 red1 1 'FAIL github.com/system-inc/adamic/internal/ir TestX'
unit d tests-02 red1 1 'FAIL github.com/system-inc/adamic/internal/ir TestY'
unit e product-05 kernel1 1 'write /tmp/x: no space left on device'
unit e product-06 kernel1 1 'write /tmp/y: No space left on device'
unit f product-08 done1 1 "${full}"
unit f product-09 done1 1 "${full}"
echo '{"done1": {"retired": 1}}' > "${LOOM_DISKRETIRE_STATE}"
# Old events live in a file the window still reads (its mtime is now), so only their own stamps keep them out.
python3 "${script}" --dry > "${T}/said" 2>&1
check two-full-disk-breaks-retire 'grep -q "would retire hole1 " "${T}/said"'
check the-kernels-wording-counts 'grep -q "would retire kernel1 " "${T}/said"'
check one-break-is-weather '! grep -q "once1" "${T}/said"'
check breaks-older-than-ten-minutes-dont-count '! grep -q "old1" "${T}/said"'
check a-red-test-is-no-full-disk '! grep -q "red1" "${T}/said"'
check a-retired-machine-isnt-retired-twice '! grep -q "done1" "${T}/said"'
check dry-writes-no-state 'grep -q done1 "${LOOM_DISKRETIRE_STATE}" && ! grep -q hole1 "${LOOM_DISKRETIRE_STATE}"'

for job in a b c d e f; do rm -f "${T}/jobs/fast/${job}.work/events.jsonl" && rmdir "${T}/jobs/fast/${job}.work"; done
rm -f "${T}/said" "${LOOM_DISKRETIRE_STATE}" && rmdir "${T}/jobs/fast" "${T}/jobs" "${T}"
echo "failures: ${failures}"
exit $((failures > 0))
