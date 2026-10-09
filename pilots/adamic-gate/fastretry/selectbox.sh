#!/bin/bash
# selectbox.sh [<fast.sh>] [<placed.py>]: fast.sh's runSelection against stubs (#7cmv2g3), one scenario per case, PASS or
# FAIL each. A warm gate box's select.tgz stands in for the pool unit's: no unit planned, the job's env made from it, and
# select-run.log naming the box, which placed.py counts as the selection placed. A box that answers nothing, or hands back
# an archive with no select.json, leaves the selection to the pool. The mutant (the select.json check on the box's
# archive dropped) must fail:
#
#	pilots/adamic-gate/fastretry/selectbox.sh
#	sed 's/ \&\& \[ -f "${work}\/select\/select.json" \]; then$/; then/' fast.sh > m.sh && fastretry/selectbox.sh m.sh   # fails 1
#
# select.json byte for byte against the pool's, on the real boxes, is selectparity.sh.
set -u
here=$(cd "$(dirname "$0")" && pwd) failures=0
fast=${1:-${here}/../fast.sh}
placedPy=${2:-${here}/../placed.py}
stubs=$(mktemp -d)
awk '/^runSelection\(\) \{/,/^}/' "${fast}" > "${stubs}/funcs.sh"; awk '/^selectOnPool\(\) \{/,/^}/' "${fast}" >> "${stubs}/funcs.sh"; awk '/^selectionMade\(\) \{/,/^}/' "${fast}" >> "${stubs}/funcs.sh"
gated=2222222222222222222222222222222222222222
mkdir -p "${stubs}/archive" && echo '{"packages": ["example.com/a"], "env": {"ADAMIC_GATE_CHANGED": "/tmp/loom-select/'${gated}'/changed-paths.txt"}, "executors_beyond_go_tests": ["smoke"]}' > "${stubs}/archive/select.json"
echo a > "${stubs}/archive/changed-paths.txt"
tar -C "${stubs}/archive" -czf "${stubs}/select.tgz" .
mkdir -p "${stubs}/empty" && echo x > "${stubs}/empty/status.txt" && tar -C "${stubs}/empty" -czf "${stubs}/empty.tgz" .
check() { if eval "$2"; then echo "PASS $1"; else echo "FAIL $1: pool $(cat $T/pool 2> /dev/null || echo none), finished $(cat $T/finished 2> /dev/null || echo none)"; failures=$((failures + 1)); fi; }
scenario() { # scenario <box mode: ok, empty, fail, absent>
	T=$(mktemp -d) && export STUB=$T
	jobs=$T/jobs bin=$T/bin sha=1111111111111111111111111111111111111111 work=$T/jobs/$sha.work
	mkdir -p $work/select $bin
	echo ${gated} > $work/gate; echo 3333333333333333333333333333333333333333 > $work/base; echo main > $work/base_name; echo 4444444444444444444444444444444444444444 > $work/tools; echo 40 > $work/priority
	# An earlier attempt's selection, which must never stand in for this one's.
	echo '{"packages": ["example.com/stale"]}' > $work/select/select.json; echo "run stale-run: 1 units" > $work/select-run.log
	case $1 in
		ok) printf '#!/bin/bash\ncp %s "$5" && echo server\n' "${stubs}/select.tgz" > $bin/select-box.sh ;;
		empty) printf '#!/bin/bash\ncp %s "$5" && echo server\n' "${stubs}/empty.tgz" > $bin/select-box.sh ;;
		fail) printf '#!/bin/bash\necho "select-box: no box took it" >&2\nexit 3\n' > $bin/select-box.sh ;;
		absent) ;;
	esac
	[ -f $bin/select-box.sh ] && chmod +x $bin/select-box.sh
	# The pool's planner: the call is the pool path taken; it refuses, so the job finishes void as when planning fails.
	printf '#!/bin/bash\necho "$*" >> $STUB/pool\nexit 1\n' > $bin/adamic-gate && chmod +x $bin/adamic-gate
}
finish() { echo "$3" >> $T/finished; }
source "${stubs}/funcs.sh"

scenario ok; runSelection $sha 20261009T000000Z > $T/out.log 2>&1
check box-selects '[ ! -e $T/pool ] && [ ! -e $T/finished ] && grep -qx "example.com/a" $work/package-list && grep -q "^export ADAMIC_GATE_CHANGED=/tmp/loom-select/${gated}/changed-paths.txt$" $work/env && grep -q "^mkdir -p /tmp/loom-select/${gated} " $work/env && [ "$(cat $work/beyond)" = smoke ]'
check box-names-itself 'grep -q "^box server: selected in [0-9]* s (select-box.sh)$" $work/select-run.log && grep -q "^[0-9:]* select: 111111111111 in [0-9]* s on server$" $T/out.log'
mkdir -p $T/placed && echo yes > $work/select-mode && touch $jobs/$sha.running
python3 "${placedPy}" $jobs $sha > /dev/null 2>&1 & placed=$!
for _ in 1 2 3 4 5 6 7 8 9 10; do [ -s $jobs/$sha.placed ] && break; sleep 0.2; done
rm -f $jobs/$sha.running; kill $placed 2> /dev/null; wait $placed 2> /dev/null
check box-counts-placed '[ "$(cat $jobs/$sha.placed 2> /dev/null)" = "1 2" ]'
scenario empty; runSelection $sha 20261009T000000Z > $T/out.log 2>&1
check box-without-select-json-goes-to-pool '[ -s $T/pool ] && grep -q "^void: .*couldn.t be planned" $T/finished && ! grep -q "example.com/stale" $work/package-list 2> /dev/null'
scenario fail; runSelection $sha 20261009T000000Z > $T/out.log 2>&1
check box-fails-goes-to-pool '[ -s $T/pool ] && grep -q "^void: .*couldn.t be planned" $T/finished && [ ! -e $work/select/select.json ] && [ ! -e $work/select-run.log ]'
scenario absent; runSelection $sha 20261009T000000Z > $T/out.log 2>&1
check no-select-box-goes-to-pool '[ -s $T/pool ] && grep -q "^void: " $T/finished'
echo "failures: ${failures}"
exit $((failures > 0))
