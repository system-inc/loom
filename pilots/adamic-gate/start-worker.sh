#!/bin/bash
# start-worker.sh <name> <pool> <cpus, an AllowedCPUs list> <runner sha256> <until> [--phase-jobs]: starts one of Fabric's
# box workers as a system unit (loom-<name>) that owns its cores. Its HOME is private (~/loom-fabric/<name>/home), so
# adamic's setup.sh, Go's cache and adamic's build cache are its own and never the box gates' (Oct 10: a worker's first
# prepare rewrote the box's ~/adamic-tools/env.sh through $HOME/.adamic-tools). The pool token is read from stdin.
set -euo pipefail
name=$1 pool=$2 cpus=$3 runner=$4 until=$5 phase=${6:-}
d=${HOME}/loom-fabric/${name}
mkdir -p "${d}/home" "${d}/root" "${d}/ws"
binary=${d}/loom-runner-${runner:0:12}
if [ ! -x "${binary}" ]; then
	curl -fsS -o "${binary}.partial" "https://adamic-store.kirkouimet.com/blobs/${runner}"
	echo "${runner}  ${binary}.partial" | sha256sum -c --quiet
	chmod 755 "${binary}.partial" && mv "${binary}.partial" "${binary}"
fi
# A pre-cloned tree (box-phase-next) saves the first unit a cold clone.
if [ ! -d "${d}/root/adamic" ] && [ -d "${HOME}/loom-fabric/box-phase-next/root/adamic/.git" ] && ! pgrep -f "git clone -q --recurse-submodules" > /dev/null; then
	mv "${HOME}/loom-fabric/box-phase-next/root/adamic" "${d}/root/adamic"
fi
(umask 077 && cat > "${d}/pool-token")
sudo systemd-run --uid="$(id -u)" --gid="$(id -g)" --unit="loom-${name}" -p AllowedCPUs="${cpus}" -p MemoryAccounting=yes -p KillMode=control-group \
	--working-directory="${d}" --setenv=HOME="${d}/home" --setenv=USER="$(id -un)" \
	--setenv=PATH="${HOME}/loom-fabric/node/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
	"${binary}" serve --strict ${phase} --pool "https://loom-wire.kirk-ouimet.workers.dev/pools/${pool}" --token-file "${d}/pool-token" \
	--worker "${name}" --until "${until}" --root "${d}/root" --workspace "${d}/ws" --log "${d}/serve.log"
sleep 3
systemctl is-active "loom-${name}"
