#!/bin/bash
# requeue.sh: serve a decided or stuck fast job again, now, through fast.sh --once (#rrkfa72; by hand about 20 times on
# Oct 9 before it was a tool). The job's tier and tools are set first, then everything that would keep it decided or
# feed it a stale selection is moved aside, never deleted, and fast.sh --once starts detached, as fast-pump.sh did.
#
#	pilots/adamic-gate/requeue.sh <sha> [--tier N] [--tools <sha40>]    # a copy in ~/.loom/bin
#
# --tier is the job file's "priority" (0 to 1000, the star's at 30 and up), --tools its "tools" (the adamic-gate commit
# its selection and phases run at). LOOM_FAST_JOBS, LOOM_BIN and LOOM_FAST_LOG point it at a test's own directories.
set -uo pipefail
bin=${LOOM_BIN:-${HOME}/.loom/bin}
jobs=${LOOM_FAST_JOBS:-${HOME}/.loom/jobs/fast}
log=${LOOM_FAST_LOG:-${HOME}/.loom/fast.log}
usage="usage: requeue.sh <sha> [--tier N] [--tools <sha40>]"
refuse() { echo "requeue: $1" >&2; exit 1; }

[ $# -ge 1 ] || refuse "${usage}"
sha=$1
shift
tier="" tools=""
while [ $# -gt 0 ]; do
	case "$1" in
		--tier) [ $# -ge 2 ] || refuse "--tier takes a number"; tier=$2; shift 2 ;;
		--tools) [ $# -ge 2 ] || refuse "--tools takes a 40-character sha"; tools=$2; shift 2 ;;
		*) refuse "unknown argument $1; ${usage}" ;;
	esac
done
[[ ${sha} =~ ^[0-9a-f]{40}$ ]] || refuse "${sha} is not a 40-character lowercase sha"
# serve reads a priority outside 0 to 1000, or one that isn't a JSON integer, as 0, so a bad tier would run silently at 0.
[ -z "${tier}" ] || { [[ ${tier} =~ ^[0-9]+$ ]] && [ "${tier}" -le 1000 ]; } || refuse "--tier ${tier} is not a whole number from 0 to 1000"
# runSelection and runPhases fall back to the fast-gate branch's tip for tools that aren't a full sha, so a typo would
# run the wrong tools without a word.
[ -z "${tools}" ] || [[ ${tools} =~ ^[0-9a-f]{40}$ ]] || refuse "--tools ${tools} is not a 40-character lowercase sha"
short=${sha:0:12} work=${jobs}/${sha}.work
[ -f "${jobs}/${sha}.json" ] || refuse "${short} has no job file at ${jobs}/${sha}.json"
# A running job's markers belong to the server serving it: moving its .placed or selection would corrupt that run.
[ -e "${jobs}/${sha}.running" ] && refuse "${short} is running (${jobs}/${sha}.running); cancel it or let it finish first"
for tool in fast.sh detach.sh; do
	[ -x "${bin}/${tool}" ] || refuse "no ${bin}/${tool} to start the job with"
done

# The tier and tools go in before anything moves: while the job still has its verdict the server never picks it up, so
# it can't start on the old fields in the moment between the two. A job file that won't parse stops here, with nothing
# moved.
if [ -n "${tier}${tools}" ]; then
	python3 - "${jobs}/${sha}.json" "${tier}" "${tools}" <<'PY' || refuse "couldn't set the tier or tools in ${jobs}/${sha}.json"
import json, os, sys
path, tier, tools = sys.argv[1:]
job = json.load(open(path))
short = os.path.basename(path)[:12]
if tier:
    print("requeue: %s set priority %s -> %s in %s" % (short, job.get("priority", "(unset)"), tier, path))
    job["priority"] = int(tier)
if tools:
    print("requeue: %s set tools %s -> %s in %s" % (short, job.get("tools") or "(unset)", tools, path))
    job["tools"] = tools
# Written beside the job and renamed over it, so the server and the watcher never read half a file. The temporary name
# doesn't end in .json, so the server's listing never takes it for a job.
with open(path + ".requeue-partial", "w") as out:
    json.dump(job, out)
os.replace(path + ".requeue-partial", path)
PY
fi

stamp=$(date -u +%H%M%S)
# aside renames one file to <file>.requeued-<HHMMSS> (UTC, as fast.log's times are), with a counter when a requeue in
# the same second already took that name: mv would overwrite it, and the earlier copy is the one being kept.
aside() {
	local path=$1 target=$1.requeued-${stamp} count=1
	[ -e "${path}" ] || return 0
	while [ -e "${target}" ]; do
		count=$((count + 1))
		target=${path}.requeued-${stamp}-${count}
	done
	mv "${path}" "${target}" || refuse "couldn't move ${path} aside"
	echo "requeue: ${short} moved ${path#"${jobs}/"} aside to ${target#"${jobs}/"}"
}
aside "${jobs}/${sha}.verdict"
aside "${jobs}/${sha}.placed"
# Requeue is an explicit ask to run the job: a cancel already served, or one still waiting for the server (which would
# stop this run within 10 s of its start), is moved aside with the verdict it left.
aside "${jobs}/${sha}.cancelled"
aside "${jobs}/${sha}.cancel"
# A select-mode job's runSelection clears these itself, but serve hands an existing select/select.json to verify.sh
# whatever the job's mode, so a stale selection would still choose this run's tests.
aside "${work}/select.tgz"
aside "${work}/select/select.json"

# fast.sh --once serves in the foreground, so it starts in a session of its own (detach.sh), its output joining the
# server's log, and outlives this shell.
pid=$("${bin}/detach.sh" "${log}" "${bin}/fast.sh" --once "${sha}") || refuse "detach.sh couldn't start fast.sh --once ${short}"
echo "requeue: ${short} started ${bin}/fast.sh --once ${sha} detached (pid ${pid}), output in ${log}"
# --once touches .running before it serves, and refuses in the same breath when the server took the job first: wait for
# the one or the other, so the operator hears a refusal here rather than in the log.
for _ in $(seq 1 100); do
	[ -e "${jobs}/${sha}.running" ] || [ -e "${jobs}/${sha}.verdict" ] && exit 0
	kill -0 "${pid}" 2> /dev/null || break
	sleep 0.1
done
[ -e "${jobs}/${sha}.running" ] || [ -e "${jobs}/${sha}.verdict" ] && exit 0
kill -0 "${pid}" 2> /dev/null && { echo "requeue: ${short} still has no .running after 10 s; pid ${pid} is alive, see ${log}"; exit 0; }
refuse "fast.sh --once ${short} exited without serving it; its last lines in ${log}: $(tail -3 "${log}" | tr '\n' ' ')"
