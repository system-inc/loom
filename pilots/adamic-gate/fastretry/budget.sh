#!/bin/bash
# budget.sh [<verify.sh>]: verify.sh's planning against stubs (#3sjs0rn), up to the job it hands Loom. LOOM_FAST_BUDGET
# unset or empty plans exactly as before the switch: the planner's argv (but for the products', #fysfvrx) and job.json
# byte for byte the same as the verify.sh this port started from (89fa600; BASELINE=<sha> names another after a
# deliberate change to the unbudgeted plan). Set to seconds, the planner gets --budget and --loom-times, a selection's
# families go to it as the tree's members (--only-tests), and its packed specs stay while its remainder leaves (f18497c
# to be20c4b). The mutants (the guard removed, the guard inverted, the job script's guard removed) must fail:
#
#	pilots/adamic-gate/fastretry/budget.sh
#	sed 's/^if \[ -n "\${LOOM_FAST_BUDGET:-}" \]; then$/LOOM_FAST_BUDGET=${LOOM_FAST_BUDGET:-60}; if true; then/' verify.sh > m.sh && fastretry/budget.sh m.sh   # fails 4
#	sed 's/^if \[ -n "\${LOOM_FAST_BUDGET:-}" \]; then$/if [ -z "${LOOM_FAST_BUDGET:-}" ]; then/' verify.sh > m.sh && fastretry/budget.sh m.sh               # fails 11
#	sed 's/^packed = bool(os.environ.get("LOOM_FAST_BUDGET")) and /packed = /' verify.sh > m.sh && fastretry/budget.sh m.sh                             # fails 1
set -u
here=$(cd "$(dirname "$0")" && pwd) failures=0
verify=${1:-${here}/../verify.sh}
stubs=$(mktemp -d)
# Everything before the job goes to Loom: the reference, the tree, the plan and the job script. Nothing runs on a pool.
cut() { awk '/^"\$\{loom\}" run /{exit} {print}' "$1"; }
cut "${verify}" > "${stubs}/verify.sh"
grep -q '"${planner}" plan ' "${stubs}/verify.sh" || { echo "FAIL: no plan in ${verify}"; exit 1; }
git -C "${here}" show "${BASELINE:-89fa600}:pilots/adamic-gate/verify.sh" > "${stubs}/baseline-whole.sh" 2> /dev/null || { echo "FAIL: no baseline verify.sh at ${BASELINE:-89fa600}"; exit 1; }
cut "${stubs}/baseline-whole.sh" > "${stubs}/baseline.sh"
check() { if eval "$2" 2> /dev/null; then echo "PASS $1"; else echo "FAIL $1"; sed "s/^/    /" $T/out.log 2> /dev/null; failures=$((failures + 1)); fi; }
sha=1111111111111111111111111111111111111111
opening='echo opening\nexport ADAMIC_GATE_UNCACHED=1\necho body'
# The planner's plan: pkg/a's family packed with pkg/b on tests-01, pkg/a's remainder alone on tests-02 (a unit that
# leaves when it's dropped), and tests-03 needing tests-02.
plan='{"name":"stub","units":[
 {"id":"tests-01","argv":["bash","-c","'"${opening}"'","adamic-gate-unit","'${sha}'","pkg/a=^(TestA|TestAUnit00|TestA_Setup)$","pkg/b=^(TestB)$"]},
 {"id":"tests-02","argv":["bash","-c","'"${opening}"'","adamic-gate-unit","'${sha}'","pkg/a=. skip=^(TestA|TestAUnit00|TestA_Setup)$"]},
 {"id":"tests-03","argv":["bash","-c","'"${opening}"'","adamic-gate-unit","'${sha}'","pkg/b=. skip=^(TestB)$"],"needs":["tests-02"]}]}'
