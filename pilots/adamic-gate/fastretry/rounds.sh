#!/bin/bash
# rounds.sh [<verify.sh>]: verify.sh's retry rounds against stubs (#gk5fh85, #m5x7n3n): a unit Loom broke is placed
# again up to twice, and no round starts past the job's ceiling or after its cancel. The mutant without that check
# must fail:
#
#	pilots/adamic-gate/fastretry/rounds.sh
#	sed '/No round starts past the job/,/^	fi$/d' verify.sh > m.sh && fastretry/rounds.sh m.sh   # fails 3
set -u
here=$(cd "$(dirname "$0")" && pwd) failures=0
verify=${1:-${here}/../verify.sh}
loop=$(mktemp)
awk '/^reruns=\(\)$/{on=1} on{print} on && /^done$/{exit}' "${verify}" > "${loop}"
grep -q 'for round in 1 2' "${loop}" || { echo "FAIL: no retry loop in ${verify}"; exit 1; }
check() { if eval "$2"; then echo "PASS $1"; else echo "FAIL $1: placements $(cat $T/calls 2>/dev/null || echo 0)"; failures=$((failures + 1)); fi; }
scenario() {
	T=$(mktemp -d) work=$T/jobs/1111.work
	mkdir -p $work
	echo '{"name":"stub","units":[{"id":"tests-01","argv":["true"]}]}' > $work/tests-only.json
	# The planner always says tests-01 broke on Loom's side; the coordinator counts its placements and, with
	# STUB_CEILING_AT=<n>, marks the job past its ceiling during its nth.
	planner=$T/planner loom=$T/loom pool=codex
	printf '#!/bin/bash\necho "BROKEN tests-01: exited 2, Loom'"'"'s fault (last output \\"\\")"\n' > $planner
	printf '#!/bin/bash\nn=$(( $(cat %s/calls 2> /dev/null || echo 0) + 1 )); echo $n > %s/calls\n[ "$n" = "${STUB_CEILING_AT:-0}" ] && touch %s/ceiling\nexit 0\n' $T $T $work > $loom
	chmod +x $planner $loom
}
run() { (source "${loop}") > /dev/null 2>&1; }

scenario; run; check two-rounds '[ "$(cat $T/calls)" = 2 ]'
scenario; touch $work/ceiling; run; check none-past-the-ceiling '[ ! -e $T/calls ]'
scenario; touch $T/jobs/1111.cancelled; run; check none-after-a-cancel '[ ! -e $T/calls ]'
scenario; STUB_CEILING_AT=1 run; check the-ceiling-during-round-one '[ "$(cat $T/calls)" = 1 ] && grep -q "no retry round 2" $work/run.log'
echo "failures: ${failures}"
exit $((failures > 0))
