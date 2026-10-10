#!/usr/bin/env bash
# cutover.sh on|off|check|prove|accept [--ref <branch> --ruleset <id>]: the one switch between the old path and the lander
# (#95gw7a9, rollback #sjywpc5; audit #33px83v).
#
# Ruleset 24823318 restricts update, deletion and non-fast-forward on refs/heads/main, and its bypass is the actor type
# DeployKey. So every deploy key with write bypasses it, not just the lander's, and the switch is about which keys can
# write, enforced by GitHub itself rather than by callers agreeing to stop:
#
#   on   the ruleset active; the lander's key present with write; every other deploy key read-only; the Mac's push-main
#        callers (launchd gate-lane, star-train, pr-lane) booted out so they don't spin on GH013
#   off  the ruleset disabled; the lander's key deleted, so GitHub refuses the lander; then loom-pusher.timer stopped on
#        workshop, so the lander doesn't spin on refusals (GitHub's refusal is the guarantee, the stop is quiet); the
#        Mac's callers bootstrapped
#
# Its private half never leaves workshop (~/.ssh/loom_lander), and on re-adds the same public key. check prints the
# state and exits 0 only when it is wholly on or wholly off, so a half-flipped switch never reads as either. On is read
# from GitHub's own view too: the ruleset must be listed by GET rules/branches/<ref>, not just marked active. prove makes
# the real attempts for the current state: an empty commit on the ref's tip (its tree is the tip's tree), pushed with
# each identity that must be refused. A refused push leaves the ref as it was; an accepted one is reported as a failed
# proof. The accepts are real landings, never a probe: the lander's next landing when on, push-main's when off.
#
# --ref and --ruleset rehearse the same switch on a scratch branch under a scratch ruleset of the same shape, so the
# rollback is tested both ways before main is touched.
set -euo pipefail

repository=system-inc/adamic
reference=main
ruleset=24823318
landerTitle="loom lander (workshop only): the one path to main at cutover"
macAgents=(com.adamic.gate-lane com.adamic.star-train com.adamic.pr-lane)

usage() {
	echo "usage: cutover.sh on|off|check|prove|accept [--ref <branch> --ruleset <id>]" >&2
	exit 2
}

[ $# -ge 1 ] || usage
action=$1
shift
while [ $# -gt 0 ]; do
	case $1 in
		--ref) reference=$2; shift 2 ;;
		--ruleset) ruleset=$2; shift 2 ;;
		*) usage ;;
	esac
done

say() {
	echo "$(date -u +%H:%M:%SZ) cutover: $*"
}

landerKey() {
	ssh Workshop 'cat ~/.ssh/loom_lander.pub' | awk '{print $1" "$2}'
}

# One line per deploy key: id, read_only, the key's type and body.
deployKeys() {
	gh api "repos/${repository}/keys" --jq '.[] | "\(.id) \(.read_only) \(.key)"'
}

enforcement() {
	gh api "repos/${repository}/rulesets/${ruleset}" --jq .enforcement
}

agentLoaded() {
	launchctl print "gui/$(id -u)/$1" > /dev/null 2>&1
}

# on, off or mixed, with a line for each part that disagrees.
state() {
	local lander keys rule applied landerWrites=no others=0 agentsUp=0 agentsDown=0
	lander=$(landerKey)
	keys=$(deployKeys)
	rule=$(enforcement)
	applied=$(gh api "repos/${repository}/rules/branches/${reference}" --jq "[.[] | select(.ruleset_id == ${ruleset})] | length")
	while read -r id readOnly kind body; do
		[ -n "${id}" ] || continue
		if [ "${kind} ${body}" = "${lander}" ]; then
			[ "${readOnly}" = false ] && landerWrites=yes
		elif [ "${readOnly}" = false ]; then
			others=$((others + 1))
			echo "  write key ${id} isn't the lander's" >&2
		fi
	done <<< "${keys}"
	# The Mac's callers only push main, so a rehearsal on a scratch ref leaves them out of its state.
	if [ "${reference}" = main ]; then
		for agent in "${macAgents[@]}"; do
			if agentLoaded "${agent}"; then agentsUp=$((agentsUp + 1)); else agentsDown=$((agentsDown + 1)); fi
		done
	fi
	echo "  ruleset ${ruleset} ${rule} (${applied} of its rules on ${reference} by GitHub's view), lander key ${landerWrites}, other write keys ${others}, Mac callers ${agentsUp} up ${agentsDown} down" >&2
	if [ "${rule}" = active ] && [ "${applied}" -gt 0 ] && [ "${landerWrites}" = yes ] && [ "${others}" = 0 ] && [ "${agentsUp}" = 0 ]; then
		echo on
	elif [ "${rule}" = disabled ] && [ "${applied}" = 0 ] && [ "${landerWrites}" = no ] && [ "${agentsDown}" = 0 ]; then
		echo off
	else
		echo mixed
	fi
}

