#!/bin/bash
# select-box.sh: a fast gate's selection on a warm gate box (#7cmv2g3; @system_adamic, Oct 9 17:43Z: 318eef6a's select
# took 94 s of its 176, a cold 4-CPU Codex instance paying the clone, Go's setup and a cold go list before deciding
# anything). The same run.py --select select.sh runs on the pool, at the same tools sha, into the same directory
# (/tmp/loom-select/<gated>, which the selection's env names), on a tree of its own on the box: ~/loom-select/adamic,
# fetched to the gated sha each time, with the box's Go caches, so a selection is a fetch and a warm go list.
#
#	select-box.sh <gated> <base> <base name> <tools sha> <select.tgz>
#
# Writes <select.tgz> (select.json, changed-paths.txt, status.txt, as the pool unit's) and prints the box it ran on, or
# exits nonzero with nothing written, and fast.sh places the pool unit as before. It never decides a red: a selection
# that leaves no select.json goes to the pool, whose unit's word stands. The whole try is held to LOOM_SELECT_BUDGET
# seconds (30). Boxes are tried least loaded first; one that is busy with another selection, has no tree yet (it starts
# a clone in the background and says so) or can't fetch hands on to the next.
#
# The box watcher's trees (~/fast-gate/tree*, tools*) are never read or touched. Workshop alone by default (Loom, Oct 9
# 18:1xZ: proven there at 5 to 8 s, byte-identical on 7 real jobs; Server went unreachable under cold setups beside a
# warm clone). Home and Cloud hold warm trees too. Threadripper is Cloud under another user, sharing its /tmp, so at
# most one of the two belongs in LOOM_SELECT_BOXES.
set -uo pipefail
gated=$1 base=$2 baseName=$3 tools=$4 archive=$5
for sha in "${gated}" "${base}" "${tools}"; do
	[[ ${sha} =~ ^[0-9a-f]{40}$ ]] || { echo "select-box: wants three full shas, got ${sha}" >&2; exit 2; }
done
boxes=${LOOM_SELECT_BOXES:-workshop}
budget=${LOOM_SELECT_BUDGET:-30}
started=${SECONDS}
# One connection per box kept for ten minutes, so the probe and the selection skip ssh's handshake (0.7 s to 0.1 s).
ssh=(ssh -o BatchMode=yes -o ConnectTimeout=3 -o ControlMaster=auto -o "ControlPath=${HOME}/.ssh/loom-select-%r@%h:%p" -o ControlPersist=600)

# What runs on the box: stdout is the selection's tar.gz and nothing else, everything said goes to stderr.
# Exit 0 with the archive; 3 when this box can't now (busy, cold, a fetch failed); 1 when run.py left no select.json.
read -r -d '' remote <<'BOX'
set -uo pipefail
sha=$1 base=$2 baseName=$3 tools=$4
root=${HOME}/loom-select tree=${HOME}/loom-select/adamic toolsTree=${HOME}/loom-select/tools
mkdir -p "${root}"
exec 9> "${root}/lock"
flock -n 9 || { echo "select-box: $(hostname) is busy with another selection" >&2; exit 3; }
export GIT_TERMINAL_PROMPT=0
if [ ! -d "${tree}/.git" ]; then
	# The first clone takes minutes, far past a selection's budget, so it runs detached under its own lock and lands
	# whole (renamed into place only when the submodules are in), and this selection goes to the pool. It yields CPU and
	# disk to the box's gates (Loom, Oct 9: never a full-speed clone beside a gate's setup).
	setsid -f nice -n 19 ionice -c 3 bash -c 'exec 8> "$1/warm.lock"; flock -n 8 || exit 0; partial=$1/adamic.partial-$$
		git clone -q https://github.com/system-inc/adamic.git "${partial}" && git -C "${partial}" submodule update -q --init --recursive && mv "${partial}" "$1/adamic"' \
		warm "${root}" > "${root}/warm.log" 2>&1 < /dev/null
	echo "select-box: $(hostname) has no tree yet; cloning it in the background" >&2
	exit 3
