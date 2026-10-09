#!/bin/bash
# verify.sh: a feedback run of one Adamic sha on Loom's pool, for whoever asked: go build ./... and go vet ./...
# as the first unit, then the named packages' tests in units at once. The build result goes to the requester the
# moment it exists (a build failure is the likeliest first red), the red list when the tests land.
#
#	pilots/adamic-gate/verify.sh <sha> <packages regex> <requester> [<slots>] [<note for the message>]
#
# A requester of "none" sends nothing (the fast-gate server reads the results from the work directory instead).
# LOOM_VERIFY_WORK names the work directory; LOOM_VERIFY_ENV names a file of `export K=V` lines every unit runs
# first (a fast gate's env); LOOM_VERIFY_PACKAGES names a file of import paths, one a line, so a package the
# reference never saw still runs whole; LOOM_VERIFY_SELECT names a fast gate's select.json, whose only_tests
# (package -> the only tests to run) and deferred (package -> tests to skip) shape every unit.
#
# LOOM_FAST_BUDGET, seconds a test unit may take, plans the tests budgeted like main's whole gate (@system_adamic, Oct
# 9 07:21Z: one gate, one rule). Unset, it is 60: budget mode lands on (@system_adamic, Oct 9 17:05Z, after Kirk's
# ruling that the caches land on, since an off-by-default landing only adds a flip we'd make anyway). "off" or empty
# plans a fixed count of units, as it ran from f18497c's dry run until #3sjs0rn brought the budgeted planner back.
#
# Side work runs only on the side pool (LOOM_VERIFY_POOL, default codex-side): the star's pool never holds it
# (@system_adamic, Oct 8 23:59Z). <slots> caps how much of it one run takes (default 5). Nothing builds on
# Kirk's Mac: the binaries are the pre-gate's (pregate.sh's header says how they are made on a box).
set -uo pipefail
# The tools this run uses: the live set by default, a staged set under its canary (#66qvxdd: pool tools are promoted only
# after main's tip passes through them).
export LOOM_BIN=${LOOM_BIN:-${HOME}/.loom/bin}
bin=${LOOM_BIN}
export LOOM_FAST_BUDGET=${LOOM_FAST_BUDGET-60}
[ "${LOOM_FAST_BUDGET}" = off ] && LOOM_FAST_BUDGET=""
sha=$1 packages=$2 requester=$3 slots=${4:-5} note=${5:-} pool=${LOOM_VERIFY_POOL:-codex-side}
work=${LOOM_VERIFY_WORK:-${HOME}/.loom/verify/${sha:0:12}-$(date -u +%Y%m%dT%H%M%SZ)}
mkdir -p "${work}"
loom=${bin}/loom-pregate planner=${bin}/adamic-gate
state=${HOME}/.adamic-full-gate gate=${HOME}/Projects/system/adamic-gate
send() {
	[ "${requester}" = none ] && return
	(cd /Users/kirkouimet/Projects/ahra && ./node_modules/.bin/ahra os send "${requester}" --body-file "$1" > /dev/null 2>&1 || true)
}

# The newest green whole gate's record sizes the units (the pre-gate keeps it cached).
green=$(cat "${state}/last-green")
reference=${HOME}/.loom/pregate/reference-${green}.jsonl.gz
if [ ! -s "${reference}" ]; then
	ref=$(git -C "${gate}" ls-remote origin "refs/heads/gate-logs/${green:0:12}/*" | awk '$2 ~ /\/full-main$/ {print $2}' | sort | tail -1)
	git -C "${gate}" fetch -q origin "${ref}" && git -C "${gate}" show FETCH_HEAD:test.jsonl.gz > "${reference}"
