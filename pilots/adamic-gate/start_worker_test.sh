#!/bin/bash
# start_worker_test.sh: a box worker started by start-worker.sh never writes the box gates' own tools (Oct 10: a
# worker's first prepare ran adamic's cloud/setup.sh as the box user, which writes $HOME/.adamic-tools, a symlink to
# the gates' ~/adamic-tools, and dropped their gate inputs). Runs on a Linux box (coreutils sha256sum), as any user:
#
#	bash pilots/adamic-gate/start_worker_test.sh
#
# It plants a box home whose .adamic-tools links to the gates' adamic-tools, stubs sudo, systemctl and curl, starts a
# worker, then plays setup's write ($HOME/.adamic-tools/env.sh) under the environment the worker's unit was given.
# The gates' env.sh must be untouched and the write must land in the worker's own home. A mutant start-worker.sh
# without the private HOME must break the gates' file, or this test proves nothing.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
failures=0
check() {
	local script=$1 want=$2
	local scratch home bin runner sha
	scratch=$(mktemp -d)
	home=${scratch}/home bin=${scratch}/bin
	mkdir -p "${home}/adamic-tools" "${bin}"
	ln -s adamic-tools "${home}/.adamic-tools"
	echo "export ADAMIC_TYPESCRIPT_SOURCE=/inputs/typescript" > "${home}/adamic-tools/env.sh"
	printf 'runner\n' > "${scratch}/runner"
	sha=$(sha256sum "${scratch}/runner" | cut -c1-64)
	# sudo records the systemd-run it was asked for; curl hands over the runner; systemctl says active.
	printf '#!/bin/bash\nprintf "%%s\\n" "$@" > %q\n' "${scratch}/unit-args" > "${bin}/sudo"
	printf '#!/bin/bash\nwhile [ $# -gt 0 ]; do [ "$1" = -o ] && cp %q "$2"; shift; done\n' "${scratch}/runner" > "${bin}/curl"
	printf '#!/bin/bash\necho active\n' > "${bin}/systemctl"
	chmod 755 "${bin}/sudo" "${bin}/curl" "${bin}/systemctl"
	echo "pool-token" | HOME=${home} PATH=${bin}:${PATH} bash "${script}" box-phase-9 box-phase 0-15 "${sha}" 10m --phase-jobs > /dev/null
	local unitHome
	unitHome=$(sed -n 's/^--setenv=HOME=//p' "${scratch}/unit-args")
	# setup.sh's write, as the worker's unit would make it.
	HOME=${unitHome:-${home}} bash -c 'mkdir -p "${HOME}/.adamic-tools" && echo "export PATH=/tools" > "${HOME}/.adamic-tools/env.sh"'
	if [ "$(cat "${home}/adamic-tools/env.sh")" = "export ADAMIC_TYPESCRIPT_SOURCE=/inputs/typescript" ]; then got=kept; else got=broken; fi
	if [ "${got}" != "${want}" ]; then
		echo "FAIL ${script##*/}: the gates' env.sh was ${got}, want ${want} (worker HOME ${unitHome:-unset})"
		failures=$((failures + 1))
	elif [ "${want}" = kept ] && [ "${unitHome}" != "${home}/loom-fabric/box-phase-9/home" ]; then
		echo "FAIL: the worker's HOME is ${unitHome}, not its own"
		failures=$((failures + 1))
	fi
	rm -r "${scratch}"
}
check "${here}/start-worker.sh" kept
mutant=$(mktemp)
grep -v -e '--setenv=HOME=' "${here}/start-worker.sh" | sed 's#--working-directory="${d}" #--working-directory="${d}" --setenv=HOME="${HOME}" #' > "${mutant}"
check "${mutant}" broken
rm "${mutant}"
[ "${failures}" -eq 0 ] && echo "ok start_worker_test.sh"
exit "${failures}"
