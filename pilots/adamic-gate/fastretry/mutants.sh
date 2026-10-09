#!/bin/bash
# mutants.sh [<mutantjudge.py>] [<promote.sh>]: the gate-mutant suite's judge and promote.sh's hold on it (#fyvmsy8),
# one scenario per case, PASS or FAIL each. The judge reads a pool run's verdict, reds and test record as the box
# watcher's judgeMutant reads a box log: a declared red is ok, a green or a red at another step is wrong, no verdict or a
# void is void, and the thin canary input is ok only as a green under the floor. promote.sh refuses a pass that carries
# no suite or a mutant not read ok, and installs one whose every mutant read ok. Nothing reaches ~/.loom: HOME is a
# temporary directory and the restart is off. The mutants (the judge passing a green, promote.sh ignoring the suite)
# must fail:
#
#	pilots/adamic-gate/fastretry/mutants.sh
#	sed 's/^if not verdict.startswith("red: " + sha + " "):/if False:/' mutantjudge.py > m.py && fastretry/mutants.sh m.py   # fails 1
#	sed '/^\[ -z "\${held}" \]/d' promote.sh > m.sh && fastretry/mutants.sh "" m.sh   # fails 2
set -u
here=$(cd "$(dirname "$0")" && pwd) failures=0
judge=${1:-${here}/../mutantjudge.py}
[ -n "${judge}" ] || judge=${here}/../mutantjudge.py
promote=${2:-${here}/../promote.sh}
sha=1111111111111111111111111111111111111111
T=$(mktemp -d)
check() { if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; [ -s "${T}/said" ] && sed "s/^/    /" "${T}/said"; failures=$((failures + 1)); fi; }
# run <step> <pattern> <verdict line> [<FAIL line>] [<passed tests>]: one judged pool run, its answer in ${T}/said.
run() {
	local step=$1 pattern=$2 verdict=$3 fail=${4:-} passes=${5:-0} n
	rm -f "${T}/verdict" "${T}/reds.txt" "${T}/test.jsonl"
	[ -n "${verdict}" ] && echo "${verdict}" > "${T}/verdict"
	[ -n "${fail}" ] && echo "FAIL ${fail} (tests-03)" > "${T}/reds.txt"
	for ((n = 0; n < passes; n++)); do
		echo '{"Action":"pass","Package":"p","Test":"Test'"${n}"'"}' >> "${T}/test.jsonl"
	done
	python3 "${judge}" "${step}" "${pattern}" "${sha}" "${T}/verdict" "${T}/reds.txt" "${T}/test.jsonl" 500 > "${T}/said" 2>&1
}
said() { [[ $(cat "${T}/said") == $1* ]]; }

run tests 'internal/native TestWASIUnit[0-9]+' "red: ${sha} fast gate on Loom's side pool, first: github.com/system-inc/adamic/internal/native TestWASIUnit03 (run r)" "github.com/system-inc/adamic/internal/native TestWASIUnit03"
check declared-test-red-is-ok 'said ok'
run tests 'internal/native TestWASIUnit[0-9]+' "green: ${sha} fast gate on Loom's side pool (run r)"
check green-is-wrong 'said wrong'
run tests '' "green: ${sha} fast gate on Loom's side pool (run r)"
check green-with-no-pattern-is-wrong 'said wrong'
run tests 'internal/native TestWASIUnit[0-9]+' "red: ${sha} fast gate on Loom's side pool, first: github.com/system-inc/adamic/internal/ir TestOther (run r)" "github.com/system-inc/adamic/internal/ir TestOther"
check red-on-another-test-is-wrong 'said wrong'
run tests '' "red: ${sha} fast gate on Loom's side pool, first failure at go build or vet (run r)"
check red-at-build-is-wrong-for-tests 'said wrong'
run census '' "red: ${sha} fast gate, first failure at census after 72.0 s (build=39.4s)"
check declared-census-red-is-ok 'said ok'
run census '' "red: ${sha} fast gate on Loom's side pool, the gate's selection refused the change: x"
check refused-selection-is-wrong 'said wrong'
run tests '' ""
check no-verdict-is-void 'said void'
run tests '' "void: ${sha} fast gate on Loom's side pool stopped at its 30-minute ceiling"
check void-is-void 'said void'
run thin '' "green: ${sha} fast gate on Loom's side pool (run r)" "" 12
check thin-green-under-floor-is-ok 'said ok'
run thin '' "green: ${sha} fast gate on Loom's side pool (run r)" "" 501
check thin-green-over-floor-is-wrong 'said wrong'

# promote.sh against a stage of one file, a pass for its hash and the canary's snapshot, all under a HOME of its own.
stage=${T}/stage bin=${T}/bin
mkdir -p "${stage}" "${bin}" "${T}/.loom/canary/pass"
echo 'echo one' > "${stage}/tool.sh"
hash=$(python3 - "${stage}" <<'PY'
import hashlib, os, sys
root = sys.argv[1]
lines = ["%s %s\n" % (hashlib.sha256(open(os.path.join(root, n), "rb").read()).hexdigest(), n) for n in sorted(os.listdir(root))]
print(hashlib.sha256("".join(lines).encode()).hexdigest())
PY
)
mkdir -p "${T}/.loom/canary/tools-${hash}" && cp "${stage}/tool.sh" "${T}/.loom/canary/tools-${hash}/"
promoteWith() { # promoteWith <mutants JSON>: a pass carrying those mutants, then promote.sh, its output in ${T}/said
	echo '{"verdict": "green: x", "mutants": '"$1"'}' > "${T}/.loom/canary/pass/${hash}"
	rm -f "${bin}/tool.sh"
	HOME=${T} LOOM_LIVE_BIN=${bin} LOOM_PROMOTE_RESTART=0 bash "${promote}" "${stage}" > "${T}/said" 2>&1
}
promoteWith '{}'
check promote-refuses-a-pass-without-the-suite '[ ! -e "${bin}/tool.sh" ] && grep -q "no gate-mutant suite" "${T}/said"'
promoteWith '{"wasi-family-must-run": "ok", "binary-dies-partway": "wrong green: x"}'
check promote-refuses-a-wrong-mutant '[ ! -e "${bin}/tool.sh" ] && grep -q "binary-dies-partway" "${T}/said"'
promoteWith '{"wasi-family-must-run": "ok", "binary-dies-partway": "ok"}'
check promote-installs-when-every-mutant-is-ok '[ -e "${bin}/tool.sh" ]'

rm -f "${T}/said" "${T}/verdict" "${T}/reds.txt" "${T}/test.jsonl" "${stage}/tool.sh" "${bin}/tool.sh" "${bin}/.promoted" \
	"${T}/.loom/canary/pass/${hash}" "${T}/.loom/canary/tools-${hash}/tool.sh"
rmdir "${T}/.loom/canary/tools-${hash}" "${T}/.loom/canary/pass" "${T}/.loom/canary" "${T}/.loom" "${stage}" "${bin}" "${T}"
echo "failures: ${failures}"
exit $((failures > 0))
