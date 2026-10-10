#!/bin/bash
# pregate.sh: the pre-gate (#vvpm0a8, @system_adamic for Kirk, Oct 8: "all tests should fail fast and loud").
# Every candidate in the whole gate's request file runs on Loom's warm Codex pool before any box takes its whole
# gate: every unit at once, so the whole red list comes back in one run. A star candidate (a sha some
# cloud/land-train-* branch points at) runs the whole Go test set on the star's pool, about 14 minutes (@system_adamic,
# Oct 9: V1's reds were in stage1 packages the narrow pre-gate never ran); any other candidate runs the packages
# that turn stars red on the side pool.
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
#	ssh Chonchon 'cd loom-src && source ~/adamic-tools/env.sh && GOOS=darwin GOARCH=arm64 go build -o /tmp/loom-pregate-darwin ./cmd/loom && GOOS=darwin GOARCH=arm64 go build -o /tmp/loom-adamic-gate-darwin ./pilots/adamic-gate'
#	scp Chonchon:/tmp/loom-pregate-darwin ~/.loom/bin/loom-pregate && scp Chonchon:/tmp/loom-adamic-gate-darwin ~/.loom/bin/adamic-gate
# (/tmp/adamic-gate on a box is the gate's TMPDIR, a directory, hence the names.)
set -uo pipefail

state=${HOME}/.adamic-full-gate
requests=${ADAMIC_FULL_GATE_REQUESTS:-${state}/requests}
verdicts=${state}/pregate
work=${HOME}/.loom/pregate
gate=${HOME}/Projects/system/adamic-gate
# The star's pool: 72 instances from Oct 9 04:35Z (solve.py on the tree's test list: 72 reach main's 496 s floor), its
# whole-set runs planned two units a slot so the longest-first packing has small units to fill in with.
starSlots=${LOOM_STAR_POOL_SLOTS:-72}
packages=${LOOM_PREGATE_PACKAGES:-'/(stage3/fixtures|internal/fresh|internal/lower|internal/flow|internal/oracle)$'}
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

# star <sha>: whether a cloud/land-train-* branch points at it, read from origin (resolved there, never from a
# local FETCH_HEAD).
star() {
	git -C "${gate}" ls-remote origin 'refs/heads/cloud/land-train-*' 2> /dev/null | grep -q "^$1"
}