# Every write deploy key that isn't the lander's goes read-only: deleted, then re-added with the same key and title.
othersReadOnly() {
	local lander title
	lander=$(landerKey)
	while read -r id readOnly kind body; do
		[ -n "${id}" ] && [ "${readOnly}" = false ] && [ "${kind} ${body}" != "${lander}" ] || continue
		title=$(gh api "repos/${repository}/keys/${id}" --jq .title)
		gh api -X DELETE "repos/${repository}/keys/${id}" > /dev/null
		gh api -X POST "repos/${repository}/keys" -f title="${title}" -f key="${kind} ${body}" -F read_only=true --jq '"  key \(.id) \(.title): read-only"'
	done <<< "$(deployKeys)"
}

landerOn() {
	local lander
	lander=$(landerKey)
	if deployKeys | awk '{print $3" "$4}' | grep -qxF "${lander}"; then
		return
	fi
	gh api -X POST "repos/${repository}/keys" -f title="${landerTitle}" -f key="${lander}" -F read_only=false --jq '"  lander key \(.id) added, write"'
}

landerOff() {
	local lander
	lander=$(landerKey)
	while read -r id readOnly kind body; do
		[ "${kind} ${body}" = "${lander}" ] || continue
		gh api -X DELETE "repos/${repository}/keys/${id}" > /dev/null
		echo "  lander key ${id} deleted"
	done <<< "$(deployKeys)"
}

# The probe, run where the identity lives (bash -s on this Mac, or on a host over ssh): an empty commit on the ref's tip
# (tip passed in, its tree the tip's tree), built in a scratch repository so no clone's state moves, then pushed with
# that host's credential. Objects come from the push URL itself (a shallow fetch), or from a local clone when the
# identity under test can't read (the lander's key once it is deleted). Prints git's answer; exits as git push did.
probeScript='
set -u
reference=$1 tip=$2 url=$3 objects=${4:-}
scratch=$(mktemp -d)
trap "rm -rf ${scratch}" EXIT
if [ -n "${objects}" ]; then
	git clone -q --bare --shared "${objects}" "${scratch}" || exit 3
else
	git init -q --bare "${scratch}" && git -C "${scratch}" fetch -q --depth 1 --no-tags "${url}" "${tip}" || exit 3
fi
if [ -n "${objects}" ] && ! git -C "${scratch}" cat-file -e "${tip}^{tree}" 2> /dev/null; then
	git -C "${scratch}" fetch -q --depth 1 --no-tags "${url}" "${tip}" 2> /dev/null || true
fi
if ! git -C "${scratch}" cat-file -e "${tip}^{tree}" 2> /dev/null && [ -n "${objects}" ]; then
	# A deleted key is refused before GitHub reads any ref, so the probe may stand on the newest main this clone holds.
	tip=$(git -C "${scratch}" rev-parse -q --verify refs/heads/main)
	echo "probe: the ref'"'"'s tip isn'"'"'t in ${objects}, so it stands on ${tip}"
fi
git -C "${scratch}" cat-file -e "${tip}^{tree}" || { echo "probe: tip ${tip} is not here to build on"; exit 3; }
probe=$(GIT_AUTHOR_NAME=cutover GIT_AUTHOR_EMAIL=cutover@loom GIT_COMMITTER_NAME=cutover GIT_COMMITTER_EMAIL=cutover@loom \
	git -C "${scratch}" commit-tree "${tip}^{tree}" -p "${tip}" -m "cutover probe: this push must be refused")
echo "probe ${probe}"
git -C "${scratch}" push "${url}" "${probe}:refs/heads/${reference}"
'

