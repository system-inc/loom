#!/bin/bash
# selectwait.sh [<select-box.sh>]: select-box.sh's own lock against stubs (#9jadv57), one scenario per case, PASS or FAIL
# each. A box busy with another selection is waited on when no box was free, and the second selection runs there; a box
# busy past the budget hands the selection back inside it; a burst of three at once all land on the one box. The stub
# ssh runs the box's script here, under a HOME of its own, with git and run.py stubbed (python3 sleeps for the
# selection's seconds, then writes select.json). It needs flock, so on a Mac it ships itself and the script to
# LOOM_HARNESS_BOX (workshop) and runs there, touching only a mktemp directory and /tmp/loom-select/<fake sha>.
# The mutant (a busy box's exit 3 restored, so it is never waited on) must fail:
#
#	pilots/adamic-gate/fastretry/selectwait.sh
#	sed 's/is busy with another selection" >\&2; exit 4;/is busy with another selection" >\&2; exit 3;/' select-box.sh > m.sh && fastretry/selectwait.sh m.sh   # fails 2
set -u
here=$(cd "$(dirname "$0")" && pwd) failures=0
selectBox=${1:-${here}/../select-box.sh}
if ! command -v flock > /dev/null; then
	box=${LOOM_HARNESS_BOX:-workshop}
	cat "${here}/selectwait.sh" "${selectBox}" | ssh -o BatchMode=yes "${box}" 'd=$(mktemp -d) && cd "$d" && cat > all &&
		n=$(grep -n "^#!/bin/bash$" all | sed -n 2p | cut -d: -f1) && head -n $((n - 1)) all > selectwait.sh && tail -n +"$n" all > select-box.sh &&
		bash selectwait.sh "$d/select-box.sh"; code=$?; rm -f all selectwait.sh select-box.sh; rmdir "$d"; exit $code'
	exit $?
fi
stubs=$(mktemp -d)
mkdir -p "${stubs}/bin"
# ssh <options> <box> <command...>: the box is here; the command line is a remote shell's, as ssh joins it.
printf '%s\n' '#!/bin/bash' 'while [ "${1#-}" != "$1" ]; do shift 2; done; shift' 'exec bash -c "$*"' > "${stubs}/bin/ssh"
printf '%s\n' '#!/bin/bash' 'exit 0' > "${stubs}/bin/git"
printf '%s\n' '#!/bin/bash' 'while [ "$1" != --out ]; do shift; done' 'sleep "${STUB_SELECT_SECONDS:-1}"' 'echo "{\"packages\": [\"example.com/a\"]}" > "$2/select.json"' 'echo a > "$2/changed-paths.txt"' > "${stubs}/bin/python3"
chmod +x "${stubs}/bin/"*
home=${stubs}/home
mkdir -p "${home}/loom-select/adamic/.git" "${home}/loom-select/tools" "${home}/adamic-tools"
echo "export PATH=${stubs}/bin:\${PATH}" > "${home}/adamic-tools/env.sh"
tools=4444444444444444444444444444444444444444 base=3333333333333333333333333333333333333333
check() { if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; for log in "${stubs}"/*.log; do echo "  ${log##*/}:"; sed 's/^/    /' "${log}"; done; failures=$((failures + 1)); fi; }
# run <name> <gated digit> <selection seconds> [budget]: select-box.sh in the background; its exit, wall and stdout land
# beside its log.
run() {
	(
		started=${SECONDS}
		HOME=${home} PATH=${stubs}/bin:${PATH} STUB_SELECT_SECONDS=$3 LOOM_SELECT_BOXES=${box:-workshop} LOOM_SELECT_BUDGET=${4:-30} \
			bash "${selectBox}" "$(printf "$2%.0s" {1..40})" "${base}" main "${tools}" "${stubs}/$1.tgz" > "${stubs}/$1.out" 2> "${stubs}/$1.log"
		echo "$? $((SECONDS - started))" > "${stubs}/$1.exit"
	) &
}
selected() { # selected <name> <most seconds>: exited 0 on the box, its archive holding select.json, within the seconds
	read -r code wall < "${stubs}/$1.exit" && [ "${code}" = 0 ] && [ "${wall}" -le "$2" ] && [ "$(cat "${stubs}/$1.out")" = "${box:-workshop}" ] &&
		tar -tzf "${stubs}/$1.tgz" | grep -qx './select.json'
}
fresh() { rm -f "${stubs}"/*.log "${stubs}"/*.out "${stubs}"/*.exit "${stubs}"/*.tgz; }

run free 5 1; wait
check free-box-selects-at-once 'selected free 4 && ! grep -q waiting "${stubs}/free.log"'
fresh
# A 5 s selection holds the lock; the second, a second behind it, waits and runs on the box.
run holder 6 5; sleep 1; run second 7 1; wait
check busy-box-is-waited-on 'selected holder 8 && selected second 9 && grep -q "busy with another selection" "${stubs}/second.log" && grep -q "took the lock after waiting" "${stubs}/second.log"'
fresh
# A box busy past the budget hands the selection back inside it, for the pool.
run holder 6 25; sleep 1; run second 7 1 14; wait
check busy-past-budget-goes-to-pool 'read -r code wall < "${stubs}/second.exit" && [ "${code}" = 3 ] && [ "${wall}" -le 14 ] && [ ! -e "${stubs}/second.tgz" ] && selected holder 28'
fresh
# Three requeues at once (Oct 9 19:46Z): each takes its turn on the box, none falls to the pool.
run one 8 4; run two 9 4; run three a 4; wait
check burst-of-three-lands-on-the-box 'selected one 16 && selected two 16 && selected three 16'
fresh
rm -f "${stubs}/bin/"* "${home}/adamic-tools/env.sh" && rmdir "${stubs}/bin" "${home}/adamic-tools"
rm -f "${home}/loom-select/lock" && rmdir "${home}/loom-select/adamic/.git" "${home}/loom-select/adamic" "${home}/loom-select/tools" "${home}/loom-select" "${home}" "${stubs}"
echo "failures: ${failures}"
exit $((failures > 0))
