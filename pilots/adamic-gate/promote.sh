#!/bin/bash
# promote.sh: a staged set of pool tools goes live only with a canary pass for exactly that set (#3spq9j3, under #66qvxdd;
# @system_adamic, Oct 9 10:21Z: every gate fault tonight came from tools that went live untested).
#
#	pilots/adamic-gate/promote.sh [<stage directory>]     # default ~/.loom/stage; a copy in ~/.loom/bin
#
# The stage's content hash (canary.sh's: every regular file by name and sha256) must have a pass in
# ~/.loom/canary/pass/<hash>; without one this refuses and changes nothing. With one, it installs the snapshot the
# canary ran (~/.loom/canary/tools-<hash>), never the stage as it stands now, each file by fresh inode (a running bash
# reads its script as it goes, and an overwrite in place would change the lines under it). Then it restarts the fast
# server and confirms the code running is the code installed: every fast.sh server process started after the install
# (Oct 9: ff519d3 was copied in at 10:04Z, but the server had read fast.sh at 09:50Z and served the old unpack for
# 24 minutes). Jobs a dying server left marked running are freed, as the server would (its own check races the old
# processes' exit; at 10:28Z seven jobs sat unserved that way).
set -uo pipefail
stage=$(cd "${1:-${HOME}/.loom/stage}" && pwd) || { echo "promote: no stage directory ${1:-${HOME}/.loom/stage}"; exit 2; }
bin=${LOOM_LIVE_BIN:-${HOME}/.loom/bin}
canary=${HOME}/.loom/canary
jobs=${HOME}/.loom/jobs/fast
hash=$(python3 - "${stage}" <<'PY'
import hashlib, os, sys
root = sys.argv[1]
lines = []
for name in sorted(os.listdir(root)):
    path = os.path.join(root, name)
    if name.startswith(".") or name == "__pycache__" or name.endswith(".pyc") or not os.path.isfile(path):
        continue
    lines.append("%s %s\n" % (hashlib.sha256(open(path, "rb").read()).hexdigest(), name))
print(hashlib.sha256("".join(lines).encode()).hexdigest())
PY
)
pass=${canary}/pass/${hash}
snapshot=${canary}/tools-${hash}
if [ ! -s "${pass}" ]; then
	echo "promote: refused, no canary pass for stage ${hash:0:12} (run canary.sh ${stage} first; passes held: $(ls "${canary}/pass" 2> /dev/null | cut -c1-12 | tr '\n' ' '))"
	exit 1
fi
[ -d "${snapshot}" ] || { echo "promote: refused, the pass for ${hash:0:12} has no snapshot at ${snapshot}"; exit 1; }
echo "promote: stage ${hash:0:12}, canary pass $(python3 -c 'import json, sys; r = json.load(open(sys.argv[1])); print(r["verdict"][:120])' "${pass}")"
installed=$(date +%s)
for file in "${snapshot}"/*; do
	[ -f "${file}" ] || continue
	name=$(basename "${file}")
	cmp -s "${file}" "${bin}/${name}" && continue
	cp -p "${file}" "${bin}/${name}.promote-$$" && mv "${bin}/${name}.promote-$$" "${bin}/${name}" && echo "promote: ${name} installed"
done
echo "${hash} $(date -u +%Y-%m-%dT%H:%M:%SZ)" > "${bin}/.promoted"
# The server: unloaded (KeepAlive would start a new one beside the dying), waited out, then loaded again, so its own
# startup check never sees the old processes. A --once job runs in its own process group and is left alone.
plist=${HOME}/Library/LaunchAgents/com.loom.fast.plist
launchctl bootout "gui/$(id -u)/com.loom.fast" 2> /dev/null
for ((second = 0; second < 60; second++)); do
	pgrep -f "[.]loom/bin/fast.sh$" > /dev/null || break
	[ "${second}" = 30 ] && pkill -TERM -f "[.]loom/bin/fast.sh$"
	sleep 1
done
pgrep -f "[.]loom/bin/fast.sh$" > /dev/null && { echo "promote: the old fast server is still running after 60 s; not starting a second (load it by hand: launchctl bootstrap gui/$(id -u) ${plist})"; exit 1; }
for marker in "${jobs}"/*.running; do
	[ -e "${marker}" ] || continue
	pgrep -f "${jobs}/$(basename "${marker}" .running)\.work/" > /dev/null || mv "${marker}" "${marker}.promoted-$(date -u +%H%M)"
done
launchctl bootstrap "gui/$(id -u)" "${plist}" || { echo "promote: launchctl couldn't load ${plist}"; exit 1; }
sleep 3
stale=0
for pid in $(pgrep -f "[.]loom/bin/fast.sh$"); do
	started=$(date -j -f "%a %b %d %T %Y" "$(ps -o lstart= -p "${pid}")" +%s 2> /dev/null || echo 0)
	[ "${started}" -ge "${installed}" ] || stale=$((stale + 1))
done
[ -n "$(pgrep -f "[.]loom/bin/fast.sh$")" ] || { echo "promote: the fast server didn't start"; exit 1; }
[ "${stale}" = 0 ] || { echo "promote: ${stale} fast server processes predate the install; the old code is still serving"; exit 1; }
echo "promote: live, the fast server restarted on stage ${hash:0:12}"
