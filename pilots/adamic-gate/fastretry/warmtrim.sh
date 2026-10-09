#!/bin/bash
# warmtrim.sh [<main.go>]: the Codex opening's disk section (#5pfcv0t) against a stub df, inside a mount namespace with
# a /tmp and a home of its own, so its trims touch nothing of the machine's. On a Mac it ships itself and main.go's
# opening to LOOM_HARNESS_BOX (workshop). Each scenario plants caches whose presence the stub df counts against /tmp:
#	5116fd40e771: /tmp has 7.7 GB, root 3.0 GB, no tree. The clone check reads /tmp, the tree's disk, and passes.
#	room-by-clearing: /tmp has 3.0 GB with a 1.0 GB npm cache and 2.0 GB of tools checkouts: both are cleared, each
#	saying what it freed, and the tree fits.
#	nothing-left: /tmp has 2.0 GB and nothing to clear: the unit refuses, Loom's fault, exit 2.
# Mutants, each failing: the clone check reading the smaller of root and /tmp again (fails 1), no clearing (fails 1):
#
#	pilots/adamic-gate/fastretry/warmtrim.sh
#	sed 's|treeFree() { df -Pm "$(dirname "${tree}")" .*|treeFree() { freeMegabytes; }|' main.go > m.go && fastretry/warmtrim.sh m.go
#	sed 's|  \[ ! -d "${tree}/.git" \] \&\& \[ "$(treeFree)" -lt "${treeNeed}" \] \|\| break|  break|' main.go > m.go && fastretry/warmtrim.sh m.go
set -u
here=$(cd "$(dirname "$0")" && pwd)
source=${1:-${here}/../main.go}
if [ "$(uname)" != Linux ]; then
	box=${LOOM_HARNESS_BOX:-workshop}
	section=$(awk '/^const codexOpening = `/,/^`$/' "${source}" | awk '/^freeMegabytes\(\) \{/,/too little to clone a tree after clearing/')
	{ cat "$0"; echo "#---section---"; echo "${section}"; } | ssh -o BatchMode=yes "${box}" 'd=$(mktemp -d) && cd "$d" && cat > all &&
		sed "/^#---section---$/,\$d" all > warmtrim.sh && sed "1,/^#---section---$/d" all > section.sh && LOOM_WARMTRIM_SECTION=$d/section.sh bash warmtrim.sh; code=$?; rm -f all warmtrim.sh section.sh; rmdir "$d"; exit $code'
	exit $?
fi
section=${LOOM_WARMTRIM_SECTION:-}
[ -n "${section}" ] || { section=$(mktemp); awk '/^const codexOpening = `/,/^`$/' "${source}" | awk '/^freeMegabytes\(\) \{/,/too little to clone a tree after clearing/' > "${section}"; }
failures=0
# scenario <name> <tmp base MB> <root MB> <npm MB> <tools MB>: the section in a private /tmp; prints its output and exit.
scenario() {
	local T
	# Outside /tmp, which the namespace replaces: the stubs, the section and the home must stay visible inside it.
	T=$(mktemp -d "${HOME}/.warmtrim-XXXXXX")
	mkdir -p "${T}/tmp" "${T}/home" "${T}/bin"
	cp "${section}" "${T}/section.sh"
	cat > "${T}/bin/df" <<STUB
#!/bin/bash
# df -Pm <paths...>: /tmp's room is its base less each planted cache still there; everything else is the root.
echo "Filesystem 1048576-blocks Used Available Capacity Mounted"
for path in "\${@:2}"; do
	if [ "\${path}" = /tmp ]; then
		free=$2
		[ -e /tmp/adamic-npm ] && free=\$((free - $4))
		[ -e /tmp/adamic-gate-tools ] && free=\$((free - $5))
		echo "tmpfs 8800 0 \${free} 0% /tmp"
	else
		echo "overlay 32000 0 $3 0% /"
	fi
done
STUB
	chmod +x "${T}/bin/df"
	unshare -Urm bash -c 'mount --bind "$1/tmp" /tmp &&
		{ [ "$3" = 0 ] || mkdir -p /tmp/adamic-npm/x; } && { [ "$4" = 0 ] || mkdir -p /tmp/adamic-gate-tools/x; } &&
		cd /tmp && HOME=$1/home PATH=$1/bin:$PATH tree=/tmp/adamic bash -c "source $2; echo reached-the-checkout"' \
		warmtrim "${T}" "${T}/section.sh" "$4" "$5" > "${T}/out" 2>&1
	echo "exit $?" >> "${T}/out"
	OUT=$(cat "${T}/out")
	rm -rf "${T}"
}
# A case passes only if the section itself ran: it defines treeFree, which the last line it reaches always calls.
check() { if ! grep -q "No such file" <<< "${OUT}" && eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; echo "${OUT}" | sed 's/^/    /'; failures=$((failures + 1)); fi; }
scenario 5116fd40e771 7700 3000 0 0
check 5116fd40e771 'grep -q "^reached-the-checkout$" <<< "${OUT}"'
scenario room-by-clearing 6000 20000 1000 2000
check room-by-clearing 'grep -q "^loom-pilot: cleared the npm cache to make room for the tree: 1000 MB freed, now 4000 MB free on /tmp$" <<< "${OUT}" && grep -q "^loom-pilot: cleared every tools checkout to make room for the tree: 2000 MB freed, now 6000 MB free on /tmp$" <<< "${OUT}" && grep -q "^reached-the-checkout$" <<< "${OUT}"'
scenario nothing-left 2000 20000 0 0
check nothing-left 'grep -q "too little to clone a tree after clearing what it could: Loom.s fault" <<< "${OUT}" && grep -q "^exit 2$" <<< "${OUT}"'
echo "failures: ${failures}"
exit $((failures > 0))