fi
# The tree's own test list at the sha: the plan's test list (a product new since the reference gets a unit of its own,
# run first, and every test unit of its package needs it, as run.py's box gate runs products first: the trio f9fc14c1's
# TestProduct_FixtureOracleHook rode in stage3/fixtures' remainder beside TestFixturesReal, which built the hook itself
# at 40.79 s), a selected name it doesn't hold dropped, TestWASI's shards a spec of their own, and zerorun.py's stale
# names read against it.
git -C "${gate}" fetch -q origin "${sha}" 2> /dev/null
python3 "${bin}/treetests.py" "${sha}" > "${work}/tree-tests.txt" 2> /dev/null || : > "${work}/tree-tests.txt"
treeTests=() && [ -s "${work}/tree-tests.txt" ] && treeTests=(--tree-tests "${work}/tree-tests.txt")
# Budgeted (LOOM_FAST_BUDGET seconds, the default), the planner sizes units by the budget, killed at one and a half times it, with
# Loom's own times, and a selection's only tests are packed exactly: the planner plans only the names it is given, plus
# their products and setups (--only-tests). Held back on its first dry run (f18497c: floor1 88bd168f's selection planned
# as 1,449 units and 133,676 s of predicted work, against about 8,000 s as 13 units, its untimed tests sized about 15
# times pessimistic), and came back behind a switch (#3sjs0rn, Oct 9). Off, nothing in this block changes the plan: no
# argument, the same stderr, the same message.
budget=() planErrors=/dev/null
if [ -n "${LOOM_FAST_BUDGET:-}" ]; then
	[[ ${LOOM_FAST_BUDGET} =~ ^[0-9]+(\.[0-9]+)?$ ]] || { echo "verify: LOOM_FAST_BUDGET is seconds a unit may take, not ${LOOM_FAST_BUDGET}"; exit 1; }
	budget=(--budget "${LOOM_FAST_BUDGET}" --unit-setup 10 --split-all)
	[ -s "${HOME}/.loom/loom-times.tsv" ] && budget+=(--loom-times "${HOME}/.loom/loom-times.tsv")
	planErrors=${work}/plan.err
	# The planner keeps a selected name only as itself, but a requested name is a family (run.py 7adc5bf9's rule, which
	# the selection's spec below writes): the name, its shards (the name plus Unit, Points or _ and a number), and its
	# _Setup and _Union. So the names go to the planner as the tree's members of each family; a family with none is
	# stale and plans nothing. Without the tree there are no members to name, and the selection is packed as below.
	rm -f "${work}/only-tests.json"
	if [ -n "${LOOM_VERIFY_SELECT:-}" ] && [ -s "${work}/tree-tests.txt" ]; then
		python3 - "${LOOM_VERIFY_SELECT}" "${work}/tree-tests.txt" "${work}/only-tests.json" <<'PY'
import json, re, sys
selected, treeTests, out = sys.argv[1], sys.argv[2], sys.argv[3]
only = json.load(open(selected)).get("only_tests") or {}
tree = {}
for line in open(treeTests):
    package, _, test = line.strip().partition(" ")
    if test:
        tree.setdefault(package, set()).add(test)
if only:
    json.dump({package: sorted(name for name in tree.get(package, ()) if any(re.fullmatch(re.escape(test) + r"((Unit|Points|_)[0-9]+|_Setup|_Union)?", name) for test in tests))
               for package, tests in only.items()}, open(out, "w"))
PY
		[ -s "${work}/only-tests.json" ] && budget+=(--only-tests "${work}/only-tests.json")
	fi
	# A changed package's tests pack at 1.5x their times (#6pekqxy): its tests are the likeliest to run slower than they
	# did. The selection's changed paths, beside its select.json, name the packages: each path's directory under the
	# module, up to any testdata. A name no plan holds changes nothing.
	changedPaths=$([ -n "${LOOM_VERIFY_SELECT:-}" ] && dirname "${LOOM_VERIFY_SELECT}")/changed-paths.txt
	if [ -s "${changedPaths}" ]; then
		changed=$(awk '{ directory = $0; sub(/\/[^\/]*$/, "", directory); if (directory == $0) directory = ""; sub(/(^|\/)testdata(\/.*)?$/, "", directory); print "github.com/system-inc/adamic" (directory == "" ? "" : "/" directory) }' "${changedPaths}" | sort -u | paste -sd, -)
		[ -n "${changed}" ] && budget+=(--changed-packages "${changed}")
	fi
