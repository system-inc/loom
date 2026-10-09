#!/bin/bash
# rearm.sh: keep both Codex pools serving. A serve turn ends at its deadline, and some end long before it (Oct 9:
# 13 of 40 instances "interrupted" 24 to 48 minutes into a 115-minute turn), so the pools shrink unless someone
# sends the next turn. Run every 10 minutes (LaunchAgent com.loom.rearm), it sends the serve prompt to every
# fleet member whose session has completed. A member whose last reply says Codex's approval review rejected the
# runner is left alone: sending again only spends a turn on the same refusal.
set -uo pipefail
ahra=/Users/kirkouimet/Projects/ahra
runner=${LOOM_RUNNER_SHA:-c1825196c9c8d1da3e89b74ce39e050fe9f0cc6ed5fe242703af8662dea2ae52}
cd "${ahra}" || exit 1
for pair in loom-pool:codex loom-side:codex-side; do
	fleet=${pair%%:*} pool=${pair#*:}
	prompt=${HOME}/.loom/rearm-${pool}.md
	"${HOME}/.loom/bin/loom-pregate" pool prompt --runner "${runner}" --until 115m "${pool}" > "${prompt}" || continue
	for id in $(./node_modules/.bin/ahra ai fleet "${fleet}" --json 2> /dev/null | python3 -c "
import json, sys
for member in json.load(sys.stdin):
    if member.get('session', {}).get('status') == 'Completed':
        print(member['id'])"); do
		if ./node_modules/.bin/ahra ai summary "${id}" 2> /dev/null | grep -q "approval review rejected"; then
			echo "$(date -u +%H:%M:%S) ${fleet} ${id}: left alone, Codex's approval review rejected the runner"
			continue
		fi
		./node_modules/.bin/ahra ai send "${id}" --message-file "${prompt}" > /dev/null 2>&1 && echo "$(date -u +%H:%M:%S) ${fleet} ${id}: next serve turn sent"
	done
done
