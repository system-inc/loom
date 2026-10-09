#!/bin/bash
# box_test.sh: a pool unit on a box sees nothing of the box user's own (#mtxysfp). Runs on a Linux box with user
# namespaces (any of the WSL gate boxes), as the box user:
#
#	bash pilots/adamic-gate/box_test.sh
#
# It plants a home with an ssh key and a build-cache token, points box.sh at a scratch units directory
# (LOOM_BOX_UNITS) whose warm-up script is a probe, and runs one worker warm-only (LOOM_BOX_WARM_ONLY), so nothing
# serves and no pool is asked. The probe must find the planted secrets and the shared pool token hidden and a tmpfs
# over /mnt/c, while the worker's own copy of the token (serve's to read and remove), its own directory and node stay
# readable and names still resolve (/etc/resolv.conf lives under /mnt/wsl on WSL). Then a mutant box.sh without the
# tmpfs mounts must let the probe read the secrets and /mnt/c, or this test proves nothing.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
# Not under /tmp: the worker binds its own /tmp over it, so a scratch there would vanish inside the namespace.
scratch=$(mktemp -d "${HOME}/box-test.XXXXXX")
home=${scratch}/home
# Inside the planted home, as ~/loom-units is inside the box user's: so the shared pool token is hidden with it.
units=${home}/loom-units
runnerSha=0000000000000000000000000000000000000000000000000000000000000000
failures=0

mkdir -p "${home}/.ssh" "${units}/node/bin"
echo "planted-key" > "${home}/.ssh/id_test"
echo "planted-token" > "${home}/.adamic-build-cache-token"
chmod 600 "${home}/.ssh/id_test" "${home}/.adamic-build-cache-token"
echo "pool-token-value" > "${units}/pool-token"
printf '#!/bin/sh\nexit 0\n' > "${units}/loom-runner-${runnerSha:0:12}"
printf '#!/bin/sh\nexit 0\n' > "${units}/node/bin/npm"
chmod 755 "${units}/loom-runner-${runnerSha:0:12}" "${units}/node/bin/npm"
cat > "${units}/before.sh" <<PROBE
for path in "${home}/.ssh/id_test" "${home}/.adamic-build-cache-token"; do
	if cat "\${path}" > /dev/null 2>&1; then echo "read \${path}"; else echo "hidden \${path}"; fi
done
# A drive counts as hidden when a tmpfs sits on it in the worker's own mount table (or the box has no such drive):
# its contents can't prove it, since a box's /mnt/c may be empty to begin with.
if [ ! -d /mnt/c ] || grep -qE "^([^ ]+ ){4}/mnt/c .* - tmpfs " /proc/self/mountinfo; then echo "hidden /mnt/c"; else echo "read /mnt/c"; fi
getent hosts github.com > /dev/null 2>&1 && echo "resolves github.com"
# The worker's own copy waits in its directory for serve, which reads and removes it; the shared one is in the hidden home.
[ -s /tmp/.box/worker/pool-token ] && echo "kept pool-token"
if cat "${units}/pool-token" > /dev/null 2>&1; then echo "read the shared pool-token"; else echo "hidden the shared pool-token"; fi
[ -x /tmp/.box/node/bin/npm ] && echo "kept node"
touch /tmp/.box/worker/probe-wrote && echo "kept worker"
PROBE

# probe <box.sh> <worker name>: one warm-only worker on core 0, its before.log once it ends.
probe() {
	local script=$1 name log
	name=$(hostname)-0
	log=${units}/${name}/before.log
	rm -f "${log}"
	HOME=${home} LOOM_BOX_UNITS=${units} LOOM_RUNNER_SHA=${runnerSha} LOOM_BOX_WARM_ONLY=1 \
		bash "${script}" 1 0 1m http://127.0.0.1:9/pools/test > /dev/null 2>&1
	for _ in $(seq 1 50); do
		pgrep -f -- "${units}/${name}" > /dev/null || break
		sleep 0.2
	done
	cat "${log}" 2> /dev/null
}

check() {
	local output=$1 line=$2
	if ! grep -qx "${line}" <<< "${output}"; then
		echo "FAIL: expected '${line}' in:"; sed 's/^/  /' <<< "${output}"
		failures=$((failures + 1))
	fi
}

output=$(probe "${here}/box.sh")
check "${output}" "hidden ${home}/.ssh/id_test"
check "${output}" "hidden ${home}/.adamic-build-cache-token"
check "${output}" "hidden /mnt/c"
check "${output}" "resolves github.com"
check "${output}" "kept pool-token"
check "${output}" "kept node"
check "${output}" "kept worker"
check "${output}" "hidden the shared pool-token"

# The mutant: box.sh without the tmpfs over the home. The probe must read the planted secrets through it.
mutant=${scratch}/box-mutant.sh
grep -v 'mount -t tmpfs' "${here}/box.sh" > "${mutant}"
output=$(probe "${mutant}")
check "${output}" "read ${home}/.ssh/id_test"
check "${output}" "read ${home}/.adamic-build-cache-token"
check "${output}" "read /mnt/c"

if [ "${failures}" -eq 0 ]; then
	echo "box_test: ok (scratch ${scratch})"
else
	echo "box_test: ${failures} failed (scratch ${scratch})"
	exit 1
fi