pregate() {
	local sha=$1 started=${SECONDS} reference inputs run verdict summary pool units only scope priority slots unit
	local again rerun planned broken
	local job=${work}/${sha}.job.json record=${work}/${sha}.record.jsonl report=${work}/${sha}.reds.txt
	echo "running" > "${verdicts}/${sha}"
	reference=$(reference) || { void "${sha}" "no green whole gate's record to plan from"; return; }
	inputs=$(cat "${HOME}/.loom/gate-inputs" 2> /dev/null)
	# LOOM_PREGATE_WHOLE=1 asks for the whole set on the star's pool for a candidate no train branch names yet (a
	# lane's next star, such as compiler's V2 on Oct 9).
	split=()
	# Its units' rank on the shared pool: the caller's (gate.sh passes main's 20 or the star's 30), else the star's 30
	# for a train tip and 0 for a side candidate, the rank the runs queued before priorities hold.
	priority=${LOOM_PRIORITY:-$(star "${sha}" && echo 30 || echo 0)}
	if [ "${LOOM_PREGATE_WHOLE:-}" = 1 ] || star "${sha}"; then
		pool=codex units=$((starSlots * 2)) only="" scope="the whole Go test set"
		# Child by child, sized by Loom's own 4-CPU times where it has them (~/.loom/loom-times.tsv, compare --times of
		# a recent whole set): cf04e18f's 851 s unit was tsprinter's TestMutants run whole, 304 s on a box.
		split=(--split-all)
		[ -s "${HOME}/.loom/loom-times.tsv" ] && split+=(--loom-times "${HOME}/.loom/loom-times.tsv")
	else
		pool=codex-side units=15 only=${packages} scope="the red-prone packages"
	fi
	# The tree's own test list at the sha (treetests.py). Under a budget (LOOM_PREGATE_BUDGET, seconds; main.sh sets 60
	# for main's gate, #w3y7pks) the plan is made from it, package first, as many units as fit the budget, each packed
	# unit killed at budget plus a half and that kill a red; unbudgeted it is kept beside the job only, since without
	# package affinity its thousands of small split tests scattered over 2,625 units (Oct 9, 04:40Z).
	git -C "${gate}" fetch -q origin "${sha}" 2> /dev/null
	python3 "${HOME}/.loom/bin/treetests.py" "${sha}" > "${work}/${sha}.tree-tests.txt" 2> /dev/null
	if [ -n "${LOOM_PREGATE_BUDGET:-}" ] && [ -s "${work}/${sha}.tree-tests.txt" ]; then
		split+=(--budget "${LOOM_PREGATE_BUDGET}" --unit-setup 10 --tree-tests "${work}/${sha}.tree-tests.txt")
	fi
	"${planner}" plan --target codex --remainder --gate-inputs "${inputs}" --reference "${reference}" --sha "${sha}" --units "${units}" --only "${only}" ${split[@]+"${split[@]}"} > "${job}" 2> "${work}/${sha}.plan.err" || { void "${sha}" "planning failed"; return; }
	printf 'running\npre-gate of %s (%s) on %s since %s\n' "${sha}" "${scope}" "${pool}" "$(date -u +%H:%M:%SZ)" > "${verdicts}/${sha}"
	slots=$([ "${pool}" = codex ] && echo "${starSlots}" || echo 15)
	"${loom}" run --uncached --slots none --pool "${pool}=${slots}" --priority "${priority}" --record "${record}" "${job}" > "${work}/${sha}.log" 2>&1
	# A unit that broke for Loom's own reasons (exit 2: a failed checkout, a full disk; or no results at all) proved
	# nothing about the change, so it is placed once more in a run of its own and read from there (Oct 9: main beb1be1b's
	# and ac1e362f's whole gates went void on 2 and 3 checkouts GitHub turned away). Broken twice, the run is void.
	again=() rerun=()
	rm -f "${work}/${sha}.again.json" "${work}/${sha}.again-record.jsonl"
	"${planner}" reds --job "${job}" --record "${record}" > "${work}/${sha}.first-reds.txt" 2>&1
	while read -r unit; do again+=("${unit}"); done < <(grep -E '^BROKEN [^ ]+: (exited 2, Loom|no results|killed in its opening)' "${work}/${sha}.first-reds.txt" | awk '{print $2}' | tr -d :)
	if [ ${#again[@]} -gt 0 ]; then
		python3 - "${job}" "${work}/${sha}.again.json" "${again[@]}" <<'PYTHON'
import json, sys
job, keep = json.load(open(sys.argv[1])), set(sys.argv[3:])
job["name"] += "-again"
job["units"] = [unit for unit in job["units"] if unit["id"] in keep]
# A product that passed in the first run stays passed: the units placed again need only the products placed with them.
for unit in job["units"]:
    unit["needs"] = [need for need in unit.get("needs") or [] if need in keep]
    if not unit["needs"]:
        unit.pop("needs", None)
json.dump(job, open(sys.argv[2], "w"))
PYTHON
		echo "again: ${again[*]}" >> "${work}/${sha}.log"
		"${loom}" run --uncached --slots none --pool "${pool}=$((${#again[@]} < slots ? ${#again[@]} : slots))" --priority "${priority}" --record "${work}/${sha}.again-record.jsonl" "${work}/${sha}.again.json" >> "${work}/${sha}.log" 2>&1
		rerun=(--rerun "${work}/${sha}.again.json:${work}/${sha}.again-record.jsonl")
	fi
	"${planner}" reds --job "${job}" --record "${record}" ${rerun[@]+"${rerun[@]}"} --tests "${work}/${sha}.tests.jsonl" > "${report}" 2>&1
	case $? in 0) verdict=green ;; 1) verdict=red ;; *) verdict=void ;; esac
	# Every run teaches the times table (#2en3b4t): its leaves and parents by their own seconds on 4 CPUs, then the
	# p90s the next plan packs by.
	# Each leaf killed at 90 s is filed P0 for its owner, or noted on its grain task, and Loom's own P0s close after
	# two runs under 60 s (#kxnza1j).
	python3 "${HOME}/.loom/bin/p0.py" "${report}" "${work}/${sha}.tests.jsonl" --sha "${sha}" --run "$(head -1 "${report}" | awk '{print $2}' | tr -d :)" >> "${work}/p0.log" 2>&1
	if [ -s "${work}/${sha}.tests.jsonl" ]; then
		python3 "${HOME}/.loom/bin/times.py" update "${work}/${sha}.tests.jsonl" --sha "${sha}" --run "$(head -1 "${report}" | awk '{print $2}' | tr -d :)" &&
			python3 "${HOME}/.loom/bin/times.py" tsv > "${HOME}/.loom/loom-times.tsv.partial" && mv "${HOME}/.loom/loom-times.tsv.partial" "${HOME}/.loom/loom-times.tsv"
		# The burn-down after every run that times leaves, by itself, never when someone remembers it (@system_adamic):
		# one line on #5g5151k's status feed, which the witness's grain curve reads, and the summary table as a comment
		# for the owners; the whole list kept beside the run.
		python3 "${HOME}/.loom/bin/burndown.py" "${work}/${sha}.tests.jsonl" --run "${sha:0:12} ${scope}, run $(head -1 "${report}" | awk '{print $2}' | tr -d :)" > "${work}/${sha}.burndown.md"
		# The units whose results came back, of those planned, from the reds' first line ("... in 660 units, ... 51 units
		# broken ..."): a void run's count is partial and its line says so.
		planned=$(head -1 "${report}" | sed -nE 's/.* in ([0-9]+) units,.*/\1/p') broken=$(head -1 "${report}" | sed -nE 's/.* ([0-9]+) units broken.*/\1/p')
		python3 "${HOME}/.loom/bin/burndown.py" "${work}/${sha}.tests.jsonl" --line --run "${sha:0:12}, ${verdict} run" \
			--units-planned "${planned:-0}" --units-run "$(( ${planned:-0} - ${broken:-0} ))" > "${work}/${sha}.burndown-status.txt"
		sed '/^## /,$d' "${work}/${sha}.burndown.md" > "${work}/${sha}.burndown-line.md"
		(
			cd /Users/kirkouimet/Projects/ahra &&
				./node_modules/.bin/ahra tasks status 5g5151k "$(cat "${work}/${sha}.burndown-status.txt")" --auto >> "${work}/burndown-post.log" 2>&1
			./node_modules/.bin/ahra tasks comment 5g5151k --role Agent --text-file "${work}/${sha}.burndown-line.md" > /dev/null 2>&1 || true
		)
		gzip -9f "${work}/${sha}.tests.jsonl"
	fi
	run=$(head -1 "${report}" | awk '{print $2}' | tr -d :)
	summary="pre-gate of ${sha} (${scope}, ${pool}) in $((SECONDS - started)) s,$(head -1 "${report}" | cut -d, -f2-) (run ${run}, list ${report})"
	echo "${verdict} ${summary}" > "${work}/${sha}.verdict"
	case ${verdict} in
		green) printf 'green\n%s\n' "${summary}" > "${verdicts}/${sha}" ;;
		red) printf 'red: %s failed\n%s\n' "$(grep -c '^FAIL ' "${report}")" "${summary}" > "${verdicts}/${sha}" ;;
		*) rm -f "${verdicts}/${sha}" ;;
	esac
	echo "$(date -u +%H:%M:%S) $(cat "${work}/${sha}.verdict")"
	if [ "${verdict}" != green ]; then
		{
			echo "Pre-gate of ${sha} is ${verdict} in $((SECONDS - started)) s on Loom's ${pool} pool:$(head -1 "${report}" | cut -d, -f2-). It ran ${scope}, every unit at once; the whole list, failed leaves only$([ "${verdict}" = void ] && echo ", and the candidate stays eligible for the boxes since the fault is Loom's"):"
			echo
			head -c 12000 "${report}" | tail -n +2
			[ "$(wc -c < "${report}")" -gt 12000 ] && echo "... cut at 12 KB; the full list is ${report}"
		} > "${work}/${sha}.message"
		# LOOM_PREGATE_ALSO names one more recipient, the candidate's owner (compiler for its views stack).
		for recipient in system_adamic_integration ${LOOM_PREGATE_ALSO:-}; do
			(cd /Users/kirkouimet/Projects/ahra && ./node_modules/.bin/ahra os send "${recipient}" --body-file "${work}/${sha}.message" > /dev/null 2>&1 || true)
		done
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
	# A star candidate gets the whole gate on the pool (gate.sh: Go set, phases, census, a landing record the loops and
	# push-main read since the promotion); any other candidate, the red-prone packages' pre-gate.
	if [ -n "${next}" ] && star "${next}"; then
		LOOM_PRIORITY=30 "${HOME}/.loom/bin/gate.sh" "${next}"
	elif [ -n "${next}" ]; then
		pregate "${next}"
	fi
	sleep 10
done
