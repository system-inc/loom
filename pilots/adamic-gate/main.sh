#!/bin/bash
# main.sh: Loom's whole-suite run of every new main, step 79's check (@system_adamic for Kirk, Oct 9 03:06Z). Splits
# land on main with no gate in front of them, so this run is where each must show every leaf under 60 s: the whole Go
# set on the star's pool, planned by children and by Loom's own times (pregate.sh --once), its red list to
# integration, its burn-down line posted on #5g5151k by the pre-gate, and the whole burn-down kept beside the run. One main at a time, the
# newest first; a main already run is never run again.
#
#	pilots/adamic-gate/main.sh            # the server, as LaunchAgent com.loom.main (a copy in ~/.loom/bin)
set -uo pipefail
state=${HOME}/.loom/main gate=${HOME}/Projects/system/adamic-gate
planner=${HOME}/.loom/bin/adamic-gate burndown=${HOME}/.loom/bin/burndown.py
mkdir -p "${state}"
while true; do
	tip=$(git -C "${gate}" ls-remote origin refs/heads/main 2> /dev/null | cut -f1)
	if [[ ${tip} =~ ^[0-9a-f]{40}$ ]] && [ ! -f "${state}/${tip}.done" ]; then
		echo "$(date -u +%H:%M:%S) main ${tip:0:12}: whole Go set on the star's pool"
		LOOM_PREGATE_WHOLE=1 "${HOME}/.loom/bin/pregate.sh" --once "${tip}" > "${state}/${tip}.out" 2>&1
		work=${HOME}/.loom/pregate
		if [ -s "${work}/${tip}.record.jsonl" ]; then
			gzip -dc "${work}/${tip}.tests.jsonl.gz" > "${state}/${tip}.tests.jsonl" 2> /dev/null ||
				"${planner}" reds --job "${work}/${tip}.job.json" --record "${work}/${tip}.record.jsonl" --tests "${state}/${tip}.tests.jsonl" > /dev/null 2>&1
			python3 "${burndown}" "${state}/${tip}.tests.jsonl" --run "main ${tip:0:12}, $(head -1 "${work}/${tip}.verdict" | cut -d' ' -f1)" > "${state}/${tip}.burndown.md"
			gzip -9f "${state}/${tip}.tests.jsonl"
			echo "$(date -u +%H:%M:%S) main ${tip:0:12}: $(head -1 "${work}/${tip}.verdict"); $(sed -n 3p "${state}/${tip}.burndown.md")"
		fi
		touch "${state}/${tip}.done"
	fi
	sleep 60
done
