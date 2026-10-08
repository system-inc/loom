#!/bin/bash
# pregate.sh: the pre-gate (#vvpm0a8, @system_adamic for Kirk, Oct 8: "all tests should fail fast and loud").
# Every train candidate in the whole gate's request file runs the packages that turn stars red on Loom's warm Codex
# pool before any box takes its whole gate: every unit at once, so the whole red list comes back in one run.
#
#	pilots/adamic-gate/pregate.sh [--once <sha>]
#
# The verdict is ~/.adamic-full-gate/pregate/<sha>, read by both whole-gate loops (developer tools' 8e5dfb79, by the
# first word of its first line: green lets a box take the candidate, anything else holds it). It says "running" the
# moment the candidate appears, then "green" or "red: <n> failed", the summary on the next line. A void run (a unit
# broke or never reported: Loom's fault, never the change's) removes the file, so the boxes take the candidate as if
# there were no pre-gate; Loom's own record stays in ~/.loom/pregate/<sha>.verdict. Integration gets every red and
# void list the moment the run ends.
#
# Inputs it keeps current: the reference (the newest green whole gate's test.jsonl.gz, which sizes the units and
# names the tests; tests it doesn't know still run in a remainder unit per package) and the gate inputs' manifest
# in ~/.loom/gate-inputs (the hash units fetch from the public store).
#
# Nothing builds on Kirk's Mac (@system_adamic, Oct 8 23:43Z), so the binaries are made on a box, darwin/arm64:
#	rsync -a --exclude .git --exclude wire/node_modules ~/Projects/system/loom/ chonchon:loom-src/
#	ssh chonchon 'cd loom-src && source ~/adamic-tools/env.sh && GOOS=darwin GOARCH=arm64 go build -o /tmp/loom-pregate-darwin ./cmd/loom && GOOS=darwin GOARCH=arm64 go build -o /tmp/loom-adamic-gate-darwin ./pilots/adamic-gate'
#	scp chonchon:/tmp/loom-pregate-darwin ~/.loom/bin/loom-pregate && scp chonchon:/tmp/loom-adamic-gate-darwin ~/.loom/bin/adamic-gate
# (/tmp/adamic-gate on a box is the gate's TMPDIR, a directory, hence the names.)
set -uo pipefail

state=${HOME}/.adamic-full-gate
requests=${ADAMIC_FULL_GATE_REQUESTS:-${state}/requests}
verdicts=${state}/pregate
work=${HOME}/.loom/pregate
gate=${HOME}/Projects/system/adamic-gate
packages=${LOOM_PREGATE_PACKAGES:-'/(stage3/fixtures|internal/fresh|internal/lower|internal/flow|internal/oracle)$'}
units=${LOOM_PREGATE_UNITS:-25}
mkdir -p "${verdicts}" "${work}"
loom=${HOME}/.loom/bin/loom-pregate planner=${HOME}/.loom/bin/adamic-gate
[ -x "${loom}" ] && [ -x "${planner}" ] || { echo "pregate: build ${loom} and ${planner} on a box first (see the header)"; exit 1; }

# The newest green whole gate's per-test record, cached by its sha.
reference() {
	local green ref
	green=$(cat "${state}/last-green" 2> /dev/null) || return 1
	if [ ! -s "${work}/reference-${green}.jsonl.gz" ]; then
		ref=$(git -C "${gate}" ls-remote origin "refs/heads/gate-logs/${green:0:12}/*" | awk '$2 ~ /\/full-main$/ {print $2}' | sort | tail -1)
		[ -n "${ref}" ] && git -C "${gate}" fetch -q origin "${ref}" && git -C "${gate}" show FETCH_HEAD:test.jsonl.gz > "${work}/reference-${green}.jsonl.gz.partial" || return 1
		mv "${work}/reference-${green}.jsonl.gz.partial" "${work}/reference-${green}.jsonl.gz"
	fi
	echo "${work}/reference-${green}.jsonl.gz"
}