# Expects a refusal at GitHub: prints the proof line; exit 1 if the push landed or never reached GitHub.
proveRefused() {
	local who=$1 host=$2 url=$3 objects=${4:-} tip output refusal code=0
	tip=$(gh api "repos/${repository}/git/ref/heads/${reference}" --jq .object.sha)
	if [ "${host}" = local ]; then
		output=$(bash -c "${probeScript}" probe "${reference}" "${tip}" "${url}" "${objects}" 2>&1) || code=$?
	else
		output=$(ssh "${host}" bash -s -- "${reference}" "${tip}" "${url}" "${objects}" <<< "${probeScript}" 2>&1) || code=$?
	fi
	if [ "${code}" = 0 ]; then
		say "PROOF FAILED: ${who} pushed ${reference}: ${output}"
		return 1
	fi
	if [ "${code}" = 3 ]; then
		say "no proof: ${who}'s probe couldn't be built: ${output}"
		return 1
	fi
	# Only GitHub's own refusal counts. A push that failed on this side (no credential, a URL rewritten to https, a
	# network error) never reached the rule, so it proves nothing.
	if ! refusal=$(echo "${output}" | grep -m1 -E 'GH013|Permission to .* denied|Permission denied \(publickey\)|ERROR: .*(marked as read only|[Dd]eploy key|denied)|remote rejected'); then
		say "no proof: ${who}'s push failed before GitHub refused it: $(echo "${output}" | grep -v '^probe' | tr '\n' ' ')"
		return 1
	fi
	say "refused, as it must be: ${who} on ${tip:0:12}: ${refusal}"
}

prove() {
	local now failed=0
	now=$(state)
	[ "${now}" != mixed ] || { say "the switch is mixed, so no probe runs"; return 1; }
	say "${reference} is ${now}, probing"
	if [ "${now}" = on ]; then
		proveRefused "Kirk's credential from this Mac" local "git@github.com:${repository}.git" || failed=1
		# ssh:// rather than git@github.com:, which Cloud's git config rewrites to https (no credential there).
		proveRefused "threadripper's deploy key" threadripper "ssh://git@github.com/${repository}.git" || failed=1
	else
		proveRefused "the lander's key" workshop "git@github-lander:${repository}.git" '~/loom-lander/adamic.git' || failed=1
	fi
	return "${failed}"
}

# accept, on a scratch ref only: the identity the state lets through pushes a probe, and it must land. It is the other
# half of the rehearsal, and the mutant for prove: proveRefused must read the landed push as a failed proof. Never on
# main, where the accepts are real landings.
accept() {
	local now
	[ "${reference}" != main ] || { say "accept never runs on main"; return 2; }
	now=$(state)
	[ "${now}" != mixed ] || { say "the switch is mixed, so no probe runs"; return 1; }
	say "${reference} is ${now}, probing an identity that must land"
	if [ "${now}" = on ]; then
		! proveRefused "the lander's key" workshop "git@github-lander:${repository}.git" '~/loom-lander/adamic.git'
	else
		! proveRefused "Kirk's credential from this Mac" local "git@github.com:${repository}.git"
	fi
}

case ${action} in
	accept)
		accept
		;;
	check)
		now=$(state)
		say "${reference} is ${now}"
		[ "${now}" != mixed ]
		;;
	on)
		landerOn
		othersReadOnly
		gh api -X PUT "repos/${repository}/rulesets/${ruleset}" -f enforcement=active --jq '"  ruleset \(.id) \(.enforcement)"'
		if [ "${reference}" = main ]; then
			ssh Workshop 'systemctl --user start loom-pusher.timer' && echo "  loom-pusher.timer started"
			for agent in "${macAgents[@]}"; do
				launchctl bootout "gui/$(id -u)/${agent}" 2> /dev/null && echo "  ${agent} booted out" || true
			done
		fi
		now=$(state)
		say "${reference} is ${now}"
		[ "${now}" = on ]
		;;
	off)
		gh api -X PUT "repos/${repository}/rulesets/${ruleset}" -f enforcement=disabled --jq '"  ruleset \(.id) \(.enforcement)"'
		landerOff
		if [ "${reference}" = main ]; then
			ssh Workshop 'systemctl --user stop loom-pusher.timer' && echo "  loom-pusher.timer stopped"
			for agent in "${macAgents[@]}"; do
				agentLoaded "${agent}" || { launchctl bootstrap "gui/$(id -u)" "${HOME}/Library/LaunchAgents/${agent}.plist" && echo "  ${agent} bootstrapped"; }
			done
		fi
		now=$(state)
		say "${reference} is ${now}"
		[ "${now}" = off ]
		;;
	prove)
		prove
		;;
	*)
		usage
		;;
esac