fi
# Products pack into units of 300 s by Loom's times, budget on or off (#fysfvrx, @system_adamic, Oct 9): one unit each,
# 2bb9a979's 531 products were 531 of its 543 units. Loom's times size the products alone, so off, the tests keep the
# reference's times and splits; the reference holds no product, so without Loom's times each still runs alone.
products=(--product-budget 300)
[ -s "${HOME}/.loom/loom-times.tsv" ] && products+=(--product-times "${HOME}/.loom/loom-times.tsv")
"${planner}" plan --target codex --remainder --gate-inputs "$(cat "${HOME}/.loom/gate-inputs")" --reference "${reference}" --sha "${sha}" --units ${LOOM_VERIFY_UNITS:-12} --only "${packages}" ${treeTests[@]+"${treeTests[@]}"} "${products[@]}" ${budget[@]+"${budget[@]}"} > "${work}/tests.json" 2> "${planErrors}" || { echo "verify: planning failed$([ -s "${planErrors}" ] && echo ": $(tail -1 "${planErrors}")")"; exit 1; }

# The build-and-vet unit runs on the same opening as the tests, so it sees the tree they will.
python3 - "${work}" "${LOOM_VERIFY_ENV:-}" "${LOOM_VERIFY_PACKAGES:-}" "${LOOM_VERIFY_SELECT:-}" <<'PY'
import json, sys
work, environment, listed, selected = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
job = json.load(open(work + "/tests.json"))
# A fast gate runs the packages it names and nothing else. The planner's remainder carries "@unplanned=", which runs
# every package go list finds that the plan doesn't name; for a whole gate that's a package new since the reference,
# but here the plan names only the selection, so it ran about 75 packages whole in one unit (Oct 9: 6b11cbda's and
# trio 75d5288e's units ran past the 20-minute ceiling). A listed package the reference never saw is run below.
# What the plan did name is kept, so the record can say how many packages it didn't run (@system_adamic, Oct 9 09:02Z).
planned = set()
for unit in job["units"]:
    for spec in unit["argv"][5:]:
        if spec.startswith("@unplanned="):
            planned.update(name for name in spec[len("@unplanned="):].split(",") if name)
    unit["argv"] = unit["argv"][:5] + [spec for spec in unit["argv"][5:] if not spec.startswith("@unplanned=")]
open(work + "/planned-packages.txt", "w").write("".join(name + "\n" for name in sorted(planned)))
def quote(name):  # Go's regexp.QuoteMeta
    return "".join("\\" + c if c in "\\.+*?()|[]{}^$" else c for c in name)
selection = json.load(open(selected)) if selected else {}
import re
tree = {}
for line in open(work + "/tree-tests.txt"):
    package, _, test = line.strip().partition(" ")
    if test:
        tree.setdefault(package, set()).add(test)
# TestWASI and its shards TestWASIUnit00 and on run with the WASI SDK's clang first, in a spec of their own (Oct 9: the
# selection's ^(...|TestWASI) put all 36 shards on native clang, and every one skipped). The unit body knows the spec.
wasiClang = re.compile(r"TestWASI(Unit[0-9]+)?")
stale = []
import os
# The unit with the fewest specs, never a product unit while a test unit is left: a product unit runs to completion
# under a 600 s ceiling (#fysfvrx), and a package run whole there, as 2bb9a979's 21 listed packages were, could hit it.
def fewest():
    units = [unit for unit in job["units"] if not unit["id"].startswith("product-")] or job["units"]
    return min(units, key=lambda unit: len(unit["argv"]))
