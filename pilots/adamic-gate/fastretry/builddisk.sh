#!/bin/bash
# builddisk.sh [<verify.sh>]: verify.sh's build-vet body against a stub go. A build that fails on a full disk exits 2
# (Loom's, placed again), a real build failure exits 1 (the tree's red), a clean one 0. The mutant without the disk
# check must fail:
#
#	pilots/adamic-gate/fastretry/builddisk.sh
#	sed '/grep -qs "no space left on device" "${out}\/build.log"/,/^fi$/d' verify.sh > m.sh && fastretry/builddisk.sh m.sh   # fails 1
set -u
here=$(cd "$(dirname "$0")" && pwd) failures=0
verify=${1:-${here}/../verify.sh}
T=$(mktemp -d)
python3 - "${verify}" > "${T}/body.sh" <<'PY'
import re, sys
text = open(sys.argv[1]).read()
print(re.search(r"body = '''(.*?)'''", text, re.S).group(1))
PY
grep -q "loom-build" "${T}/body.sh" || { echo "FAIL: no build body in ${verify}"; exit 1; }
check() { if [ "$2" = "$3" ]; then echo "PASS $1"; else echo "FAIL $1: exit $2, want $3"; failures=$((failures + 1)); fi; }
run() { # run <go build's output> <go build's exit>
	local tree=${T}/tree out=${T}/out
	mkdir -p "${tree}/.git" "${out}" "${T}/bin"
	printf '#!/bin/bash\n[ "$1" = build ] && { printf "%%s\\n" %q; exit %s; }\nexit 0\n' "$1" "$2" > "${T}/bin/go"
	printf '#!/bin/bash\necho stub\n' > "${T}/bin/git"
	chmod +x "${T}/bin/go" "${T}/bin/git"
	PATH="${T}/bin:${PATH}" tree=${tree} out=${out} started=${SECONDS} bash "${T}/body.sh" > "${T}/out.log" 2>&1
	echo $?
}
check full-disk-is-looms "$(run 'link: mapping output file failed: no space left on device' 1)" 2
check real-red-stays-red "$(run './x.go:1: undefined: y' 1)" 1
check clean-build-passes "$(run '' 0)" 0
echo "failures: ${failures}"
exit $((failures > 0))
