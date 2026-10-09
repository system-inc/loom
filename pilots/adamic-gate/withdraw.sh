#!/bin/bash
# withdraw.sh: an owner takes a fast job out of the gate's queue because it doesn't need a verdict now: superseded by a
# later cut, folded into a train, or no longer wanted (@system_adamic, Oct 9 18:23Z: 186 current tips had no verdict,
# a day of backlog at the gate's rate). The job's line leaves the queue; the branch is never touched.
#
#	pilots/adamic-gate/withdraw.sh <sha> <who> <reason>     # a copy in ~/.loom/bin
#
# It writes <sha>.withdrawn (who, why, when), which clock.py reads as the job's answer, counted apart from verdicts. A job
# waiting or running is also cancelled through the server's own cancel (<sha>.cancel), so it stops and is never served
# again; a decided one is left as it is. LOOM_FAST_JOBS points it at a test's own directory.
set -uo pipefail
jobs=${LOOM_FAST_JOBS:-${HOME}/.loom/jobs/fast}
sha=${1:-} who=${2:-} reason=${3:-}
[[ ${sha} =~ ^[0-9a-f]{40}$ ]] || { echo "withdraw: want a full sha, got '${sha}'" >&2; exit 1; }
[ -n "${who}" ] && [ -n "${reason}" ] || { echo "usage: withdraw.sh <sha> <who> <reason>" >&2; exit 1; }
[ -f "${jobs}/${sha}.json" ] || { echo "withdraw: ${sha:0:12} has no job file in ${jobs}" >&2; exit 1; }
[ -e "${jobs}/${sha}.withdrawn" ] && { echo "withdraw: ${sha:0:12} is already withdrawn: $(cat "${jobs}/${sha}.withdrawn")"; exit 0; }
echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) by ${who}: ${reason}" > "${jobs}/${sha}.withdrawn"
# Live (running, or waiting with no verdict): the server's cancel stops it and records it cancelled, so it never serves.
if [ -e "${jobs}/${sha}.running" ] || [ ! -e "${jobs}/${sha}.verdict" ]; then
	touch "${jobs}/${sha}.cancel"
	echo "withdraw: ${sha:0:12} withdrawn by ${who} (${reason}); cancel asked, it leaves the queue"
else
	echo "withdraw: ${sha:0:12} withdrawn by ${who} (${reason}); it was already decided, so nothing to stop"
fi
