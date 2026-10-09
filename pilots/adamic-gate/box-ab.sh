#!/bin/bash
# box-ab.sh: does a box's Loom workers slow its own box gate? (#x2bmxpk; @system_adamic, Oct 9 08:19Z: run it the
# first time a box drops under load 4, interleaved off, on, off, on, best of each, so drift can't pass as the effect.)
#
#	ssh <box> 'bash -s' < box-ab.sh       # waits up to 2 hours for a quiet box, then measures; output on stdout
#
# It holds a free box-gate slot the gate's own way (flock on ~/fast-gate/lock[-N]) and runs in that slot's tree, so the
# gate never shares the slot with it. The tests are ordinary ones (no products) of 5 to 40 s on the box's own times
# table, in packages a slot's tree keeps built, that exist in that tree: up to eight. A pass is one go test -count=1
# -json of them all at the box gate's priority on every core; its number is the sum of the tests' own elapsed
# seconds. "On" means the box's workers are serving the pool (box.sh, 8 workers on cores 32-63); "off", none running.
# The ratio is best-on over best-off; under 1.10 the workers may return, and the box gate is the judge either way.
set -uo pipefail
units=${HOME}/loom-units
pool=https://loom-wire.kirk-ouimet.workers.dev/pools/codex
log() { echo "$(date -u +%H:%M:%S) box-ab: $*"; }
workersOff() {
	pkill -TERM -f "[l]oom-runner-.* serve" 2> /dev/null
	pkill -TERM -f "[l]oom-units/before.sh" 2> /dev/null
	sleep 5
}
serving() { pgrep -f "[l]oom-runner-.* serve" | wc -l; }

# Workers readied beforehand (box.sh with LOOM_BOX_WARM_ONLY=1) finish first, so an "on" pass never waits on a clone.
for ((minute = 0; minute < 30; minute++)); do
	pgrep -f "[l]oom-units/before.sh" > /dev/null || break
	sleep 60
done
# A quiet box: load under 4 (@system_adamic's line) and slot 3, the one this holds, free, checked each minute for two
# hours. Another slot may be running a gate quietly; the load says how much it costs, and every pass logs it.
for ((minute = 0; ; minute++)); do
	load=$(cut -d' ' -f1 /proc/loadavg)
	awk -v current="${load}" 'BEGIN { exit !(current < 4) }' && flock -n "${HOME}/fast-gate/lock-3" true && break
	[ "${minute}" -ge 120 ] && { log "no quiet window in two hours (load ${load})"; exit 1; }
	sleep 60
done
log "quiet: load ${load}, slot 3 free"

# The last slot and its tree, held for the whole measure.
lock=${HOME}/fast-gate/lock-3 tree=${HOME}/fast-gate/tree-3
exec 9> "${lock}"
flock -n 9 || { log "slot 3 was taken just now"; exit 1; }
source "${HOME}/adamic-tools/env.sh"
cd "${tree}" || exit 1
mapfile -t picked < <(awk -F'\t' '$3 >= 5 && $3 <= 40 && $2 !~ /^TestProduct_/ && $1 ~ /internal\/(lower|regexp|flow)$/ {print $1 "\t" $2}' "${HOME}/fast-gate/test-seconds.tsv" |
	while IFS=$'\t' read -r package test; do
		directory=${package#github.com/system-inc/adamic/}
		grep -lq "^func ${test}(" "${directory}"/*_test.go 2> /dev/null && printf '%s\t%s\n' "${package}" "${test}"
	done | head -8)
[ ${#picked[@]} -ge 4 ] || { log "only ${#picked[@]} tests found in the slot's tree"; exit 1; }
packages=$(printf '%s\n' "${picked[@]}" | cut -f1 | sort -u | tr '\n' ' ')
pattern="^($(printf '%s\n' "${picked[@]}" | cut -f2 | paste -sd'|' -))\$"
log "tree $(git rev-parse --short HEAD), ${#picked[@]} tests: ${pattern}"

pass() {
	local name=$1 started=${SECONDS}
	go test -count=1 -json -run "${pattern}" ${packages} > "/tmp/box-ab-${name}.jsonl" 2> /dev/null
	python3 - "/tmp/box-ab-${name}.jsonl" "${name}" "$((SECONDS - started))" "$(cut -d' ' -f1 /proc/loadavg)" "$(serving)" <<'PYTHON'
import json, sys
path, name, wall, load, serving = sys.argv[1:]
seconds = {}
for line in open(path):
    try:
        event = json.loads(line)
    except ValueError:
        continue
    if event.get("Action") in ("pass", "fail") and event.get("Test") and "/" not in event["Test"]:
        seconds[event["Test"]] = event.get("Elapsed", 0)
print("%s\t%.2f\t%d tests\twall %s s\tload %s\tworkers %s" % (name, sum(seconds.values()), len(seconds), wall, load, serving))
PYTHON
}

workersOn() {
	bash "${HOME}/loom-units/box.sh" 8 32 115m "${pool}" > /dev/null 2>&1
	# On means serving and busy: at least six workers past their warm-up, each having taken a unit since it started.
	for ((wait = 0; wait < 90; wait++)); do
		[ "$(serving)" -ge 6 ] && break
		sleep 10
	done
	sleep 60
}

workersOff
pass warm > /dev/null
results=()
for order in off on off on; do
	if [ "${order}" = on ]; then workersOn; else workersOff; fi
	results+=("$(pass "${order}")")
	log "${results[-1]}"
done
workersOff
printf '%s\n' "${results[@]}" | python3 -c '
import sys
best = {}
for line in sys.stdin:
    name, seconds = line.split("\t")[:2]
    best[name] = min(best.get(name, float("inf")), float(seconds))
ratio = best["on"] / best["off"]
print("best off %.2f s, best on %.2f s, ratio %.3f: %s" % (best["off"], best["on"], ratio, "under 1.10, the workers may return" if ratio < 1.10 else "over 1.10, the workers stay off"))'