# Budgeted with the tree, the planner packed a selected package's family members itself (--only-tests above), the
# shards in a spec of their own: its remainder spec, which would run the rest, is dropped and its packed specs stay
# (f18497c). Otherwise its specs all leave for the one family spec below.
packed = bool(os.environ.get("LOOM_FAST_BUDGET")) and os.path.exists(work + "/only-tests.json")
# A package whose tests the gate names runs exactly those, in one spec on the unit with the fewest.
for package, tests in sorted((selection.get("only_tests") or {}).items()):
    for unit in job["units"]:
        if packed:
            unit["argv"] = unit["argv"][:5] + [spec for spec in unit["argv"][5:] if not (spec.split("=", 1)[0] == package and " skip=" in spec)]
        else:
            unit["argv"] = unit["argv"][:5] + [spec for spec in unit["argv"][5:] if spec.split("=", 1)[0] != package]
    # A requested name is a family: the test itself, or its split's shards, the name plus Unit, Points or _ and a number
    # (run.py 7adc5bf9's rule, which zerorun.py and the census read the same way; never a bare prefix, which took
    # TestWASITargetFlags for TestWASI). A name the tree at the sha holds no member of can't run: it's dropped and
    # listed, never planned (floor1 1e8eff51's plan named three, and its record was refused for running none of them).
    if tree:
        held = tree.get(package, set())
        members = {test: {name for name in held if re.fullmatch(re.escape(test) + r"((Unit|Points|_)[0-9]+)?", name)} for test in tests}
        stale += [package + " " + test for test in sorted(tests) if not members[test]]
        wasi = sorted(name for test in tests for name in members[test] if wasiClang.fullmatch(name))
        # A family holding a wasm shard runs in the shards' own spec; the rest keep the family spec.
        tests = [test for test in tests if members[test] and not any(wasiClang.fullmatch(name) for name in members[test])]
    else:
        wasi = []
    if packed:
        continue
    unit = fewest()
    if tests:
        # Never an exact anchor alone (@system_adamic, Oct 9 11:01Z: the trio's internal/native requested
        # TestNormalizeMatchesNode, the tree holds TestNormalizeMatchesNodePoints00 and on, and ^(...)$ ran "no tests to
        # run" and passed).
        # A split's shards need their family's X_Setup in the same process ("run TestShardsAgree_Setup before selecting
        # leaves"), and X_Union checks the shards' union, so both run beside the shards; zerorun.py counts only the
        # family's own members.
        unit["argv"].append(package + "=^(" + "|".join(quote(test) for test in sorted(tests)) + ")((Unit|Points|_)[0-9]+|_Setup|_Union)?$")
    if wasi:
        unit["argv"].append(package + "=^(" + "|".join(quote(name) for name in wasi) + ")$")
open(work + "/stale-names.txt", "w").write("".join(name + "\n" for name in stale))
# A remainder runs every test of its package the specs don't name, and the plan here comes from the reference, which
# can predate TestWASI's split: its shards would ride in internal/native's remainder on native clang. They leave it for
# a spec of their own on the same unit.
native = "github.com/system-inc/adamic/internal/native"
shards = sorted(name for name in tree.get(native, ()) if wasiClang.fullmatch(name))
for unit in job["units"]:
    for index, spec in enumerate(list(unit["argv"][5:]), start=5):
        package, _, pattern = spec.partition("=")
        if package != native or not pattern.startswith(". skip=^(") or not pattern.endswith(")$"):
            continue
        skip = pattern[len(". skip="):]
        loose = [name for name in shards if not re.search(skip, name)]
        if loose:
            unit["argv"][index] = package + "=. skip=" + skip[:-2] + "|" + "|".join(quote(name) for name in loose) + ")$"
            unit["argv"].append(native + "=^(" + "|".join(quote(name) for name in loose) + ")$")
# Tests an earlier attempt already proved, under the same package hash at this sha (inputs.py kept-match, named by
# LOOM_VERIFY_KEPT), are skipped like deferred ones: a re-plan, a new width or a run stopped at its ceiling never
# throws away a proven test (#kmtvw7m; @system_adamic, Oct 9 09:24Z: key kept verdicts by package and hash).
deferred = {package: list(tests) for package, tests in (selection.get("deferred") or {}).items()}
if os.environ.get("LOOM_VERIFY_KEPT"):
    for package, tests in json.load(open(os.environ["LOOM_VERIFY_KEPT"])).items():
        merged = sorted(set(deferred.get(package, [])) | set(tests))
        # A skip list rides in one exec argument (Linux's 128 KiB): a package whose kept names would pass about 100 KB
        # keeps nothing and runs whole, which is always sound.
        if sum(len(quote(test)) + 1 for test in merged) > 100_000:
            continue
        deferred[package] = merged
    open(work + "/kept-skipped.txt", "w").write("%d tests in %d packages kept from an earlier attempt\n" % (sum(len(tests) for tests in json.load(open(os.environ["LOOM_VERIFY_KEPT"])).values()), len(json.load(open(os.environ["LOOM_VERIFY_KEPT"])))))