fi
source "${HOME}/adamic-tools/env.sh" >&2
mkdir -p -m 1777 "${TMPDIR:-/tmp}"
# Holding the lock means nothing else uses this tree, so a git lock in it is stale (a selection cut off at its budget).
find "${tree}/.git" -maxdepth 6 -name index.lock -delete 2> /dev/null
# Nothing here reads the script's stdin, which is the rest of this script.
git -C "${tree}" fetch -q origin "${sha}" "${base}" "${tools}" < /dev/null && git -C "${tree}" switch -q --detach "${sha}" &&
	git -C "${tree}" submodule update -q --init --recursive < /dev/null || { echo "select-box: $(hostname) couldn't check out ${sha}" >&2; exit 3; }
if [ -d "${toolsTree}" ]; then
	git -C "${toolsTree}" switch -q --detach "${tools}" || { echo "select-box: $(hostname) tools checkout failed" >&2; exit 3; }
else
	git -C "${tree}" worktree add -q --detach "${toolsTree}" "${tools}" || { echo "select-box: $(hostname) tools worktree failed" >&2; exit 3; }
fi
selection=/tmp/loom-select/${sha}
[ -e "${selection}" ] && mv "${selection}" "${selection}.replaced-$$"
mkdir -p "${selection}"
nice python3 "${toolsTree}/cloud/fast-gate/run.py" --select --tree "${tree}" --sha "${sha}" --base "${base}" --base-name "${baseName}" --tools "${toolsTree}" --out "${selection}" >&2 < /dev/null
code=$?
[ -f "${selection}/select.json" ] && tar -C "${selection}" -czf - .
written=$([ -f "${selection}/select.json" ] && echo 0 || echo 1)
# The archive is the record: the directory goes, file by file, so the next selection of this sha starts empty.
rm -f "${selection}/select.json" "${selection}/changed-paths.txt" "${selection}/status.txt"
rmdir "${selection}" 2> /dev/null
# run.py leaves an empty build-store-* queue (audits/, spool/) beside the tree on every run; only empty ones go, and under
# the lock none is in use.
rmdir "${root}"/build-store-*/audits "${root}"/build-store-*/spool "${root}"/build-store-* 2> /dev/null
echo "select-box: $(hostname) run.py --select exited ${code}" >&2
exit "${written}"
BOX

# Least loaded first: each box's one-minute load over its CPUs, asked of all at once, in at most 3 s.
probes=$(mktemp -d)
for box in ${boxes}; do
	timeout 3 "${ssh[@]}" "${box}" 'echo $(cut -d" " -f1 /proc/loadavg) $(nproc --all)' > "${probes}/${box}" 2> /dev/null &
done
wait
order=$(for box in ${boxes}; do
	read -r load cpus < "${probes}/${box}" 2> /dev/null && [ -n "${cpus:-}" ] && awk -v box="${box}" -v load="${load}" -v cpus="${cpus}" 'BEGIN {printf "%.3f %s\n", load / cpus, box}'
done | sort -n | awk '{print $2}')
rm -f "${probes}"/* && rmdir "${probes}"
[ -n "${order}" ] || { echo "select-box: no box answered (${boxes})" >&2; exit 3; }

partial=${archive}.partial-$$
for box in ${order}; do
	left=$((budget - (SECONDS - started)))
	[ "${left}" -gt 0 ] || { echo "select-box: out of budget (${budget} s) before ${box}" >&2; exit 3; }
	timeout "${left}" "${ssh[@]}" "${box}" bash -s -- "$(printf '%q ' "${gated}" "${base}" "${baseName}" "${tools}")" <<< "${remote}" > "${partial}"
	code=$?
	if [ "${code}" = 0 ] && tar -tzf "${partial}" 2> /dev/null | grep -qx './select.json'; then
		mv "${partial}" "${archive}"
		echo "${box}"
		exit 0
	fi
	rm -f "${partial}"
	case ${code} in
		3 | 255) echo "select-box: ${box} passed (exit ${code}) after $((SECONDS - started)) s" >&2 ;;
		124) echo "select-box: ${box} ran past the budget (${budget} s)" >&2; exit 3 ;;
		*) echo "select-box: ${box}'s selection left no select.json (exit ${code}); the pool decides" >&2; exit 1 ;;
	esac
done
echo "select-box: no box took it (${order//$'\n'/ })" >&2
exit 3
