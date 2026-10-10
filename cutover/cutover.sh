#!/usr/bin/env bash
# cutover.sh on|off|check|prove|accept [--ref <branch> --ruleset <id>] [--timer <unit>] [--gate-key <id>]
#            [--delete-lander-key]: the one switch between the old path and the lander (#95gw7a9, rollback #sjywpc5;
# audit #33px83v; today's house #mxg7y3a). Rehearsal, step by step: docs/cutover.md.
#
# Ruleset 24823318 (loom-lander-only-main) restricts update, deletion and non-fast-forward on refs/heads/main, and its
# bypass is the actor type DeployKey. So every deploy key with write bypasses it, not just the lander's, and on is about
# which keys can write, enforced by GitHub itself rather than by callers agreeing to stop:
#
#   on   the ruleset active; the lander's key (Workshop's ~/.ssh/loom_lander) present with write; every other deploy key
#        read-only, the gate key (Cloud's, gateKey) among them; the pusher's timer on Workshop enabled and started, so a
#        reboot keeps landing
#   off  the ruleset disabled, which is all a person's own credential needs to push main again (today's old path: no Mac
#        launch agent pushes main any more); the pusher's timer stopped and disabled, so nothing lands. The lander's key
#        stays, with write and unused, unless --delete-lander-key, which makes GitHub refuse it too
#
# check prints the state and exits 0 only when it is wholly on or wholly off (1 when mixed). A part it can't read, a
# host that doesn't answer or a GitHub read that fails, exits 3 with no state, so neither ever reads as on or off. On
# is read from GitHub's own view too: the ruleset must be listed by GET rules/branches/<ref>, not just marked active.
# prove makes the real attempts for the current state: an empty commit on the ref's tip (its tree the tip's tree),
# pushed with each identity that must be refused, and only GitHub's refusal of that identity for that reason counts. A
# refused push leaves the ref as it was; one that lands is a failed proof; one that never reached GitHub proves nothing
# and fails too. The accepts are real landings, never a probe on main: accept runs only in a rehearsal.
#
# --ref and --ruleset rehearse the same switch on a scratch branch under a scratch ruleset of the same shape, and they
# come together. A rehearsal flips only the scratch ruleset: deploy keys are repo-wide and the pusher lands main, so it
# never touches either (it reads the keys, since on needs them in on's shape). Every write first checks that the
# ruleset targets exactly refs/heads/<ref>, so a rehearsal can't flip a ruleset over main.
set -uo pipefail

repository=system-inc/adamic
mainRuleset=24823318
reference=main
ruleset=${mainRuleset}
# ssh names from Kirk's ssh config, matched by case.
landerHost=Workshop
gateHost=Cloud
# Cloud's deploy key (~/.ssh/adamic_deploy there), read-only since Oct 10; 165739542 before it is gone.
gateKey=165969193
# The pusher's timer on Workshop, a setting so the Go pusher (#nvq0tc8) can bring its own unit.
pusherTimer=loom-pusher.timer
landerTitle="loom lander (workshop only): the one path to main at cutover"
# Workshop's bare clone, relative to its home: the lander's probe builds on it once GitHub refuses the key.
landerObjects=loom-lander/adamic.git
deleteLanderKey=no
rehearsal=no

usage() {
	echo "usage: cutover.sh on|off|check|prove|accept [--ref <branch> --ruleset <id>] [--timer <unit>] [--gate-key <id>] [--delete-lander-key]" >&2
	exit 2
}

[ $# -ge 1 ] || usage
action=$1
shift
while [ $# -gt 0 ]; do
	case $1 in
		--ref | --ruleset | --timer | --gate-key) [ $# -ge 2 ] || usage ;;
	esac
	case $1 in
		--ref) reference=$2; shift 2 ;;
		--ruleset) ruleset=$2; shift 2 ;;
		--timer) pusherTimer=$2; shift 2 ;;
		--gate-key) gateKey=$2; shift 2 ;;
		--delete-lander-key) deleteLanderKey=yes; shift ;;
		*) usage ;;
	esac