# Tests the fast gate defers are skipped in every spec of their package.
for package, tests in deferred.items():
    if not tests:
        continue
    names = [quote(test) for test in sorted(tests)]
    for unit in job["units"]:
        for index, spec in enumerate(unit["argv"][5:], start=5):
            if spec.split("=", 1)[0] != package:
                continue
            if " skip=^(" in spec:
                head, skipped = spec.split(" skip=^(", 1)
                unit["argv"][index] = head + " skip=^(" + "|".join(names) + "|" + skipped
            else:
                unit["argv"][index] = spec + " skip=^(" + "|".join(names) + ")$"
job["units"] = [unit for unit in job["units"] if len(unit["argv"]) > 5]
# Budgeted, a unit can leave whole (a selected package's dropped remainder was its only spec), and the plan names its
# units as needs: a unit that needed one that left no longer waits on it (f18497c).
if os.environ.get("LOOM_FAST_BUDGET"):
    present = {unit["id"] for unit in job["units"]}
    for unit in job["units"]:
        if unit.get("needs"):
            unit["needs"] = [need for need in unit["needs"] if need in present]
            if not unit["needs"]:
                del unit["needs"]
# A listed package the reference never saw runs whole, as a remainder that skips nothing, on the unit with least.
covered = {spec.split("=", 1)[0] for unit in job["units"] for spec in unit["argv"][5:]}
for package in (open(listed).read().split() if listed else []):
    if package not in covered:
        fewest()["argv"].append(package + "=. skip=^()$")
# A job's env runs first in every unit, after the line naming the gate inputs.
exports = open(environment).read() if environment else ""
if exports:
    for unit in job["units"]:
        first, rest = unit["argv"][2].split("\n", 1)
        unit["argv"][2] = first + "\n" + exports.rstrip("\n") + "\n" + rest
script = job["units"][0]["argv"][2]
# The unit body begins at its own export of the gate's environment, the last one: a fast gate's env, injected
# above the opening, can export ADAMIC_GATE_UNCACHED too (d56ae116's build unit lost its whole opening to that).
opening = script[:script.rindex("export ADAMIC_GATE_UNCACHED")]
body = '''[ -n "${tree:-}" ] && [ -d "${tree}/.git" ] && [ -n "${out:-}" ] || { echo "loom-build: no tree to build (the opening never ran): Loom's fault"; exit 2; }
cd "${tree}" || exit 2
echo "loom-build: tree $(git -C "${tree}" rev-parse HEAD) setup $(( SECONDS - started )) s"
go build ./... > "${out}/build.log" 2>&1; build=$?
go vet ./... > "${out}/vet.log" 2>&1; vet=$?
if [ "${build}" = 0 ]; then echo "loom-build: go build ./... passed"; else echo "loom-build: go build ./... FAILED (exit ${build})"; head -40 "${out}/build.log"; fi
if [ "${vet}" = 0 ]; then echo "loom-build: go vet ./... passed"; else echo "loom-build: go vet ./... FAILED (exit ${vet})"; head -40 "${out}/vet.log"; fi
[ "${build}" = 0 ] && [ "${vet}" = 0 ]
'''
unit = {"id": "build-vet", "argv": ["bash", "-c", opening + body, "adamic-build-vet", job["units"][0]["argv"][4]],
        "timeoutSeconds": 3600, "outputs": [{"glob": "loom-out/build.log"}, {"glob": "loom-out/vet.log"}], "resources": {"cpus": 4}}
