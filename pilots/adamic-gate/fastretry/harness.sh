#!/bin/bash
# harness.sh [<fast.sh>]: fast.sh's runPhases, runStage3 and finish against stubs (#ber4297), one scenario per case, PASS or
# FAIL each. A phases or stage 3 unit Loom broke is placed once more; a second break, a cancel or the ceiling voids; a
# unit failing on its own is red. The mutant (both loops cut to one attempt) must fail:
#
#	pilots/adamic-gate/fastretry/harness.sh                       # the repo's fast.sh
#	sed 's/for attempt in 1 2; do/for attempt in 1; do/' fast.sh > m.sh && fastretry/harness.sh m.sh   # fails 7
#	sed 's/\[ ! -e "\${jobs}\/\${sha}.verdict.void-again" \] && //' fast.sh > m.sh && fastretry/harness.sh m.sh   # fails 1
#
# finish's void-again cases (#snbpqyb): a void at tier 40 and up goes back on the server's waiting list once, with its
# proven tests; the mutant above (every void at 40 placed again, the first-void check dropped) must fail.
set -u
here=$(cd "$(dirname "$0")" && pwd) failures=0
fast=${1:-${here}/../fast.sh}
stubs=$(mktemp -d)
mkdir -p "${stubs}/fakebin/tar/phase" && cp "${here}/curl" "${stubs}/fakebin/curl" && echo "green: phases passed (stub)" > "${stubs}/fakebin/tar/phase/status.txt"
tar -czf "${stubs}/phase.tgz" -C "${stubs}/fakebin/tar" phase
awk '/^runPhases\(\) \{/,/^}/' "$fast" > "$stubs/funcs.sh"; awk '/^runStage3\(\) \{/,/^}/' "$fast" >> "$stubs/funcs.sh"; awk '/^finish\(\) \{/,/^}/' "$fast" >> "$stubs/funcs.sh"
check() { if eval "$2"; then echo "PASS $1"; else echo "FAIL $1: calls $(cat $T/calls 2>/dev/null), status $(cat $work/phases-status 2>/dev/null)"; sed "s/^/    /" $T/out.log; failures=$((failures + 1)); fi; }
scenario() { # scenario <name> <plan lines...>
	T=$(mktemp -d) && export STUB=$T && cp "$stubs/phase.tgz" $T/ && printf '%s\n' "${@:2}" > $T/plan
	jobs=$T/jobs bin=$T/bin gate=$T/gate sha=1111111111111111111111111111111111111111 work=$T/jobs/$sha.work
	mkdir -p $work $bin && ln -s "$here/stub-pregate" $bin/loom-pregate && ln -s "$here/stub-gate" $bin/adamic-gate && ln -s "$here/../completion.py" $bin/completion.py
	git init -q --bare $T/origin.git && git init -q $gate && git -C $gate remote add origin $T/origin.git
	for f in tools base gate; do echo 2222222222222222222222222222222222222222 > $work/$f; done
	echo 40 > $work/priority; echo yes > $work/complete; echo '{"Action":"pass"}' > $work/test.jsonl
}
export PATH=$stubs/fakebin:$PATH
source "$stubs/funcs.sh"

