#!/bin/bash
# box.sh: Loom's runner serving a pool from one of our own boxes, on the cores its box gate leaves idle (@system_adamic,
# Oct 9 07:45Z, on Kirk's ask: "we should be using all of their cores"). Each worker is one `loom-runner serve`, the same
# runner the Codex instances run, and the box gate always comes first:
#
#	nice 19 and the idle I/O class, so the box gate's processes take every cycle and every disk turn they want;
#	taskset to 4 cores of its own, so a unit sizes itself as on a 4-CPU Codex instance (nproc and Go both honour it);
#	a mount namespace of its own with its own directory bound over /tmp, and HOME inside it. The Codex opening assumes
#	the machine is its alone: it clears /tmp/adamic-gate (the box gate's own TMPDIR on server: 5,503 entries, Oct 9),
#	/tmp/go-build*, /tmp/Test* and other units' directories, and keeps one tree at /tmp/adamic. Here it sees only its
#	own. The nested user namespace maps the worker back to its own uid, so tests never run as root.
#
#	ssh <box> 'bash -s -- <workers> <first core> <until> <pool url>' < box.sh   # the pool token on the box, see below
#
# The pool token is read from ~/loom-units/pool-token (mode 600), minted on the Mac by `loom-pregate pool prompt` and
# copied there; it reaches the pool's next and nothing else, and expires. The runner is the pools' own, by hash.
#
# Each worker readies its tree before it asks for a unit: ~/loom-units/before.sh (`adamic-gate warm --script --sha <main>`,
# the Codex pool's before script) runs in its namespace first, with no budget over it, so a cold clone and setup (7 to 14
# minutes) never land inside a unit's 90 s (Oct 9 07:48Z: server's first unit was killed at 90 s still cloning, and the
# next one tripped on the half-made tree). A worker whose warm-up fails never serves; its before.log says why.
# LOOM_BOX_WARM_ONLY=1 readies each worker's tree and stops there, serving nothing (#x2bmxpk: the workers stay off until
# the A/B passes, but its "on" passes shouldn't spend their time cloning).
set -euo pipefail
workers=$1 first=$2 until=$3 pool=$4
runnerSha=${LOOM_RUNNER_SHA:-7f01c04925b5bcdf4c2359abcee90f0723b656867e696313db20391d08cdada7}
# LOOM_BOX_UNITS points a test (box_test.sh) at a scratch directory in place of ~/loom-units.
units=${LOOM_BOX_UNITS:-${HOME}/loom-units}
runner=${units}/loom-runner-${runnerSha:0:12}
if [ ! -x "${runner}" ]; then
	curl -fsS -o "${runner}.partial" "https://adamic-store.kirkouimet.com/blobs/${runnerSha}"
	echo "${runnerSha}  ${runner}.partial" | sha256sum -c --quiet
	chmod 755 "${runner}.partial"
	mv "${runner}.partial" "${runner}"
fi
[ -s "${units}/pool-token" ] || { echo "box: no pool token at ${units}/pool-token"; exit 2; }
[ -s "${units}/before.sh" ] || { echo "box: no warm-up script at ${units}/before.sh"; exit 2; }
# A Codex image ships Node with npm, which the opening's npm ci needs; a box's toolchain has node alone (Oct 9 08:0xZ:
# server's first warm-up stopped at "npm: command not found"). So the workers get nodejs.org's build of the toolchain's
# own version, checksummed, at ~/loom-units/node.
if [ ! -x "${units}/node/bin/npm" ]; then
	version=$("${HOME}/adamic-tools/bin/node" --version)
	archive=node-${version}-linux-x64.tar.xz
	(cd "${units}" && curl -fsS -O "https://nodejs.org/dist/${version}/${archive}" &&
		curl -fsS "https://nodejs.org/dist/${version}/SHASUMS256.txt" | grep " ${archive}\$" | sha256sum -c --quiet &&
		tar -xJf "${archive}" && rm "${archive}" && ln -sfn "node-${version}-linux-x64" node)
fi
for ((index = 0; index < workers; index++)); do
	name=$(hostname)-$((first / 4 + index))
	cores=$((first + 4 * index))-$((first + 4 * index + 3))
	worker=${units}/${name}
	if pgrep -f -- "--worker ${name} " > /dev/null; then
		echo "box: ${name} is already serving"
		continue
	fi
	mkdir -p "${worker}/tmp"
	# Each worker's own copy of the pool token, which its serve reads and removes, so the token is never on a command line.
	(umask 077 && cp "${units}/pool-token" "${worker}/pool-token")
	# The private /tmp is bound in a namespace where the worker is root, then a nested namespace maps it back to its
	# own uid before the runner starts.
	#
	# A unit sees nothing of the box user's own (#mtxysfp, Oct 9 21:30Z): the mapped user is the box user, so with only
	# /tmp bound a unit could read ~/.ssh, ~/.adamic-build-cache-token (it writes and deletes the build-cache Worker) and
	# ~/.loom, and on WSL the Windows drives (/mnt/c and every other drive letter). So what the worker needs is bound
	# into /tmp/.box first (the
	# runner, the warm-up script and node read-only, the worker's own directory for its logs and its own copy of the pool
	# token, which serve reads and removes), then an
	# empty tmpfs goes over the home and over each drive, and everything after reads from /tmp/.box. Only the drives:
	# /etc/resolv.conf is a link into /mnt/wsl on every one of our WSL boxes, and hiding all of /mnt broke name
	# resolution for every worker started on aa8cb73 (Oct 9 21:45Z: "Could not resolve host: github.com").
	setsid nohup nice -n 19 ionice -c 3 taskset -c "${cores}" unshare -Urm sh -c '
		mount --bind "$1/tmp" /tmp && mkdir -p /tmp/.box/node /tmp/.box/worker /tmp/warm &&
		for file in runner before.sh; do : > "/tmp/.box/${file}"; done &&
		mount --bind "$4" /tmp/.box/runner && mount --bind "$9" /tmp/.box/before.sh &&
		mount --bind "${10}" /tmp/.box/node && mount --bind "$1" /tmp/.box/worker &&
		for path in runner before.sh node; do mount -o remount,bind,ro "/tmp/.box/${path}" || exit 2; done &&
		mount -t tmpfs -o size=1m,mode=755 none "${12}" &&
		for drive in /mnt/?; do [ ! -d "${drive}" ] || mount -t tmpfs -o size=1m,mode=755 none "${drive}" || exit 2; done &&
		cd /tmp/warm &&
		unshare -U --map-user="$2" --map-group="$3" env HOME=/tmp/home TMPDIR=/tmp PATH="/tmp/.box/node/bin:${PATH}" bash /tmp/.box/before.sh > /tmp/.box/worker/before.log 2>&1 &&
		{ [ -z "${11}" ] || exit 0; } && cd / && exec unshare -U --map-user="$2" --map-group="$3" env HOME=/tmp/home TMPDIR=/tmp PATH="/tmp/.box/node/bin:${PATH}" \
			/tmp/.box/runner serve --pool "$5" --token-file /tmp/.box/worker/pool-token --worker "$7" --until "$8" --workspace /tmp/loom-units --log /tmp/.box/worker/serve.log
	' box "${worker}" "$(id -u)" "$(id -g)" "${runner}" "${pool}" "${units}/pool-token" "${name}" "${until}" "${units}/before.sh" "${units}/node" "${LOOM_BOX_WARM_ONLY:-}" "${HOME}" \
		>> "${worker}/serve.out" 2>&1 < /dev/null &
	echo "box: ${name} serving on cores ${cores} until ${until}"
done