# Never more than one unsized package in a unit (@system_adamic, Oct 9 10:13Z): a remainder spec runs every test of its
# package (or of a split parent) the reference never sized, so two of them in one 4-CPU unit ran past the ceiling three
# times tonight with every other unit green. Each remainder beyond a unit's first goes to a unit of its own; this run
# sizes them for the next plan.
def unsized(spec):
    pattern = spec.split("=", 1)[1] if "=" in spec else ""
    return pattern == "." or pattern.startswith(". skip=") or "$/." in pattern
# (Its loop names its unit "crowd", never "unit": the build-vet unit above is held in "unit" until it's inserted below,
# and a loop that rebound it put the last test unit there twice and dropped build-vet, Oct 9 10:14 to 10:21Z.)
extra = []
for crowd in job["units"]:
    specs = crowd["argv"][5:]
    remainders = [spec for spec in specs if unsized(spec)]
    for index, spec in enumerate(remainders[1:], start=1):
        moved = json.loads(json.dumps(crowd))
        moved["id"] = "%s-r%d" % (crowd["id"], index)
        moved["argv"] = crowd["argv"][:5] + [spec]
        extra.append(moved)
    if len(remainders) > 1:
        crowd["argv"] = crowd["argv"][:5] + [spec for spec in specs if spec not in remainders[1:]]
job["units"] += extra
crowded = [crowd["id"] for crowd in job["units"] if sum(1 for spec in crowd["argv"][5:] if unsized(spec)) > 1]
if len({crowd["id"] for crowd in job["units"]} | {unit["id"]}) != len(job["units"]) + 1:
    sys.exit("verify: two units share an id after the split")
if crowded:
    sys.exit("verify: units still hold more than one unsized package: %s" % ", ".join(crowded))
job["name"] = "adamic-verify"
job["units"].insert(0, unit)
json.dump(job, open(work + "/job.json", "w"), indent=2)
PY
"${loom}" run --uncached --slots none --pool "${pool}=${slots}" --priority "${LOOM_PRIORITY:-0}" --record "${work}/record.jsonl" "${work}/job.json" > "${work}/run.log" 2>&1 &
coordinator=$!

# The build first, the moment its unit ends.
until grep -qE "^build-vet: (passed|failed|broken|void)" "${work}/run.log" || ! kill -0 "${coordinator}" 2> /dev/null; do sleep 5; done
run=$(head -1 "${work}/run.log" | awk '{print $2}' | tr -d :)
# Its output through curl (Cloudflare refuses Python's own client), with a coordinator token minted here.
token=$(python3 - "${run}" <<'PY'
import base64, hashlib, hmac, json, os, sys, time
secret = open(os.path.expanduser("~/.loom/token-secret")).read().strip().encode()
payload = base64.urlsafe_b64encode(json.dumps({"run": sys.argv[1], "scope": "coordinator", "expires": int(time.time()) + 600}, separators=(",", ":")).encode()).rstrip(b"=")
print((payload + b"." + base64.urlsafe_b64encode(hmac.new(secret, payload, hashlib.sha256).digest()).rstrip(b"=")).decode())
PY
)
# Every page of its events, 100 at a time: a build-vet placed after the first test units has its exit past the first
# page, and read from that page alone it was broken (Oct 9 10:24Z: b02c1093 and 3ea66219, build passed, run void).
: > "${work}/events.jsonl"
after=0
while page=$(curl -fsS "https://loom-wire.kirk-ouimet.workers.dev/runs/${run}/events?after=${after}" -H "Authorization: Bearer ${token}") && [ -n "${page}" ]; do
	printf '%s\n' "${page}" >> "${work}/events.jsonl"
	next=$(printf '%s\n' "${page}" | tail -1 | python3 -c 'import json, sys; print(json.loads(sys.stdin.read())["position"])')
	[ "${next}" -gt "${after}" ] || break
	after=${next}
