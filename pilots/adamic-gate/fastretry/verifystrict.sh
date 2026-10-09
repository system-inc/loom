#!/bin/bash
# verifystrict.sh [<verify.sh>]: verify.sh's place against stubs (#098rcha step d), one scenario per case, PASS or FAIL
# each, and parity.py on planted test lines. Off, a job runs on its pool as before; LOOM_VERIFY_STRICT=1 converts it
# (adamic-gate test-jobs, LOOM_AGAIN dropped first) and runs the test jobs on codex-strict with the rest on the pool; a
# conversion that fails is placed as before, and says so. The mutant (the strict pool placed as a plain one, so a
# strict worker would meet argv) must fail:
#
#	pilots/adamic-gate/fastretry/verifystrict.sh
#	sed 's/--strict-pool "codex-strict=/--pool "codex-strict=/' verify.sh > m.sh && fastretry/verifystrict.sh m.sh   # fails 1
#	sed 's/echo 1 > "${work}\/reds.exit"$/: /' verify.sh > m.sh && fastretry/verifystrict.sh m.sh   # fails 2: a mismatch that leaves the verdict
set -u
here=$(cd "$(dirname "$0")" && pwd) failures=0
verify=${1:-${here}/../verify.sh}
stubs=$(mktemp -d)
awk '/^place\(\) \{/,/^}/' "${verify}" > "${stubs}/place.sh"
check() { if eval "$2"; then echo "PASS $1"; else echo "FAIL $1: loom $(cat "${T}/loom" 2> /dev/null), out $(cat "${T}/out" 2> /dev/null)"; failures=$((failures + 1)); fi; }
scenario() { # scenario <converter: ok or fails>
	T=$(mktemp -d)
	mkdir -p "${T}/bin"
	printf '#!/bin/bash\nprintf "%%s " "$@" > %s/loom\n' "${T}" > "${T}/bin/loom" && chmod +x "${T}/bin/loom"
	if [ "$1" = ok ]; then
		printf '#!/bin/bash\n[ "$1" = test-jobs ] && cp "$3" %s/converted-from && cat "$3" && echo "1 of 2 units are test jobs" >&2\n' "${T}" > "${T}/bin/planner"
	else
		printf '#!/bin/bash\necho "a job it cannot read" >&2\nexit 2\n' > "${T}/bin/planner"
	fi
	chmod +x "${T}/bin/planner"
	echo '{"name": "j", "units": [{"id": "tests-0", "argv": ["bash"], "timeoutSeconds": 90, "environment": {"LOOM_AGAIN": "1"}}]}' > "${T}/job.json"
	loom=${T}/bin/loom planner=${T}/bin/planner pool=codex-side LOOM_PRIORITY=40
}
source "${stubs}/place.sh"

scenario ok; LOOM_VERIFY_STRICT= place "${T}/job.json" "${T}/record.jsonl" 5 > "${T}/out" 2>&1
check off-runs-as-before '[ "$(cat "${T}/loom")" = "run --uncached --slots none --pool codex-side=5 --priority 40 --record ${T}/record.jsonl ${T}/job.json " ] && [ ! -e "${T}/converted-from" ]'
scenario ok; LOOM_VERIFY_STRICT=1 place "${T}/job.json" "${T}/record.jsonl" 5 > "${T}/out" 2>&1
check strict-places-test-jobs-on-codex-strict '[ "$(cat "${T}/loom")" = "run --uncached --slots none --strict-pool codex-strict=5 --pool codex-side=1 --priority 40 --record ${T}/record.jsonl ${T}/job.strict.json " ] && grep -q "^strict: 1 of 2 units are test jobs on codex-strict$" "${T}/out"'
check strict-drops-the-round-marker-only '! grep -q LOOM_AGAIN "${T}/converted-from" && grep -q "\"argv\": \[\"bash\"\]" "${T}/converted-from"'
scenario fails; LOOM_VERIFY_STRICT=1 place "${T}/job.json" "${T}/record.jsonl" 5 > "${T}/out" 2>&1
check a-failed-conversion-runs-as-before '[ "$(cat "${T}/loom")" = "run --uncached --slots none --pool codex-side=5 --priority 40 --record ${T}/record.jsonl ${T}/job.json " ] && grep -q "^strict: converting job.json failed (a job it cannot read); placed as argv$" "${T}/out"'

# parity.py: the same outcomes agree; a different outcome or a test only one run holds differs.
P=$(mktemp -d)
printf '%s\n' '{"Package":"p","Test":"TestA","Action":"run"}' '{"Package":"p","Test":"TestA","Action":"pass"}' '{"Package":"p","Test":"TestB","Action":"skip"}' > "${P}/strict.jsonl"
cp "${P}/strict.jsonl" "${P}/race.jsonl"
python3 "${here}/../parity.py" "${P}/strict.jsonl" "${P}/race.jsonl" > "${P}/same.txt"; same=$?
printf '%s\n' '{"Package":"p","Test":"TestA","Action":"fail"}' '{"Package":"p","Test":"TestC","Action":"pass"}' > "${P}/race.jsonl"
python3 "${here}/../parity.py" "${P}/strict.jsonl" "${P}/race.jsonl" > "${P}/differ.txt"; differ=$?
check parity-agrees '[ "${same}" = 0 ] && [ "$(head -1 "${P}/same.txt")" = "parity: identical, 2 tests the same, 0 differ (strict 2, race 2)" ]'
check parity-names-each-difference '[ "${differ}" = 1 ] && grep -qx "p TestA: strict pass, race fail" "${P}/differ.txt" && grep -qx "p TestB: strict skip, race absent" "${P}/differ.txt" && grep -qx "p TestC: strict absent, race pass" "${P}/differ.txt"'
# judgeRace: a race that agrees leaves the verdict; one that differs, or left no lines, turns it red with FAIL lines.
awk '/^judgeRace\(\) \{/,/^}/' "${verify}" > "${stubs}/judge.sh"
source "${stubs}/judge.sh"
race() { # race <race lines file or "none">
	work=$(mktemp -d) sha=1111111111111111111111111111111111111111 bin=${here}/..
	cp "${P}/strict.jsonl" "${work}/test.jsonl" && echo "green: 2 tests" > "${work}/reds.txt" && echo 0 > "${work}/reds.exit"
	printf '#!/bin/bash\n[ "%s" = none ] || cp "%s" "%s/race-test.jsonl"\n' "$1" "$1" "${work}" > "${work}/planner" && chmod +x "${work}/planner"
	planner=${work}/planner
	judgeRace > /dev/null
}
cp "${P}/strict.jsonl" "${P}/agree.jsonl"
race "${P}/agree.jsonl"
check race-that-agrees-keeps-the-verdict '[ "$(cat "${work}/reds.exit")" = 0 ] && ! grep -q FAIL "${work}/reds.txt"'
race "${P}/race.jsonl"
check race-that-differs-is-red '[ "$(cat "${work}/reds.exit")" = 1 ] && grep -qx "FAIL parity: p TestA: strict pass, race fail" "${work}/reds.txt"'
race none
check race-with-no-lines-is-red '[ "$(cat "${work}/reds.exit")" = 1 ] && grep -q "^FAIL parity: parity: " "${work}/reds.txt"'
echo "failures: ${failures}"
exit $((failures > 0))
