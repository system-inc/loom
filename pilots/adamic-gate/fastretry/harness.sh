#!/bin/bash
# harness.sh [<fast.sh>]: fast.sh's runPhases and runStage3 against stubs (#ber4297), one scenario per case, PASS or
# FAIL each. A phases or stage 3 unit Loom broke is placed once more; a second break, a cancel or the ceiling voids; a
# unit failing on its own is red. The mutant (both loops cut to one attempt) must fail:
#
#	pilots/adamic-gate/fastretry/harness.sh                       # the repo's fast.sh
#	sed 's/for attempt in 1 2; do/for attempt in 1; do/' fast.sh > m.sh && fastretry/harness.sh m.sh   # fails 6
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
echo "failures: $failures"
exit $((failures > 0))