done
python3 - "${work}" <<'PY'
import json, sys
work = sys.argv[1]
lines, code = [], None
for line in open(work + "/events.jsonl"):
    event = json.loads(line)["event"]
    if event.get("unit") != "build-vet":
        continue
    if event["type"] == "output":
        lines.append(event.get("text", "").rstrip("\n"))
    elif event["type"] == "exit":
        code = event.get("code")
text = "\n".join(lines)
# The build body exits 0 or 1; the opening exits 2 when Loom's own setup or checkout fails, before any build.
if code == 0:
    verdict = "passed"
elif code == 1 and "loom-build: go build" in text:
    verdict = "failed"
else:
    verdict = "broken"
open(work + "/build.verdict", "w").write(verdict + "\n")
open(work + "/build.txt", "w").write(text[:6000] + "\n")
PY
{
	case $(cat "${work}/build.verdict") in
		passed) echo "${sha:0:12} on Loom's pool: go build ./... and go vet ./... passed ($(grep -E '^build-vet: ' "${work}/run.log" | tail -1)):" ;;
		failed) echo "${sha:0:12} on Loom's pool: the build is RED ($(grep -E '^build-vet: ' "${work}/run.log" | tail -1)):" ;;
		*) echo "${sha:0:12} on Loom's pool: the build unit BROKE before building, Loom's setup or checkout, not your tree; it runs again ($(grep -E '^build-vet: ' "${work}/run.log" | tail -1)):" ;;
	esac
	echo
	cat "${work}/build.txt"
	echo
	echo "The packages (${packages}) are running; their red list follows when they land.${note:+ ${note}}"
} > "${work}/build-message.txt"
send "${work}/build-message.txt"

# Then the tests' whole red list.
wait "${coordinator}"
python3 -c "import json,sys; j=json.load(open(sys.argv[1])); j['units']=[u for u in j['units'] if u['id']!='build-vet']; json.dump(j,open(sys.argv[2],'w'))" "${work}/job.json" "${work}/tests-only.json"
# Weather is absorbed at the unit (#98cgqyc; @system_adamic, Oct 9 11:21Z: a unit's infra break never voids its job).
# A unit that broke for Loom's own reasons (exit 2, no results, killed in its opening) or was killed at its budget
# without a failing test proved nothing about the change: it is placed again in a run of its own, up to two more
# times, and the red list reads it from its last run while every other unit keeps its verdict. Broken all three
# times, the job is void and the red list names the unit and the workers it broke on. (The pool can't exclude a
# worker, so "again" means another placement, almost always on another of the pool's instances.)
reruns=()
for round in 1 2; do
	# No round starts past the job's ceiling or after its cancel: fast.sh's watchdog stops the run in flight, and a
	# round placed after it would run on unbounded (#gk5fh85, #m5x7n3n; Oct 9: canary 5 hung past its ceiling in a
	# retry round, 9cab573b retried for 45 minutes after its cancel, and stopping a round only started the next).
	if [ -f "${work}/ceiling" ] || [ -f "${work%.work}.cancelled" ] || [ -f "${work%.work}.cancel" ]; then
		echo "no retry round ${round}: the job is past its ceiling or cancelled" >> "${work}/run.log"
		break
	fi
	"${planner}" reds --job "${work}/tests-only.json" --record "${work}/record.jsonl" ${reruns[@]+"${reruns[@]}"} > "${work}/reds-round-${round}.txt" 2>&1
	again=$(python3 - "${work}/reds-round-${round}.txt" <<'PY'
import re, sys
lines = open(sys.argv[1]).read().splitlines()
weather = {re.match(r"BROKEN ([^ :]+):", line).group(1) for line in lines if re.match(r"BROKEN [^ :]+: (exited 2, Loom|no results|killed in its opening)", line)}
killed = {line.split()[1].rstrip(":") for line in lines if line.startswith("KILLED ")}
# A unit with a test that failed by name is red on that test, never weather.
named = {match.group(1) for line in lines for match in [re.match(r"FAIL github\.com/\S+ \S+ \(([^)]+)\)", line)] if match}
print(" ".join(sorted(weather | (killed - named))))
PY
)
	[ -n "${again}" ] || break
	python3 - "${work}/tests-only.json" "${work}/again-${round}.json" "${round}" ${again} <<'PY'
