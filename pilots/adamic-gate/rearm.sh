#!/bin/bash
# rearm.sh: keep both Codex pools serving. A serve turn ends at its deadline, and some end long before it (Oct 9:
# 13 of 40 instances "interrupted" 24 to 48 minutes into a 115-minute turn), so the pools shrink unless someone
# sends the next turn. Run every 10 minutes (LaunchAgent com.loom.rearm), it sends the serve prompt to every
# fleet member whose session has completed. A member whose last reply says Codex's approval review rejected the
# runner is left alone: sending again only spends a turn on the same refusal.
set -uo pipefail
ahra=/Users/kirkouimet/Projects/ahra
runner=${LOOM_RUNNER_SHA:-7f01c04925b5bcdf4c2359abcee90f0723b656867e696313db20391d08cdada7}
# A new runner reaches one side instance first (the staged-rollout law). ~/.loom/runner-staging holds the new
# runner's sha256, then the member it went to: the next side member whose turn ends takes it, keeps it on every
# later turn, and no other member does. After a green unit on that worker, the sha becomes LOOM_RUNNER_SHA's default.
staging=${HOME}/.loom/runner-staging
# The star's pool keeps the star fleet's first LOOM_STAR_INSTANCES members (loom-pool-codex-1 to -50); the rest serve
# side work (@system_adamic, Oct 9 04:16Z: the solver shows the star's wall is its floor on 47 instances, so the other
# 50 cost it nothing). Each moves at its next serve turn. Re-solve each run (solve.py) and raise this when splits make
# width matter again.
starInstances=${LOOM_STAR_INSTANCES:-50}
read -r staged stagedOn < "${staging}" 2> /dev/null || staged=""
cd "${ahra}" || exit 1
for pair in loom-pool:codex loom-side:codex-side; do
	fleet=${pair%%:*} pool=${pair#*:}
	prompt=${HOME}/.loom/rearm-${pool}.md
	"${HOME}/.loom/bin/loom-pregate" pool prompt --runner "${runner}" --until 115m "${pool}" > "${prompt}" || continue
	stagedPrompt=""
	if [ -n "${staged}" ] && [ "${pool}" = codex-side ]; then
		stagedPrompt=${HOME}/.loom/rearm-${pool}-staged.md
		"${HOME}/.loom/bin/loom-pregate" pool prompt --runner "${staged}" --until 115m "${pool}" > "${stagedPrompt}" || stagedPrompt=""
	fi
	for member in $(./node_modules/.bin/ahra ai fleet "${fleet}" --json 2> /dev/null | python3 -c "
import json, re, sys
for member in json.load(sys.stdin):
    if member.get('session', {}).get('status') == 'Completed':
        number = re.search(r'-(\d+)$', member.get('label') or '')
        print('%s:%s' % (member['id'], number.group(1) if number else 0))"); do
		id=${member%%:*} number=${member#*:}
		memberPrompt=${prompt}
		if [ "${pool}" = codex ] && [ "${number}" -gt "${starInstances}" ]; then
			memberPrompt=${HOME}/.loom/rearm-codex-side.md
			"${HOME}/.loom/bin/loom-pregate" pool prompt --runner "${runner}" --until 115m codex-side > "${memberPrompt}" || continue
		fi
		if ./node_modules/.bin/ahra ai summary "${id}" 2> /dev/null | grep -q "approval review rejected"; then
			echo "$(date -u +%H:%M:%S) ${fleet} ${id}: left alone, Codex's approval review rejected the runner"
			continue
		fi
		if [ -n "${stagedPrompt}" ] && { [ -z "${stagedOn:-}" ] || [ "${stagedOn}" = "${id}" ]; }; then
			./node_modules/.bin/ahra ai send "${id}" --message-file "${stagedPrompt}" > /dev/null 2>&1 || continue
			stagedOn=${id}
			echo "${staged} ${id}" > "${staging}"
			echo "$(date -u +%H:%M:%S) ${fleet} ${id}: next serve turn sent on the staged runner ${staged:0:12}"
			continue
		fi
		./node_modules/.bin/ahra ai send "${id}" --message-file "${memberPrompt}" > /dev/null 2>&1 && echo "$(date -u +%H:%M:%S) ${fleet} ${id}: next serve turn sent ($(basename "${memberPrompt}" .md | sed 's/^rearm-//'))"
	done
done
