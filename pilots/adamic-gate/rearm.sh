#!/bin/bash
# rearm.sh: keep both Codex pools serving. A serve turn ends at its deadline, and some end long before it (Oct 9:
# 13 of 40 instances "interrupted" 24 to 48 minutes into a 115-minute turn), so the pools shrink unless someone
# sends the next turn. Run every 2 minutes (LaunchAgent com.loom.rearm, StartInterval 120; at 10, 10 of the star's 72 sat
# finished between passes, Oct 9), it sends the serve prompt to every
# fleet member whose session has completed. A member whose last reply says Codex's approval review rejected the
# runner is left alone: sending again only spends a turn on the same refusal.
set -uo pipefail
ahra=/Users/kirkouimet/Projects/ahra
runner=${LOOM_RUNNER_SHA:-7f01c04925b5bcdf4c2359abcee90f0723b656867e696313db20391d08cdada7}
# A new runner reaches one side instance first (the staged-rollout law). ~/.loom/runner-staging holds the new
# runner's sha256, then the member it went to: the next side member whose turn ends takes it, keeps it on every
# later turn, and no other member does. After a green unit on that worker, the sha becomes LOOM_RUNNER_SHA's default.
staging=${HOME}/.loom/runner-staging
# The star's pool keeps the star fleet's first LOOM_STAR_INSTANCES members (loom-pool-codex-1 to -72); the rest serve
# side work (@system_adamic, Oct 9 04:16Z: the star takes what reaches its floor, the rest go to side work). Re-solved
# on the tree's own test list (04:35Z): main 11171d0c's floor is 496 s and takes 72 instances; at 50 it was 696 s.
# Each member moves at its next serve turn. Re-solve each run (solve.py) and move this with the answer.
starInstances=${LOOM_STAR_INSTANCES:-72}
read -r staged stagedOn < "${staging}" 2> /dev/null || staged=""
# The before script (#k225rs4): the opening at main's tip, nothing built (adamic-gate warm --script), published by hash,
# so a member's turn readies its instance before serve asks for a unit and a cold setup (7 to 14 minutes) never runs
# inside a unit's 90 s budget. On for every member once ~/.loom/before-on exists, after a staged instance served green.
before=()
tip=$(git -C "${HOME}/Projects/system/adamic-gate" ls-remote origin refs/heads/main 2> /dev/null | cut -f1)
if [ -f "${HOME}/.loom/before-on" ] && [[ ${tip} =~ ^[0-9a-f]{40}$ ]] &&
	"${HOME}/.loom/bin/adamic-gate" warm --script --sha "${tip}" --gate-inputs "$(cat "${HOME}/.loom/gate-inputs")" > "${HOME}/.loom/before.sh.partial"; then
	mv "${HOME}/.loom/before.sh.partial" "${HOME}/.loom/before.sh"
	hash=$(shasum -a 256 "${HOME}/.loom/before.sh" | cut -c1-64)
	if [ "$(cat "${HOME}/.loom/before.published" 2> /dev/null)" != "${hash}" ]; then
		token=$(python3 - <<'PYTHON'
import base64, hashlib, hmac, json, os, time
secret = open(os.path.expanduser("~/.loom/token-secret")).read().strip().encode()
payload = base64.urlsafe_b64encode(json.dumps({"run": "before-script", "scope": "coordinator", "expires": int(time.time()) + 600}, separators=(",", ":")).encode()).rstrip(b"=")
print((payload + b"." + base64.urlsafe_b64encode(hmac.new(secret, payload, hashlib.sha256).digest()).rstrip(b"=")).decode())
PYTHON
)
		curl -fsS -X PUT --data-binary @"${HOME}/.loom/before.sh" -H "Authorization: Bearer ${token}" "https://loom-wire.kirk-ouimet.workers.dev/public/blobs/${hash}" > /dev/null &&
			echo "${hash}" > "${HOME}/.loom/before.published"
	fi
	[ "$(cat "${HOME}/.loom/before.published" 2> /dev/null)" = "${hash}" ] && before=(--before "${hash}")