done
case ${action} in on | off | check | prove | accept) ;; *) usage ;; esac
case ${reference} in '' | -* | *[!A-Za-z0-9._/-]*) usage ;; esac
case ${ruleset} in '' | *[!0-9]*) usage ;; esac
case ${gateKey} in '' | *[!0-9]*) usage ;; esac
case ${pusherTimer} in -* | *[!A-Za-z0-9@._-]*) usage ;; *.timer) ;; *) usage ;; esac
if [ "${reference}" != main ] || [ "${ruleset}" != "${mainRuleset}" ]; then
	if [ "${reference}" = main ] || [ "${ruleset}" = "${mainRuleset}" ]; then
		echo "cutover: a rehearsal names a scratch --ref and a scratch --ruleset together, neither of them main's" >&2
		exit 2
	fi
	rehearsal=yes
fi
if [ "${deleteLanderKey}" = yes ] && { [ "${action}" != off ] || [ "${rehearsal}" = yes ]; }; then
	echo "cutover: --delete-lander-key is for off on main only: deploy keys are repo-wide" >&2
	exit 2
fi

say() {
	echo "$(date -u +%H:%M:%SZ) cutover: $*"
}

# The ruleset's enforcement, refusing one that doesn't target exactly refs/heads/<ref>.
readRuleset() {
	local line target
	if ! line=$(gh api "repos/${repository}/rulesets/${ruleset}" --jq '"\(.enforcement) \(.conditions.ref_name.include | join(","))"'); then
		say "can't read ruleset ${ruleset}"
		return 1
	fi
	rule=${line%% *}
	target=${line#* }
	if [ "${target}" != "refs/heads/${reference}" ]; then
		say "ruleset ${ruleset} targets ${target:-nothing}, not refs/heads/${reference}, so this switch won't touch it"
		return 1
	fi
	case ${rule} in active | disabled | evaluate) ;; *) say "ruleset ${ruleset} reads as '${rule}'"; return 1 ;; esac
}

# The lander's public key, read where its private half lives.
readLander() {
	local out kind body
	if ! out=$(ssh "${landerHost}" cat .ssh/loom_lander.pub); then
		say "can't read the lander's public key on ${landerHost}"
		return 1
	fi
	read -r kind body _ <<< "${out}"
	case ${kind} in ssh-* | ecdsa-* | sk-*) ;; *) say "${landerHost}'s .ssh/loom_lander.pub isn't a public key"; return 1 ;; esac
	[ -n "${body}" ] || { say "${landerHost}'s .ssh/loom_lander.pub has no key body"; return 1; }
	lander="${kind} ${body}"
}

# The pusher's timer by systemctl show, which fails when systemd can't be asked, where is-active would print inactive.
readTimer() {
	local out
	if ! out=$(ssh "${landerHost}" systemctl --user show -p LoadState -p ActiveState -p UnitFileState "${pusherTimer}"); then
		say "can't read ${pusherTimer} on ${landerHost}"
		return 1
	fi
	timerLoad=$(echo "${out}" | sed -n 's/^LoadState=//p')
	timerActive=$(echo "${out}" | sed -n 's/^ActiveState=//p')
	timerEnabled=$(echo "${out}" | sed -n 's/^UnitFileState=//p')
	if [ -z "${timerLoad}" ] || [ -z "${timerActive}" ]; then
		say "${pusherTimer} on ${landerHost} reads as '$(echo "${out}" | tr '\n' ' ')'"
		return 1
	fi
}

timerOn() {
	[ "${timerLoad}" = loaded ] && [ "${timerActive}" = active ] && [ "${timerEnabled}" = enabled ]
}

timerOff() {
	case ${timerActive} in active | activating | reloading | refreshing) return 1 ;; esac
	case ${timerEnabled} in '' | disabled | masked | masked-runtime) return 0 ;; esac
	return 1
}

