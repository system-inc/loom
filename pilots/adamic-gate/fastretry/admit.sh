#!/bin/bash
# admit.sh [<fast.sh>]: fast.sh's serving loop, one pass, against planted jobs (admission control, @system_adamic Oct 9
# 17:20Z). With ~/.loom/admit at K, at most K jobs run whatever their tier, highest tier admitted first; without the
# file, the old rule: the cap holds below tier 30 only. The mutant that lets tier 30 and up past K must fail:
#
#	pilots/adamic-gate/fastretry/admit.sh
#	sed 's/\[ "${running}" -ge "${admit}" \] \&\& break/[ "${priority}" -lt 30 ] \&\& [ "${running}" -ge "${admit}" ] \&\& break/' fast.sh > m.sh && fastretry/admit.sh m.sh   # fails 3
set -u
here=$(cd "$(dirname "$0")" && pwd) failures=0
fast=${1:-${here}/../fast.sh}
loop=$(mktemp)
# The loop from `while true; do` to the script's end, one pass: its sleep ends it.
awk '/^while true; do$/ {on = 1} on' "${fast}" | sed 's/^	sleep 10$/	break/' > "${loop}"
grep -q "admit" "${loop}" || { echo "FAIL: no admission in ${fast}'s loop"; exit 1; }
check() { if eval "$2"; then echo "PASS $1"; else echo "FAIL $1: started $(cat $T/started 2>/dev/null | tr '\n' ' ')"; failures=$((failures + 1)); fi; }
scenario() { # scenario <K or ""> <running count> <tier:count>...
	T=$(mktemp -d) && HOME=$T/home jobs=$T/jobs concurrent=8 && mkdir -p $HOME/.loom $jobs
	[ -n "$1" ] && echo "$1" > $HOME/.loom/admit
	local n=0 i spec
	for ((i = 0; i < $2; i++)); do n=$((n + 1)); touch $jobs/$(printf '%040d' $n).json $jobs/$(printf '%040d' $n).running; done
	for spec in "${@:3}"; do
		for ((i = 0; i < ${spec#*:}; i++)); do n=$((n + 1)); echo "{\"priority\": ${spec%%:*}}" > $jobs/$(printf '%040d' $n).json; done
	done
}
launch() { echo "$1" >> $T/started; }
cancel() { :; }
pass() { source "${loop}" > $T/out.log 2>&1; wait; }
started() { [ -s $T/started ] && wc -l < $T/started | tr -d ' ' || echo 0; }

scenario 4 2 40:3 30:2 0:2; pass; check admit-fills-to-k '[ "$(started)" = 2 ]'
check admit-highest-tier-first '[ "$(python3 -c "import json,sys; print(sorted({json.load(open(\"$T/jobs/\"+s.strip()+\".json\"))[\"priority\"] for s in open(\"$T/started\")}))")" = "[40]" ]'
scenario 4 4 40:3; pass; check admit-star-waits-at-k '[ "$(started)" = 0 ] && ! ls $T/jobs/*.running | grep -q 0000000000000000000000000000000000000005'
scenario 4 0 0:6; pass; check admit-tier-0-to-k '[ "$(started)" = 4 ] && grep -q "admitted .* 3 running of 4" $T/out.log'
scenario "" 9 40:2 0:2; pass; check no-file-star-bypasses-cap '[ "$(started)" = 2 ]'
scenario junk 0 0:3; pass; check bad-file-is-no-file '[ "$(started)" = 3 ]'
echo "failures: ${failures}"
exit $((failures > 0))