fi
cd "${ahra}" || exit 1
# The account's ceiling (Oct 9, 16:53 to 16:55Z: with about 310 Loom members plus the Circles' workers on
# kirk@kirkouimet.com, 175 Loom sessions failed together on 429 Too Many Requests). Turns are sent only while the
# three fleets' running members stay under ~/.loom/codex-ceiling, shared with the Circles' workers, so the pool
# never again grows past what the account serves.
ceiling=$(cat "${HOME}/.loom/codex-ceiling" 2> /dev/null || echo 80)
running=0
for fleet in loom-pool loom-side loom-star; do
	# Only a turn young enough to be serving counts: a serve turn ends by its 115-minute deadline, but Codex leaves some
	# sessions reading Running for hours after they died (Oct 9 17:30Z: 43 of loom-star's "running" members had sat
	# Running since 11:07 to 12:05Z with no activity, so the ceiling counted them, held every finished member, and the
	# pool drained to one asking worker).
	count=$(./node_modules/.bin/ahra ai fleet "${fleet}" --json 2> /dev/null | python3 -c "
import calendar, json, sys, time
live = 0
for member in json.load(sys.stdin):
    since = (member.get('statusSince') or {}).get('since') or ''
    if member.get('session', {}).get('status') != 'Running' or not since:
        continue
    if time.time() - calendar.timegm(time.strptime(since[:19], '%Y-%m-%dT%H:%M:%S')) < 130 * 60:
        live += 1
print(live)" 2> /dev/null || echo 0)
	running=$((running + count))
done
# loom-star's members all serve the star's pool (@system_adamic, Oct 9 10:52Z: double the live star pool, measured in
# steps of 20 against the account's concurrency cap); all of them, whatever their number.
for pair in loom-pool:codex loom-side:codex-side loom-star:codex; do
	fleet=${pair%%:*} pool=${pair#*:}
	prompt=${HOME}/.loom/rearm-${pool}.md
	"${HOME}/.loom/bin/loom-pregate" pool prompt --runner "${runner}" ${before[@]+"${before[@]}"} --until 115m "${pool}" > "${prompt}" || continue
	stagedPrompt=""
	if [ -n "${staged}" ] && [ "${pool}" = codex-side ]; then
		stagedPrompt=${HOME}/.loom/rearm-${pool}-staged.md
		"${HOME}/.loom/bin/loom-pregate" pool prompt --runner "${staged}" ${before[@]+"${before[@]}"} --until 115m "${pool}" > "${stagedPrompt}" || stagedPrompt=""
	fi
	for member in $(./node_modules/.bin/ahra ai fleet "${fleet}" --json 2> /dev/null | python3 -c "
import json, re, sys
for member in json.load(sys.stdin):
    if member.get('session', {}).get('status') == 'Completed':
        number = re.search(r'-(\d+)$', member.get('label') or '')
        print('%s:%s' % (member['id'], number.group(1) if number else 0))"); do
		id=${member%%:*} number=${member#*:}
		if [ "${running}" -ge "${ceiling}" ]; then
			echo "$(date -u +%H:%M:%S) ${fleet} ${id}: held, ${running} members running at the account's ceiling of ${ceiling}"
			continue
		fi
		memberPrompt=${prompt}
		# loom-star is the star's pool whatever a member's number; only loom-pool past LOOM_STAR_INSTANCES serves side work.
		if [ "${fleet}" = loom-pool ] && [ "${number}" -gt "${starInstances}" ]; then
			memberPrompt=${HOME}/.loom/rearm-codex-side.md
			"${HOME}/.loom/bin/loom-pregate" pool prompt --runner "${runner}" ${before[@]+"${before[@]}"} --until 115m codex-side > "${memberPrompt}" || continue
		fi
		# Codex words its refusal many ways ("approval review rejected", "the prior automatic approval rejection still
		# applies", "approval review's rejection remains unresolved"; Oct 9 17:08Z, three members re-sent and refused
		# again), and a worker the pool retired by name is refused with 403 on every turn. Each resend spends a turn, on an
		# account whose ceiling is shared, so either leaves the member alone.
		summary=$(./node_modules/.bin/ahra ai summary "${id}" 2> /dev/null)
		if grep -qiE "approval.{0,20}reject|is retired from this pool" <<< "${summary}"; then
			echo "$(date -u +%H:%M:%S) ${fleet} ${id}: left alone, $(grep -qi "is retired from this pool" <<< "${summary}" && echo "the pool retired its worker" || echo "Codex's approval review rejected the runner")"
			continue
		fi
		if [ -n "${stagedPrompt}" ] && { [ -z "${stagedOn:-}" ] || [ "${stagedOn}" = "${id}" ]; }; then
			./node_modules/.bin/ahra ai send "${id}" --message-file "${stagedPrompt}" > /dev/null 2>&1 || continue
			running=$((running + 1))
			stagedOn=${id}
			echo "${staged} ${id}" > "${staging}"
			echo "$(date -u +%H:%M:%S) ${fleet} ${id}: next serve turn sent on the staged runner ${staged:0:12}"
			continue
		fi
		./node_modules/.bin/ahra ai send "${id}" --message-file "${memberPrompt}" > /dev/null 2>&1 && running=$((running + 1)) && echo "$(date -u +%H:%M:%S) ${fleet} ${id}: next serve turn sent ($(basename "${memberPrompt}" .md | sed 's/^rearm-//'))"
	done
done
