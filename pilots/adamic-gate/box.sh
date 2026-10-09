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
set -euo pipefail
workers=$1 first=$2 until=$3 pool=$4
runnerSha=${LOOM_RUNNER_SHA:-7f01c04925b5bcdf4c2359abcee90f0723b656867e696313db20391d08cdada7}
units=${HOME}/loom-units
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
	# The private /tmp is bound in a namespace where the worker is root, then a nested namespace maps it back to its
	# own uid before the runner starts.
	setsid nohup nice -n 19 ionice -c 3 taskset -c "${cores}" unshare -Urm sh -c '
		mount --bind "$1/tmp" /tmp && mkdir -p /tmp/warm && cd /tmp/warm &&
		unshare -U --map-user="$2" --map-group="$3" env HOME=/tmp/home TMPDIR=/tmp PATH="${10}/bin:${PATH}" bash "$9" > "$1/before.log" 2>&1 &&
		cd / && exec unshare -U --map-user="$2" --map-group="$3" env HOME=/tmp/home TMPDIR=/tmp PATH="${10}/bin:${PATH}" \
			"$4" serve --pool "$5" --token "$(cat "$6")" --worker "$7" --until "$8" --workspace /tmp/loom-units --log "$1/serve.log"
	' box "${worker}" "$(id -u)" "$(id -g)" "${runner}" "${pool}" "${units}/pool-token" "${name}" "${until}" "${units}/before.sh" "${units}/node" \
		>> "${worker}/serve.out" 2>&1 < /dev/null &
	echo "box: ${name} serving on cores ${cores} until ${until}"
done