scenario() { # scenario [notree]: a fake home, tools and work directory; the planner records its argv
	T=$(mktemp -d) && home=$T/home bin=$T/bin work=$T/work
	mkdir -p $home/.adamic-full-gate $home/.loom/pregate $bin $work
	echo 2222222222222222222222222222222222222222 > $home/.adamic-full-gate/last-green
	echo reference > $home/.loom/pregate/reference-2222222222222222222222222222222222222222.jsonl.gz
	echo inputs-hash > $home/.loom/gate-inputs
	printf 'pkg/a TestA\t1.0\n' > $home/.loom/loom-times.tsv
	[ "${1:-}" = notree ] || printf '%s\n' "pkg/a TestA" "pkg/a TestAUnit00" "pkg/a TestA_Setup" "pkg/a TestAx" "pkg/a TestOther" "pkg/b TestB" > $T/tree
	# verify.sh runs it with python3; with no tree it prints nothing, as treetests.py does on a sha it can't read.
	printf 'import os, sys\nos.path.exists("%s/tree") and sys.stdout.write(open("%s/tree").read())\n' $T $T > $bin/treetests.py
	printf '%s' "${plan}" > $T/plan.json
	printf '#!/bin/bash\nprintf "%%s\\n" "$@" > %s/argv\ncat %s/plan.json\n' $T $T > $bin/adamic-gate
	chmod +x $bin/adamic-gate
	echo '{"only_tests":{"pkg/a":["TestA","TestGone"]},"deferred":{"pkg/b":["TestSlow"]}}' > $T/select.json
}
run() { # run <script> [LOOM_FAST_BUDGET value]: every wait bounded at 20 s
	env -u LOOM_FAST_BUDGET ${2+LOOM_FAST_BUDGET=$2} HOME=$home LOOM_BIN=$bin LOOM_VERIFY_WORK=$work LOOM_VERIFY_SELECT=$T/select.json \
		perl -e 'alarm 20; exec @ARGV' bash "$1" $sha 'pkg/(a|b)' none 5 > $T/out.log 2>&1
}
today() { printf '%s\n' plan --target codex --remainder --gate-inputs inputs-hash --reference $home/.loom/pregate/reference-2222222222222222222222222222222222222222.jsonl.gz --sha $sha --units 12 --only 'pkg/(a|b)' --tree-tests $work/tree-tests.txt; }
# Products pack by Loom's times, budget on or off (#fysfvrx): their arguments follow today's either way.
products() { printf '%s\n' --product-budget 300 --product-times $home/.loom/loom-times.tsv; }
spec() { python3 -c 'import json, sys; job = json.load(open(sys.argv[1])); print("\n".join(" ".join([unit["id"], json.dumps(unit.get("needs"))] + unit["argv"][5:]) for unit in job["units"]))' $work/job.json; }

# A: off, the planner gets exactly today's arguments and the products'; empty is off; unset is on at 60 (budget mode
# lands on).
scenario; run $stubs/verify.sh off; check off-argv-is-today '[ "$(cat $T/argv)" = "$(today; products)" ]'
scenario; run $stubs/verify.sh ""; check empty-argv-is-today '[ "$(cat $T/argv)" = "$(today; products)" ]'
scenario; echo off > $home/.loom/fast-budget; run $stubs/verify.sh; check unset-reads-the-file-off '[ "$(cat $T/argv)" = "$(today; products)" ]'
scenario; echo 45 > $home/.loom/fast-budget; run $stubs/verify.sh; check unset-reads-the-file-45 '[ "$(grep -A1 -x -- --budget $T/argv | tail -1)" = 45 ]'
scenario; echo off > $home/.loom/fast-budget; run $stubs/verify.sh 60; check env-beats-the-file '[ "$(grep -A1 -x -- --budget $T/argv | tail -1)" = 60 ]'
scenario; run $stubs/verify.sh; check unset-is-on-at-60 'grep -qx -- --budget $T/argv && [ "$(grep -A1 -x -- --budget $T/argv | tail -1)" = 60 ]'
# A': off, the job is byte for byte the baseline's and its argv the baseline's and the products', even with a budgeted
# attempt's only-tests.json left in the work directory (a re-plan reuses it).
scenario; echo '{"pkg/a":["TestA"]}' > $work/only-tests.json; run $stubs/baseline.sh; cp $work/job.json $T/baseline-job.json; cp $T/argv $T/baseline-argv
run $stubs/verify.sh off; check off-job-is-the-baselines 'cmp -s $work/job.json $T/baseline-job.json && [ "$(cat $T/argv)" = "$(cat $T/baseline-argv; products)" ]'
# A'': off, a selection's family spec goes to the test unit with the fewest specs, never a product unit, which runs
# under a 600 s ceiling (#fysfvrx): here product-00 ties tests-01 and comes first.
scenario; printf '%s' '{"name":"stub","units":[
 {"id":"product-00","argv":["bash","-c","'"${opening}"'","adamic-gate-unit","'${sha}'","pkg/c=^(TestProduct_C)$"]},
 {"id":"tests-01","needs":["product-00"],"argv":["bash","-c","'"${opening}"'","adamic-gate-unit","'${sha}'","pkg/a=^(TestA)$","pkg/b=^(TestB)$"]}]}' > $T/plan.json
