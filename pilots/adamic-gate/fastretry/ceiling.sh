#!/bin/bash
# ceiling.sh [<fast.sh>]: fast.sh's within against a stubbed serve (#h16aj9a): the ceiling counts from the job's first
# placed unit (<sha>.placed at 1 or more), never from serve; a job never placed is never marked, and one that ends while
# its watchdog still waits leaves no watchdog behind. The mutant watchdog that sleeps from serve (the old code) must fail:
#
#	pilots/adamic-gate/fastretry/ceiling.sh
#	sed '/until \[.*\.placed/,/^		done$/d' fast.sh > m.sh && fastretry/ceiling.sh m.sh   # fails 3
set -u
here=$(cd "$(dirname "$0")" && pwd) failures=0
fast=${1:-${here}/../fast.sh}
funcs=$(mktemp)
awk '/^within\(\) \{/,/^}/' "${fast}" > "${funcs}"
grep -q 'sleep "${ceiling}"' "${funcs}" || { echo "FAIL: no watchdog in ${fast}'s within"; exit 1; }
check() { if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; failures=$((failures + 1)); fi; }
# serve stands in for the job: it runs until the test writes $T/finish (at most 15 s), then drops .running as finish does.
serve() {
	local waited=0
	while [ ! -e "${T}/finish" ] && [ "${waited}" -lt 150 ]; do sleep 0.1; waited=$((waited + 1)); done
	rm -f "${jobs}/${sha}.running"
}
scenario() {
	T=$(mktemp -d) sha=1111111111111111111111111111111111111111
	jobs=$T/jobs bin=$T/bin work=$T/jobs/$sha.work ceiling=2
	mkdir -p "${work}" "${bin}" && : > "${bin}/placed.py" && touch "${jobs}/${sha}.running"
}
source "${funcs}"

# The first unit is placed 3 s after serve: not marked a second later, marked once the 2 s ceiling has run past it.
scenario
within "${sha}" > /dev/null 2>&1 &
job=$!
sleep 3; echo "1 2" > "${jobs}/${sha}.placed"
sleep 1; check placed-late-not-yet '[ ! -e "${work}/ceiling" ]'
sleep 3; check placed-late-marked '[ -e "${work}/ceiling" ]'
touch "${T}/finish"; wait "${job}"

# Never placed: never marked, however long it waits; and when it ends, its watchdog is gone.
scenario
within "${sha}" > /dev/null 2>&1 &
job=$!
sleep 4; check never-placed-unmarked '[ ! -e "${work}/ceiling" ]'
children=$(pgrep -P "${job}")
touch "${T}/finish"; wait "${job}"; sleep 1.5
check never-placed-no-stray-watchdog '(for child in ${children}; do ! kill -0 "${child}" 2> /dev/null || exit 1; done) && [ ! -e "${work}/ceiling" ]'
echo "failures: ${failures}"
exit $((failures > 0))