import json, sys
job, round, keep = json.load(open(sys.argv[1])), sys.argv[3], set(sys.argv[4:])
job["name"] += "-again"
job["units"] = [unit for unit in job["units"] if unit["id"] in keep]
for unit in job["units"]:
    # The unit knows it was placed again (and which round), for its log and for a planted proof's unit.
    unit.setdefault("environment", {})["LOOM_AGAIN"] = round
    unit["needs"] = [need for need in unit.get("needs") or [] if need in keep]
    if not unit["needs"]:
        unit.pop("needs", None)
json.dump(job, open(sys.argv[2], "w"))
PY
	echo "again, round ${round}: ${again}" >> "${work}/run.log"
	count=$(echo ${again} | wc -w | tr -d ' ')
	"${loom}" run --uncached --slots none --pool "${pool}=${count}" --priority "${LOOM_PRIORITY:-0}" --record "${work}/again-${round}-record.jsonl" "${work}/again-${round}.json" >> "${work}/run.log" 2>&1
	reruns+=(--rerun "${work}/again-${round}.json:${work}/again-${round}-record.jsonl")
done
"${planner}" reds --job "${work}/tests-only.json" --record "${work}/record.jsonl" ${reruns[@]+"${reruns[@]}"} --tests "${work}/test.jsonl" > "${work}/reds.txt" 2>&1
status=$?
# A unit still broken after its placements names the workers it broke on, so a void points at the unit, not the pool.
python3 - "${work}" >> "${work}/reds.txt" <<'PY'
import glob, json, os, re, sys
work = sys.argv[1]
broken = [re.match(r"BROKEN ([^ :]+):", line).group(1) for line in open(os.path.join(work, "reds.txt")) if re.match(r"BROKEN [^ :]+:", line)]
machines = {}
for path in [os.path.join(work, "record.jsonl")] + sorted(glob.glob(os.path.join(work, "again-*-record.jsonl"))):
    for line in open(path) if os.path.exists(path) else []:
        try:
            event = json.loads(line)
        except ValueError:
            continue
        event = event.get("event", event)
        if event.get("type") == "started" and event.get("unit") in broken:
            machines.setdefault(event["unit"], []).append(event.get("machine", "?"))
for unit in broken:
    print("BROKEN %s on %d placements, workers: %s" % (unit, len(machines.get(unit, [])), ", ".join(machines.get(unit, [])) or "none started"))
PY
(exit "${status}")
echo $? > "${work}/reds.exit"
# A unit that ran none of a family it requested, or whose tests started and never reached a verdict (a binary that died
# partway), proved nothing: it is red (@system_adamic, Oct 9 11:01Z and 06:13Z: a void is never a pass). zerorun.py
# checks every unit that reported against the tests the run's record holds, and the tree's own tests at the sha.
tree=() && [ -s "${work}/tree-tests.txt" ] && tree=(--tree-tests "${work}/tree-tests.txt")
if python3 "${bin}/zerorun.py" ${tree[@]+"${tree[@]}"} "${work}" > "${work}/zerorun.txt" 2>&1; then
	:
elif [ -s "${work}/zerorun.txt" ] && ! grep -q unreadable "${work}/zerorun.txt"; then
	# Its notes (a stale name, a tree it couldn't read) are lines of their own, never fields of four.
	awk -F'\t' 'NF == 4 && $4 !~ /^note: / {print "FAIL " $3 " " $4 " (" $2 ")"}' "${work}/zerorun.txt" >> "${work}/reds.txt"
	echo 1 > "${work}/reds.exit"
fi
{
	echo "${sha:0:12} on Loom's pool, the packages (${packages}):${note:+ ${note}}"
	echo
	head -c 10000 "${work}/reds.txt"
} > "${work}/reds-message.txt"
send "${work}/reds-message.txt"
echo "$(date -u +%H:%M:%S) ${sha:0:12}: $(head -1 "${work}/reds.txt")"