# Sets now to on, off or mixed, with a line for the parts; returns 1, with now unknown, when any part can't be read.
readState() {
	local id readOnly kind body onKeys=no landerSays pusherSays
	now=unknown
	readRuleset || return 1
	if ! applied=$(gh api "repos/${repository}/rules/branches/${reference}" --jq "[.[] | select(.ruleset_id == ${ruleset})] | length"); then
		say "can't read GitHub's rules on ${reference}"
		return 1
	fi
	case ${applied} in '' | *[!0-9]*) say "GitHub's rules on ${reference} read as '${applied}'"; return 1 ;; esac
	readLander || return 1
	if ! keys=$(gh api "repos/${repository}/keys" --jq '.[] | "\(.id) \(.read_only) \(.key)"'); then
		say "can't read the deploy keys"
		return 1
	fi
	landerId='' landerWrites=no others=0 gate=missing
	while read -r id readOnly kind body; do
		[ -n "${id}" ] || continue
		if [ "${id}" = "${gateKey}" ]; then
			if [ "${readOnly}" = true ]; then gate=read-only; else gate=write; fi
		fi
		if [ "${kind} ${body}" = "${lander}" ]; then
			landerId=${id}
			if [ "${readOnly}" = false ]; then landerWrites=yes; fi
		elif [ "${readOnly}" = false ]; then
			others=$((others + 1))
			echo "  write key ${id} isn't the lander's"
		fi
	done <<< "${keys}"
	timerLoad='' timerActive='' timerEnabled=''
	if [ "${rehearsal}" = no ]; then
		readTimer || return 1
	fi
	landerSays=absent pusherSays=''
	if [ "${landerWrites}" = yes ]; then
		landerSays="${landerId} write"
	elif [ -n "${landerId}" ]; then
		landerSays="${landerId} read-only"
	fi
	if [ "${rehearsal}" = no ]; then
		pusherSays=", ${pusherTimer} ${timerLoad} ${timerActive} ${timerEnabled:-without-unit-file}"
	fi
	echo "  ruleset ${ruleset} ${rule} (${applied} of its rules on ${reference} by GitHub's view), lander key ${landerSays}, other write keys ${others}, gate key ${gateKey} ${gate}${pusherSays}"
	if [ "${landerWrites}" = yes ] && [ "${others}" = 0 ] && [ "${gate}" = read-only ]; then
		onKeys=yes
	fi
	if [ "${rule}" = active ] && [ "${applied}" -gt 0 ] && [ "${onKeys}" = yes ] && { [ "${rehearsal}" = yes ] || timerOn; }; then
		now=on
	elif [ "${rule}" = disabled ] && [ "${applied}" = 0 ] && { [ "${rehearsal}" = yes ] || timerOff; }; then
		now=off
	else
		now=mixed
	fi
}

enforce() {
	gh api -X PUT "repos/${repository}/rulesets/${ruleset}" -f enforcement="$1" --jq '"  ruleset \(.id) \(.enforcement)"'
}

# The lander's key with write: added, or re-added when it is there read-only. Uses readState's reading.
landerOn() {
	if [ "${landerWrites}" = yes ]; then
		echo "  lander key ${landerId} writes"
		return 0
	fi
	if [ -n "${landerId}" ]; then
		gh api -X DELETE "repos/${repository}/keys/${landerId}" > /dev/null || return 1
	fi
	gh api -X POST "repos/${repository}/keys" -f title="${landerTitle}" -f key="${lander}" -F read_only=false --jq '"  lander key \(.id) added, write"'
}