run $stubs/verify.sh off
check off-family-not-on-a-product '[ "$(spec | grep ^product-00)" = "product-00 null pkg/c=^(TestProduct_C)$" ] && spec | grep -qF "tests-01 [\"product-00\"] pkg/b=^(TestB)$ skip=^(TestSlow)$ pkg/a=^(TestA)(("'
# B: 60, the planner gets today's arguments, then the budget, Loom's times and the selection's family members.
scenario; run $stubs/verify.sh 60
check budget-argv '[ "$(cat $T/argv)" = "$(today; products; printf "%s\n" --budget 60 --unit-setup 10 --split-all --loom-times $home/.loom/loom-times.tsv --only-tests $work/only-tests.json)" ]'
check budget-family-members '[ "$(cat $work/only-tests.json)" = "{\"pkg/a\": [\"TestA\", \"TestAUnit00\", \"TestA_Setup\"]}" ]'
# C: the packed specs stay, the selected package's remainder leaves and takes tests-02 with it, tests-03 no longer
# waits on it, no family spec is added, and the stale name is still listed.
check budget-packs-exactly '[ "$(spec)" = "build-vet null
tests-01 null pkg/a=^(TestA|TestAUnit00|TestA_Setup)$ pkg/b=^(TestB)$ skip=^(TestSlow)$
tests-03 null pkg/b=. skip=^(TestSlow|TestB)$" ] && [ "$(cat $work/stale-names.txt)" = "pkg/a TestGone" ]'
# C': the selection's changed paths, beside its select.json, reach the planner as packages (#6pekqxy's headroom).
scenario; printf 'pkg/a/a.go\npkg/a/testdata/x.json\nREADME.md\n' > $T/changed-paths.txt; run $stubs/verify.sh 60
check budget-changed-packages '[ "$(grep -A1 -x -- --changed-packages $T/argv | tail -1)" = "github.com/system-inc/adamic,github.com/system-inc/adamic/pkg/a" ]'
scenario; run $stubs/verify.sh 60; check budget-no-changed-paths '! grep -qx -- --changed-packages $T/argv'
# D: budgeted without the tree, nothing names the members: no --only-tests, and the selection is one family spec, as today.
scenario notree; run $stubs/verify.sh 60
check budget-without-tree '! grep -qx -- --only-tests $T/argv && grep -qx -- --budget $T/argv && spec | grep -qF "pkg/a=^(TestA|TestGone)((Unit|Points|_)[0-9]+|_Setup|_Union)?$"'
# E: a budget that isn't seconds plans nothing.
scenario; run $stubs/verify.sh soon; check budget-not-seconds '[ ! -e $T/argv ] && grep -q "LOOM_FAST_BUDGET is seconds" $T/out.log'
echo "failures: ${failures}"
exit $((failures > 0))