scenario stage3-retry-green "phase-stage3-stage3-lane=exit2" ""
runStage3 $sha > $T/out.log; check stage3-retry-green '[ ! -s $work/phases-status ] && [ "$(cat $T/calls)" = 2 ] && python3 -c "import json,sys; sys.exit(0 if [u[\"id\"] for u in json.load(open(\"$T/job-2.json\"))[\"units\"]] == [\"phase-stage3-stage3-lane\"] else 1)"'
scenario stage3-nofinish-retry-green "phase-stage3-stage3-lane=nofinish" ""
runStage3 $sha > $T/out.log; check stage3-nofinish-retry-green '[ ! -s $work/phases-status ] && [ "$(cat $T/calls)" = 2 ]'
scenario stage3-twice-void "phase-stage3-stage3-lane=exit2" "phase-stage3-stage3-lane=nofinish"
runStage3 $sha > $T/out.log; check stage3-twice-void 'grep -q "^void: .*stage3-lane (run stub-run-2); its first attempt broke phase-stage3-stage3-lane (run stub-run-1)" $work/phases-status && [ "$(cat $T/calls)" = 2 ]'
scenario stage3-red-stands "phase-stage3-stage3-apply-tests=failed phase-stage3-stage3-lane=exit2"
runStage3 $sha > $T/out.log; check stage3-red-stands 'grep -q "^red: stage 3 landing lane failed: phase-stage3-stage3-apply-tests " $work/phases-status && [ "$(cat $T/calls)" = 1 ]'
scenario stage3-ceiling-no-retry "phase-stage3-stage3-lane=exit2"
touch $work/ceiling; runStage3 $sha > $T/out.log; check stage3-ceiling-no-retry 'grep -q "^void: " $work/phases-status && [ "$(cat $T/calls)" = 1 ]'
scenario stage3-green "" 
runStage3 $sha > $T/out.log; check stage3-green '[ ! -s $work/phases-status ] && [ "$(cat $T/calls)" = 1 ]'