# Every write deploy key that isn't the lander's goes read-only: deleted, then re-added with the same key and title.
othersReadOnly() {
	local id readOnly kind body title added
	while read -r id readOnly kind body; do
		if [ -z "${id}" ] || [ "${readOnly}" != false ] || [ "${kind} ${body}" = "${lander}" ]; then
			continue
		fi
		title=$(gh api "repos/${repository}/keys/${id}" --jq .title) || return 1
		gh api -X DELETE "repos/${repository}/keys/${id}" > /dev/null || return 1
		added=$(gh api -X POST "repos/${repository}/keys" -f title="${title}" -f key="${kind} ${body}" -F read_only=true --jq .id) || return 1
		echo "  key ${id} (${title}) is read-only as ${added}"
		if [ "${id}" = "${gateKey}" ]; then
			say "the gate key is ${added} now: pass --gate-key ${added}, and set gateKey here"
		fi
	done <<< "${keys}"
}

landerOff() {
	if [ -z "${landerId}" ]; then
		echo "  no lander key to delete"
		return 0
	fi
	gh api -X DELETE "repos/${repository}/keys/${landerId}" > /dev/null || return 1
	echo "  lander key ${landerId} deleted"
}

# probeRun <ref> <tip> <url> [<objects>]: runs where the identity lives, sent whole to bash -s on this Mac or over ssh.
# An empty commit on the ref's tip (its tree the tip's tree), built in a scratch repository so no clone's state moves,
# then pushed with that host's credential. Objects come from the push URL itself (a shallow fetch), or from a local
# clone (relative to the home) when the identity under test can't read. git push's own words come between "push
# begins" and "push ended <code>", so nothing said before (an ssh to the host refused, a fetch) reads as GitHub's.
# shellcheck disable=SC2317,SC2329 # it runs only where declare -f sends it
probeRun() {
	local reference=$1 tip=$2 url=$3 objects=${4:-} probe code
	case ${objects} in /* | '') ;; *) objects=${HOME}/${objects} ;; esac
	probeScratch=$(mktemp -d) || return 3
	trap 'rm -rf "${probeScratch}"' EXIT
	if [ -n "${objects}" ]; then
		git clone -q --bare --shared "${objects}" "${probeScratch}/repository" || return 3
		if ! git -C "${probeScratch}/repository" cat-file -e "${tip}^{tree}" 2> /dev/null; then
			git -C "${probeScratch}/repository" fetch -q --depth 1 --no-tags "${url}" "${tip}" 2> /dev/null
		fi
		if ! git -C "${probeScratch}/repository" cat-file -e "${tip}^{tree}" 2> /dev/null; then
			# A deleted key is refused before GitHub reads any ref, so the probe may stand on the newest main here.
			tip=$(git -C "${probeScratch}/repository" rev-parse -q --verify refs/heads/main) || return 3
			echo "probe: the ref's tip isn't in ${objects}, so it stands on ${tip}"
		fi
	else
		git init -q --bare "${probeScratch}/repository" || return 3
		git -C "${probeScratch}/repository" fetch -q --depth 1 --no-tags "${url}" "${tip}" || return 3
	fi
	git -C "${probeScratch}/repository" cat-file -e "${tip}^{tree}" || { echo "probe: tip ${tip} is not here to build on"; return 3; }
	probe=$(GIT_AUTHOR_NAME=cutover GIT_AUTHOR_EMAIL=cutover@loom GIT_COMMITTER_NAME=cutover GIT_COMMITTER_EMAIL=cutover@loom \
		git -C "${probeScratch}/repository" commit-tree "${tip}^{tree}" -p "${tip}" -m "cutover probe: who may write ${reference}") || return 3
	echo "probe ${probe}"
	echo "push begins"
	git -C "${probeScratch}/repository" push "${url}" "${probe}:refs/heads/${reference}" 2>&1
	code=$?
	echo "push ended ${code}"
	return "${code}"
}

# probe <who> <host|local> <url> <refusal pattern> [<objects>]: sets outcome to landed, refused (GitHub refused it for
# the expected reason), refusedOtherwise (refused for another), or unrun (it never reached GitHub's answer).
probe() {
	local host=$2 url=$3 expect=$4 objects=${5:-} script code=0 pushed
	outcome=unrun probeSha='' refusal='' probeOutput=''
	if ! probeTip=$(gh api "repos/${repository}/git/ref/heads/${reference}" --jq .object.sha); then
		probeOutput="can't read ${reference}'s tip"
		return
	fi
	script=$(printf '%s\nprobeRun "$@"\n' "$(declare -f probeRun)")
	if [ "${host}" = local ]; then
		probeOutput=$(bash -s -- "${reference}" "${probeTip}" "${url}" "${objects}" <<< "${script}" 2>&1) || code=$?
	else
		probeOutput=$(ssh "${host}" bash -s -- "${reference}" "${probeTip}" "${url}" "${objects}" <<< "${script}" 2>&1) || code=$?
	fi
	probeSha=$(echo "${probeOutput}" | sed -n 's/^probe \([0-9a-f]\{40,64\}\)$/\1/p' | head -n 1)
	case ${probeOutput} in *"push begins"*"push ended "*) ;; *) return ;; esac
	pushed=${probeOutput#*push begins}
	if [ "${code}" = 0 ] && echo "${pushed}" | grep -qx 'push ended 0'; then
		outcome=landed
	elif echo "${pushed}" | grep -qx 'push ended 0'; then
		return
	elif [ -n "${expect}" ] && refusal=$(echo "${pushed}" | grep -m1 -E "${expect}"); then
		outcome=refused
	else
		outcome=refusedOtherwise
	fi
}

# What a probe said, on one line, for a failure.
probeSaid() {
	echo "${probeOutput}" | grep -v -e '^probe ' -e '^push begins$' | tr '\n' ' '
}

# mustRefuse <who> <host|local> <url> <refusal pattern> [<objects>]
mustRefuse() {
	probe "$@"
	case ${outcome} in
		refused) say "refused, as it must be: $1 on ${probeTip:0:12}: ${refusal}" ;;
		landed) say "PROOF FAILED: $1 pushed ${reference} to ${probeSha}: $(probeSaid)"; return 1 ;;
		refusedOtherwise) say "no proof: $1 was refused, but not with /$4/, so not by what must refuse it: $(probeSaid)"; return 1 ;;
		*) say "no proof: $1's push never reached GitHub's answer: $(probeSaid)"; return 1 ;;
	esac
}

# mustLand <who> <host|local> <url>: the push lands, and GitHub's ref reads back as the probe.
mustLand() {
	local tip
	probe "$1" "$2" "$3" ''
	if [ "${outcome}" != landed ]; then
		say "ACCEPT FAILED: $1 didn't land on ${reference} (${outcome}): $(probeSaid)"
		return 1
	fi
	if ! tip=$(gh api "repos/${repository}/git/ref/heads/${reference}" --jq .object.sha); then
		say "ACCEPT FAILED: $1 pushed, but ${reference} can't be read back"
		return 1
	fi
	if [ "${tip}" != "${probeSha}" ]; then
		say "ACCEPT FAILED: $1 pushed, but ${reference} is ${tip}, not the probe ${probeSha:-(none)}"
		return 1
	fi
	say "landed, as it must: $1 moved ${reference} to ${probeSha:0:12}"
}

prove() {
	local failed=0
	readState || { say "can't read the switch, so no probe runs"; return 1; }
	[ "${now}" != mixed ] || { say "${reference} is mixed, so no probe runs"; return 1; }
	say "${reference} is ${now}, probing what must be refused"
	if [ "${now}" = on ]; then
		mustRefuse "Kirk's credential from this Mac" local "git@github.com:${repository}.git" 'GH013' || failed=1
		# ssh:// rather than git@github.com:, which Cloud's git config rewrites to https (no credential there).
		mustRefuse "${gateHost}'s gate key" "${gateHost}" "ssh://git@github.com/${repository}.git" 'marked as read only' || failed=1
	elif [ -z "${landerId}" ]; then
		mustRefuse "the lander's key" "${landerHost}" "git@github-lander:${repository}.git" 'Permission denied \(publickey\)' "${landerObjects}" || failed=1
	elif [ "${rehearsal}" = yes ]; then
		say "a rehearsal's off keeps every key, so nothing on ${reference} must be refused; accept proves its open path"
	else
		say "off keeps the lander's key ${landerId}, so GitHub would take its push: the lander is quiet because ${pusherTimer} is ${timerActive} and ${timerEnabled:-without a unit file}, read above; nothing must be refused"
	fi
	return "${failed}"
}

# accept, in a rehearsal only: the identity the state lets through pushes a probe, and it must land and read back. It is
# the other half of the rehearsal, and the mutant for prove: mustRefuse must read a landed push as a failed proof.
accept() {
	[ "${rehearsal}" = yes ] || { say "accept never runs on main: its accepts are real landings"; return 2; }
	readState || { say "can't read the switch, so no probe runs"; return 1; }
	[ "${now}" != mixed ] || { say "${reference} is mixed, so no probe runs"; return 1; }
	say "${reference} is ${now}, probing the identity that must land"
	if [ "${now}" = on ]; then
		mustLand "the lander's key" "${landerHost}" "git@github-lander:${repository}.git"
	else
		mustLand "Kirk's credential from this Mac" local "git@github.com:${repository}.git"
	fi
}

# Reads the switch after on or off: exits 0 only when it reads as wanted and every step went through.
finish() {
	local want=$1 failed=$2
	readState || { say "can't read the switch after ${action}"; exit 3; }
	say "${reference} is ${now}"
	[ "${now}" = "${want}" ] && [ "${failed}" = 0 ]
	exit $?
}

case ${action} in
	check)
		readState || { say "can't read the switch, so it reads as neither on nor off"; exit 3; }
		say "${reference} is ${now}"
		[ "${now}" != mixed ]
		;;
	on)
		readState || { say "can't read the switch, so on changes nothing"; exit 3; }
		if [ "${rehearsal}" = no ]; then
			landerOn || { say "on stopped: the lander's key isn't in place"; finish on 1; }
			othersReadOnly || { say "on stopped: a key isn't read-only yet"; finish on 1; }
		fi
		enforce active || { say "on stopped: ruleset ${ruleset} isn't active"; finish on 1; }
		if [ "${rehearsal}" = no ]; then
			if ssh "${landerHost}" systemctl --user enable --now "${pusherTimer}"; then
				echo "  ${pusherTimer} enabled and started on ${landerHost}"
			else
				say "on stopped: ${pusherTimer} isn't enabled on ${landerHost}"
				finish on 1
			fi
		fi
		finish on 0
		;;
	off)
		# The human path first, then the lander's quiet: each step runs even when one before it failed.
		failed=0
		if readRuleset; then enforce disabled || failed=1; else failed=1; fi
		if [ "${rehearsal}" = no ]; then
			if ! readTimer; then
				failed=1
			elif [ "${timerLoad}" = not-found ]; then
				echo "  no ${pusherTimer} on ${landerHost}, so nothing to stop"
			elif ssh "${landerHost}" systemctl --user disable --now "${pusherTimer}"; then
				echo "  ${pusherTimer} stopped and disabled on ${landerHost}"
			else
				say "${pusherTimer} isn't stopped on ${landerHost}"
				failed=1
			fi
			if [ "${deleteLanderKey}" = yes ]; then
				if readLander && keys=$(gh api "repos/${repository}/keys" --jq '.[] | "\(.id) \(.read_only) \(.key)"'); then
					landerId=$(echo "${keys}" | awk -v lander="${lander}" '$3" "$4 == lander {print $1; exit}')
					landerOff || failed=1
				else
					failed=1
				fi
			fi
		fi
		finish off "${failed}"
		;;
	prove)
		prove
		;;
	accept)
		accept
		;;
esac