# Loom's fault, never the change's: the boxes take the candidate as if there were no pre-gate.
void() {
	echo "void $2" > "${work}/$1.verdict"
	rm -f "${verdicts}/$1"
	echo "$(date -u +%H:%M:%S) void: pre-gate of $1: $2"
}

pregate() {
	local sha=$1 started=${SECONDS} reference inputs run verdict summary
	local job=${work}/${sha}.job.json record=${work}/${sha}.record.jsonl report=${work}/${sha}.reds.txt
	echo "running" > "${verdicts}/${sha}"
	reference=$(reference) || { void "${sha}" "no green whole gate's record to plan from"; return; }
	inputs=$(cat "${HOME}/.loom/gate-inputs" 2> /dev/null)
	"${planner}" plan --target codex --remainder --gate-inputs "${inputs}" --reference "${reference}" --sha "${sha}" --units "${units}" --only "${packages}" > "${job}" 2> /dev/null || { void "${sha}" "planning failed"; return; }
	printf 'running\npre-gate of %s on the Codex pool since %s\n' "${sha}" "$(date -u +%H:%M:%SZ)" > "${verdicts}/${sha}"
	"${loom}" run --uncached --slots none --pool codex=25 --record "${record}" "${job}" > "${work}/${sha}.log" 2>&1
	"${planner}" reds --job "${job}" --record "${record}" > "${report}" 2>&1
	case $? in 0) verdict=green ;; 1) verdict=red ;; *) verdict=void ;; esac
	run=$(head -1 "${report}" | awk '{print $2}' | tr -d :)
	summary="pre-gate of ${sha} in $((SECONDS - started)) s,$(head -1 "${report}" | cut -d, -f2-) (run ${run}, list ${report})"
	echo "${verdict} ${summary}" > "${work}/${sha}.verdict"
	case ${verdict} in
		green) printf 'green\n%s\n' "${summary}" > "${verdicts}/${sha}" ;;
		red) printf 'red: %s failed\n%s\n' "$(grep -c '^FAIL ' "${report}")" "${summary}" > "${verdicts}/${sha}" ;;
		*) rm -f "${verdicts}/${sha}" ;;
	esac
	echo "$(date -u +%H:%M:%S) $(cat "${work}/${sha}.verdict")"
	if [ "${verdict}" != green ]; then
		{
			echo "Pre-gate of ${sha} is ${verdict} in $((SECONDS - started)) s on the Codex pool:$(head -1 "${report}" | cut -d, -f2-). The packages that turn stars red, every unit at once; the whole list, failed leaves only$([ "${verdict}" = void ] && echo ", and the candidate stays eligible for the boxes since the fault is Loom's"):"
			echo
			head -c 12000 "${report}" | tail -n +2
			[ "$(wc -c < "${report}")" -gt 12000 ] && echo "... cut at 12 KB; the full list is ${report}"
		} > "${work}/${sha}.message"
		(cd /Users/kirkouimet/Projects/ahra && ./node_modules/.bin/ahra os send system_adamic_integration --body-file "${work}/${sha}.message" > /dev/null 2>&1 || true)
	fi
}

if [ "${1:-}" = --once ]; then
	pregate "$2"
	exit
fi
# Every new request is held "running" within 5 s, even while another pre-gate runs, before a whole-gate loop can
# claim it (they look every 60 s). A void decided between the look and the write is undone at once.
undecided() {
	while read -r sha; do
		[ -n "${sha}" ] && [ ! -f "${work}/${sha}.verdict" ] && echo "${sha}"
	done < <(cat "${requests}" 2> /dev/null)
}
hold() {
	while true; do
		for sha in $(undecided); do
			[ -f "${verdicts}/${sha}" ] || echo "running" > "${verdicts}/${sha}"
			grep -q '^void' "${work}/${sha}.verdict" 2> /dev/null && rm -f "${verdicts}/${sha}"
		done
		sleep 5
	done
}
hold &
trap 'kill $! 2> /dev/null' EXIT
# Then the first request Loom hasn't decided runs, bottom first as integration writes them.
while true; do
	next=$(undecided | head -1)
	[ -n "${next}" ] && pregate "${next}"
	sleep 10
done