# runPhases runs runStage3 after a non-void phases word (complete job): plan lines are per pregate call in order.
scenario phases-retry-green "phase-fast=noupload" "" ""
runPhases $sha 20261009T000000Z > $T/out.log 2>&1; check phases-retry-green 'grep -q "^green: phases passed" $work/phases-status && [ "$(cat $T/calls)" = 3 ] && git --git-dir=$T/origin.git show "gate-logs/111111111111/20261009T000000Z/fast-phases:box.txt" | grep -q "placed twice, its first attempt left no status.txt (run stub-run-1 on m10)"'
scenario phases-exit2-retry-green "phase-fast=exit2" "" ""
runPhases $sha 20261009T000000Z > $T/out.log 2>&1; check phases-exit2-retry-green 'grep -q "^green: phases passed" $work/phases-status && [ "$(cat $T/calls)" = 3 ]'
scenario phases-twice-void "phase-fast=noupload" "phase-fast=nofinish"
runPhases $sha 20261009T000000Z > $T/out.log 2>&1; check phases-twice-void 'grep -q "^void: the phases unit left no status.txt (run stub-run-2); its first attempt left no status.txt (run stub-run-1 on m10)" $work/phases-status && [ "$(cat $T/calls)" = 2 ]'
scenario phases-cancel-no-retry "phase-fast=noupload"
touch $jobs/$sha.cancelled; runPhases $sha 20261009T000000Z > $T/out.log 2>&1; check phases-cancel-no-retry 'grep -q "^void: " $work/phases-status && [ "$(cat $T/calls)" = 1 ]'
# A job stopped at its ceiling never sends its short record to the census: void, and no unit placed.
scenario phases-ceiling-no-census ""
touch $work/ceiling; runPhases $sha 20261009T000000Z > $T/out.log 2>&1; check phases-ceiling-no-census 'grep -q "^void: stopped at its ceiling" $work/phases-status && [ "$(cat $T/calls 2>/dev/null || echo 0)" = 0 ]'
# A phase run.py calls red with an empty log for that phase was killed at its limit: placed once more.
scenario phases-killed-build-retry-green "" "" ""
mkdir -p $T/k/phase && echo "red: x fast gate, first failure at build after 186.1 s, under load" > $T/k/phase/status.txt && : > $T/k/phase/build.log && tar -czf $T/phase-1.tgz -C $T/k phase
runPhases $sha 20261009T000000Z > $T/out.log 2>&1; check phases-killed-build-retry-green 'grep -q "^green: phases passed" $work/phases-status && [ "$(cat $T/calls)" = 3 ]'
# The same red with a failure in its log is the change's: never placed again.
scenario phases-build-red-stands "" ""
mkdir -p $T/k/phase && echo "red: x fast gate, first failure at build after 20.0 s" > $T/k/phase/status.txt && echo "./x.go:1: undefined: y" > $T/k/phase/build.log && tar -czf $T/phase-1.tgz -C $T/k phase
runPhases $sha 20261009T000000Z > $T/out.log 2>&1; check phases-build-red-stands 'grep -q "^red: x fast gate, first failure at build" $work/phases-status && [ "$(cat $T/calls)" = 2 ] && [ "$(python3 -c "import json; print(json.load(open(\"$T/job-2.json\"))[\"name\"])")" != adamic-gate-fast-phases ]'
scenario phases-green-once "" ""
runPhases $sha 20261009T000000Z > $T/out.log 2>&1; check phases-green-once 'grep -q "^green: phases passed" $work/phases-status && [ "$(cat $T/calls)" = 2 ] && git --git-dir=$T/origin.git show "gate-logs/111111111111/20261009T000000Z/fast-phases:box.txt" | grep -qv "placed twice"'
# finish: a merge-gated record's fast.json names the merge its units ran as its sha, beside candidate and gated, so
# push-main pairs it with the phases record run.py writes for that same tree; and it states its tests stage (completion.py).
scenario finish-names-the-merge
echo 3333333333333333333333333333333333333333 > $work/gate; echo "run stub-run-1: 1 units on 1 slots, uncached" > $work/run.log; echo passed > $work/build.verdict
printf '%s\n' '{"Package":"p","Test":"TestA","Action":"pass"}' '{"Package":"p","Test":"TestB","Action":"skip"}' > $work/test.jsonl
LOOM_FAST_PUBLISH=0 ceiling=1800 finish $sha 20261009T000000Z "green: stub" stub-run-1 > $T/out.log 2>&1
check finish-names-the-merge 'python3 -c "import json,glob,sys; r=json.load(open(glob.glob(\"$work/record-*/fast.json\")[0])); sys.exit(0 if (r[\"sha\"], r[\"candidate\"], r[\"gated\"]) == (\"3\"*40, \"1\"*40, \"3\"*40) and r[\"planned_stages\"] == [\"tests\"] and r[\"stages_exit\"] == {\"tests\": 0} and (r[\"pass\"], r[\"skip\"], r[\"fail\"]) == (1, 1, 0) and r[\"build_ok\"] else 1)"'
# finish: a void at tier 40 and up is placed again once, at the same tier (#snbpqyb; @system_adamic, Oct 9 16:00Z). Its
# verdict goes to <sha>.verdict.void-again, the work directory and its proven tests stay for serve to carry, and the
# server's own waiting list (its python, read from fast.sh) names the job again. A second void, tier 30, a cancel, a red
# and a --once run each write .verdict as before.
awk '/^	waiting=.*<<\047PYTHON\047$/{on=1; next} on && /^PYTHON$/{exit} on{print}' "$fast" > "$stubs/waiting.py"
grep -q 'ready.append' "$stubs/waiting.py" || { echo "FAIL: no waiting list in $fast"; failures=$((failures + 1)); }
voided() { # voided <priority> [verdict]: finish on a job that ran at <priority>, with two proven tests and one kept from before
	scenario "voided-$1"
	echo "$1" > $work/priority; echo "{\"sha\": \"$sha\", \"priority\": $1}" > $jobs/$sha.json; touch $jobs/$sha.running
	printf '%s\n' '{"Package":"p","Test":"TestA","Action":"pass"}' '{"Package":"p","Test":"TestB","Action":"pass"}' '{"Package":"p","Test":"TestC","Action":"fail"}' \
		'{"Package":"p","Test":"TestA/sub","Action":"pass"}' '{"Package":"p","Test":"TestK","Action":"pass","LoomSource":"stub-run-0"}' > $work/test.jsonl
	cp $work/test.jsonl $T/test-before.jsonl
	LOOM_FAST_PUBLISH=0 ceiling=1800 finish $sha 20261009T000000Z "${2:-void: $sha fast gate on Loom's side pool broke (a unit never reported: Loom's fault), so the boxes take it (run stub-run-1)}" stub-run-1 > $T/out.log 2>&1
}
served() { [ "$(python3 $stubs/waiting.py $jobs)" = "$sha $1" ]; }
voided 40
check void-again-first-void-at-40 '[ ! -e $jobs/$sha.verdict ] && [ ! -e $jobs/$sha.running ] && grep -q "^void: $sha .*Loom.s fault" $jobs/$sha.verdict.void-again && served 40 && cmp -s $work/test.jsonl $T/test-before.jsonl && grep -q "void-again: 111111111111 voided at tier 40, queued once more with 2 kept tests" $T/out.log && [ $(wc -l < $T/out.log) = 1 ]'
# The same job voids again on its second attempt: that void stands.
touch $jobs/$sha.running; cp $jobs/$sha.verdict.void-again $T/first-void
LOOM_FAST_PUBLISH=0 ceiling=1800 finish $sha 20261009T001000Z "void: $sha fast gate on Loom's side pool broke again (run stub-run-2)" stub-run-2 > $T/out.log 2>&1
check void-again-second-void-stands 'grep -q "^void: $sha .*broke again" $jobs/$sha.verdict && cmp -s $jobs/$sha.verdict.void-again $T/first-void && [ -z "$(python3 $stubs/waiting.py $jobs)" ] && ! grep -q void-again: $T/out.log'
voided 30
check void-at-30-stands 'grep -q "^void: " $jobs/$sha.verdict && [ ! -e $jobs/$sha.verdict.void-again ] && [ -z "$(python3 $stubs/waiting.py $jobs)" ]'
voided 40 "void: $sha cancelled while it ran (${sha:0:12}.cancel), so the boxes take it if it's still wanted (run stub-run-1)"
check void-cancelled-while-running-stands 'grep -q "^void: $sha cancelled while it ran" $jobs/$sha.verdict && [ ! -e $jobs/$sha.verdict.void-again ]'
# A cancel that landed as .cancelled, its verdict reading Loom's breakage (the ceiling's rewrite, or a race with serve's check).
scenario cancelled-file; echo 40 > $work/priority; echo "{\"sha\": \"$sha\", \"priority\": 40}" > $jobs/$sha.json; touch $jobs/$sha.running $jobs/$sha.cancelled
LOOM_FAST_PUBLISH=0 ceiling=1800 finish $sha 20261009T000000Z "void: $sha fast gate on Loom's side pool broke (run stub-run-1)" stub-run-1 > $T/out.log 2>&1
check void-at-40-cancelled-file-stands 'grep -q "^void: " $jobs/$sha.verdict && [ ! -e $jobs/$sha.verdict.void-again ]'
voided 40 "red: $sha fast gate on Loom's side pool, go tests only, 1 failed; first: p TestC (branch b, run stub-run-1)"
check red-at-40-stands 'grep -q "^red: " $jobs/$sha.verdict && [ ! -e $jobs/$sha.verdict.void-again ] && [ -z "$(python3 $stubs/waiting.py $jobs)" ]'
voided 40 "green: $sha fast gate on Loom's side pool, go tests only (run stub-run-1)"
check green-at-40-stands 'grep -q "^green: " $jobs/$sha.verdict && [ ! -e $jobs/$sha.verdict.void-again ]'
# A void the ceiling wrote (stopped with units unreported, no red in hand) is Loom's too: placed again.
scenario ceiling-void; echo 40 > $work/priority; echo "{\"sha\": \"$sha\", \"priority\": 40}" > $jobs/$sha.json; touch $jobs/$sha.running $work/ceiling
LOOM_FAST_PUBLISH=0 ceiling=1800 finish $sha 20261009T000000Z "void: $sha fast gate on Loom's side pool broke (run stub-run-1)" stub-run-1 > $T/out.log 2>&1
check ceiling-void-at-40-again 'grep -q "^void: .*30-minute ceiling" $jobs/$sha.verdict.void-again && [ ! -e $jobs/$sha.verdict ] && served 40 && grep -q "queued once more with 0 kept tests" $T/out.log'
# A --once run (the canary's, at tier 50) has no server behind it: its void stands for its caller to read.
once=$sha voided 40
check void-at-40-once-stands 'grep -q "^void: " $jobs/$sha.verdict && [ ! -e $jobs/$sha.verdict.void-again ]'
echo "failures: $failures"
exit $((failures > 0))
