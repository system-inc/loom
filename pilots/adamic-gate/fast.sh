#!/bin/bash
# fast.sh: side work's fast gates on Loom's side pool (#qpc471v; @system_adamic, Oct 8: side work goes to the
# pool's spare capacity, the boxes keep the star and landings). The interface, agreed with developer tools:
#
#	the gate watcher writes  ~/.loom/jobs/fast/<sha>.json     {"branch", "sha", "base", "packages": [...], "env": {...}}
#	Loom writes              ~/.loom/jobs/fast/<sha>.verdict  first line as a fast gate's status.txt (green:/red:/void:)
#	and publishes            gate-logs/<sha12>/<stamp>/fast    the fast gate's shape, fast.json marked runner pool
#
# Each job is verify.sh's run on codex-side (go build and go vet as one unit, the listed packages' tests at once),
# at most `concurrent` jobs at a time (8 since the side pool grew to 64 instances on Oct 9) and 5 of the pool's slots each. The pool covers the Go tests only: the record
# says so ("covers": "go-tests"), and a void verdict (Loom's fault, or nothing for the pool to run) sends the tip
# back to the boxes.
#
#	pilots/adamic-gate/fast.sh            # the server, as LaunchAgent com.loom.fast (a copy in ~/.loom/bin)
#	pilots/adamic-gate/fast.sh --once <sha>   # one job now, beside the server's (its .running keeps the server off it)
#
# Every job runs on the tools it was served with, to its verdict (#8xfsf4x): a snapshot of LOOM_BIN by content hash,
# named in its record as loom_tools. The server starts each job as fast.sh --served <sha>, in a session of its own.
#
# A job is cancelled by writing ~/.loom/jobs/fast/<sha>.cancel (the watcher's skip list, developer tools): a job not
# started never starts, and a running one's coordinator is stopped within 10 s, which drops its units still queued on
# the pool. Its verdict reads void, cancelled, so the tip goes back to the boxes if anyone still wants it.
#
# Every job runs under a ceiling (#x80gpc0; @system_adamic, Oct 9: 20 minutes for a fast gate, never a job that runs
# away). Past it, every coordinator the job started is stopped, which drops its units still queued on the pool, and
# the verdict says so: a red already in hand stays red, anything else is void with the ceiling as its reason.
set -uo pipefail
# The tools this run uses: the live set by default, a staged set under its canary (#66qvxdd: pool tools are promoted only
# after main's tip passes through them).
export LOOM_BIN=${LOOM_BIN:-${HOME}/.loom/bin}
bin=${LOOM_BIN}

jobs=${LOOM_FAST_JOBS:-${HOME}/.loom/jobs/fast}
concurrent=${LOOM_FAST_CONCURRENT:-8}
ceiling=${LOOM_FAST_CEILING:-1200}
gate=${HOME}/Projects/system/adamic-gate
mkdir -p "${jobs}"

# pin prints the snapshot of the tools in ${bin} a job runs on (#8xfsf4x; @system_adamic, Oct 9: candidates went back
# to the queue mid-run because tools were promoted under them). The snapshot is ~/.loom/canary/tools-<hash>, the hash
# canary.sh's (every regular file by name and sha256), made from the very bytes the hash read when it doesn't exist yet.
# A run whose LOOM_BIN is already its own snapshot (a canary's) pins to itself.
pin() {
	python3 - "${bin}" "${HOME}/.loom/canary" <<'PY'
import hashlib, os, sys
root, store = sys.argv[1:]
files = []
for name in sorted(os.listdir(root)):
    path = os.path.join(root, name)
    if name.startswith(".") or name == "__pycache__" or name.endswith(".pyc") or not os.path.isfile(path):
        continue
    files.append((name, open(path, "rb").read(), os.stat(path).st_mode & 0o7777))
digest = hashlib.sha256("".join("%s %s\n" % (hashlib.sha256(data).hexdigest(), name) for name, data, _ in files).encode()).hexdigest()
snapshot = os.path.join(store, "tools-" + digest)
if not os.path.isdir(snapshot):
    partial = "%s.partial-%d" % (snapshot, os.getpid())
    os.makedirs(partial)
    for name, data, mode in files:
        with open(os.path.join(partial, name), "wb") as out:
            out.write(data)
        os.chmod(os.path.join(partial, name), mode)
    try:
        os.rename(partial, snapshot)
    except OSError:
        # Another job made the same snapshot first: by its name, the same bytes.
        for name, _, _ in files:
            os.remove(os.path.join(partial, name))
        os.rmdir(partial)
print(snapshot)
PY
}

# A job runs as its own process, --once (by hand, requeue.sh, canary.sh) or --served (the server's, which marked it
# running): it pins the tools first and runs from that snapshot to its verdict, fast.sh itself included, so a promotion
# changes only jobs served after it. The live set's last promoted stage (.promoted) goes with it to the record.
once= served= pinned=
case "${1:-}" in
	--once | --served)
		[[ ${2:-} =~ ^[0-9a-f]{40}$ ]] && [ -f "${jobs}/$2.json" ] || { echo "fast: $1 takes the sha of a job in ${jobs}"; exit 2; }
		if ! snapshot=$(pin) || [ ! -f "${snapshot}/fast.sh" ] || { [ -n "${LOOM_TOOLS_PINNED:-}" ] && [ "${LOOM_TOOLS_PINNED}" != "${snapshot}" ]; }; then
			echo "fast: ${2:0:12} couldn't pin the tools in ${bin}"
			# The server's job is decided here, so it never sits marked running with nothing behind it.
			[ "$1" = --served ] && echo "void: $2 fast gate on Loom's side pool: its tools couldn't be pinned (Loom's fault), so the boxes take it" > "${jobs}/$2.verdict" && rm -f "${jobs}/$2.running"
			exit 2
		fi
		[ "${snapshot}" = "${bin}" ] || exec env LOOM_BIN="${snapshot}" LOOM_TOOLS_PINNED="${snapshot}" LOOM_TOOLS_PROMOTED="$(cut -d' ' -f1 "${bin}/.promoted" 2> /dev/null)" bash "${snapshot}/fast.sh" "$@"
		pinned=${snapshot##*/tools-}
		if [ "$1" = --once ]; then
			once=$2
			[ -f "${jobs}/${once}.verdict" ] || [ -f "${jobs}/${once}.running" ] && { echo "fast: ${once:0:12} is already decided or running"; exit 2; }
			touch "${jobs}/${once}.running"
		else
			served=$2
			[ -f "${jobs}/${served}.running" ] || { echo "fast: --served ${served:0:12} isn't marked running; the server marks a job before it serves it"; exit 2; }
		fi
		;;
	*)
		# A job left running by a server that died starts again, unless something still runs it: a --once or --served
		# job runs in a session of its own and outlives the server.
		for marker in "${jobs}"/*.running; do
			[ -e "${marker}" ] || continue
			pgrep -f "${jobs}/$(basename "${marker}" .running)\.work/|fast\.sh --(once|served) $(basename "${marker}" .running)" > /dev/null || rm -f "${marker}"
		done
		;;
esac

serve() {
	local sha=$1 started=${SECONDS} work=${jobs}/$1.work stamp verdict line run
	stamp=$(date -u +%Y%m%dT%H%M%SZ)
	# The run an earlier attempt's proof comes from, named on its kept events below.
	[ -s "${work}/run.log" ] && head -1 "${work}/run.log" | awk '{print $2}' | tr -d : > "${work}/previous-run"
	rm -f "${work}"/run.log "${work}"/reds.txt "${work}"/reds.exit "${work}"/ceiling "${work}"/kept.json "${work}"/kept-skipped.txt
	mkdir -p "${work}"
	# The tools this attempt runs on, named in its record: the snapshot's hash, and the stage last promoted into the set.
	echo "${pinned} ${LOOM_TOOLS_PROMOTED:-}" > "${work}/loom-tools"
	echo "$(date -u +%H:%M:%S) pinned: ${sha:0:12} on tools ${pinned:0:12}"
	# An earlier attempt's proven tests carry over (#v4cm3s7; @system_adamic, Oct 9 09:23Z: a run stopped at its ceiling
	# never discards its proven units). Its passed tests are kept by package and input hash at the gate it ran at, then
	# matched at this attempt's gate below; one attempt back, not accumulated.
	if [ -s "${work}/test.jsonl" ] && [[ $(cat "${work}/gate" 2> /dev/null) =~ ^[0-9a-f]{40}$ ]]; then
		# Only the attempt's own events: kept events it carried (tagged LoomSource) never carry again.
		grep -v '"LoomSource"' "${work}/test.jsonl" > "${work}/test-previous.jsonl"
		rm -f "${work}/test.jsonl"
		python3 "${bin}/inputs.py" kept --sha "$(cat "${work}/gate")" --gate-inputs "$(cat "${HOME}/.loom/gate-inputs")" "${work}/test-previous.jsonl" > "${work}/kept-previous.json" 2> /dev/null || rm -f "${work}/kept-previous.json"
	fi
	python3 - "${jobs}/${sha}.json" "${work}" <<'PY'
import json, re, shlex, sys
job = json.load(open(sys.argv[1]))
work = sys.argv[2]
open(work + "/select-mode", "w").write("yes" if job.get("packages") == "select" else "")
# The job's rank on the pool: developer tools' watcher puts the star's at 30 (Oct 9 04:23Z); side work is 0.
priority = job.get("priority", 0)
open(work + "/priority", "w").write(str(priority if isinstance(priority, int) and 0 <= priority <= 1000 else 0))
for field in ("base_name", "tools"):
    open(work + "/" + field, "w").write(str(job.get(field, "")))
# A landing (a cloud/land-* branch, or a job asking for "phases") gets the fast gate's other stages after its Go tests.
# A job marked complete (an area, or a landing that wants the box's whole shape, #vaf0xwc) runs its phases and the stage 3
# landing lane besides, and its record says so.
complete = job.get("complete") is True
open(work + "/complete", "w").write("yes" if complete else "")
open(work + "/phases-wanted", "w").write("yes" if complete or job.get("phases") is True or str(job.get("branch", "")).startswith("cloud/land-") else "")
packages = [] if job.get("packages") == "select" else [package for package in job.get("packages") or [] if package]
open(work + "/packages", "w").write("^(" + "|".join(re.escape(package) for package in packages) + ")$" if packages else "")
open(work + "/package-list", "w").write("".join(package + "\n" for package in packages))
open(work + "/env", "w").write("".join("export %s=%s\n" % (key, shlex.quote(str(value))) for key, value in sorted((job.get("env") or {}).items()) if re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key)))
open(work + "/branch", "w").write(str(job.get("branch", "")))
open(work + "/base", "w").write(str(job.get("base", "")))
# The merge the gate tests (#11ymb02; @system_adamic, Oct 9 08:17Z: every gate tests the tip merged onto main's tip):
# developer tools' watcher names it "gate" when the tip doesn't already hold main. The tip keeps naming the job.
gate = job.get("gate") or job["sha"]
open(work + "/gate", "w").write(gate if re.fullmatch(r"[0-9a-f]{40}", gate) else job["sha"])
PY
	# The job's "tools" (the adamic-gate commit its selection, phases and stage 3 run at) is pinned the same way: a job
	# that names none takes the fast-gate branch's tip once, here, never again at each stage while the branch moves.
	if [[ ! $(cat "${work}/tools") =~ ^[0-9a-f]{40}$ ]]; then
		local tip
		tip=$(git -C "${gate}" ls-remote origin refs/heads/devtools/fast-gate 2> /dev/null | cut -f1)
		[[ ${tip} =~ ^[0-9a-f]{40}$ ]] && echo "${tip}" > "${work}/tools"
	fi
	if [ -s "${work}/select-mode" ]; then
		runSelection "${sha}" "${stamp}" || return
	fi
	if [ ! -s "${work}/packages" ]; then
		finish "${sha}" "${stamp}" "void: ${sha} fast gate on Loom's side pool: the job names no Go package, so the boxes take it" ""
		return
	fi
	# The star's jobs (priority 30 and up) run on the star's pool, every unit at once (Oct 9 08:3xZ: on the side pool's
	# five slots 3095b212's thirteen units needed three waves of five to seven minutes, against the 20-minute ceiling).
	local width=5
	# and in units a third the size: the 12-unit plan held units of 13 to 15 minutes, which with selection and setup
	# overran the 20-minute ceiling (Oct 9 09:21Z: star 6b11cbda, f2a4527e and the trio each void with 1 to 3 left).
	[ "$(cat "${work}/priority")" -ge 30 ] && width=72 && export LOOM_VERIFY_POOL=codex LOOM_VERIFY_UNITS=36
	if [ -s "${work}/kept-previous.json" ]; then
		python3 "${bin}/inputs.py" kept-match --sha "$(cat "${work}/gate")" --gate-inputs "$(cat "${HOME}/.loom/gate-inputs")" "${work}/kept-previous.json" > "${work}/kept.json" 2> /dev/null || rm -f "${work}/kept.json"
	fi
	LOOM_VERIFY_KEPT=$([ -s "${work}/kept.json" ] && echo "${work}/kept.json") LOOM_PRIORITY=$(cat "${work}/priority") LOOM_VERIFY_WORK=${work} LOOM_VERIFY_ENV=${work}/env LOOM_VERIFY_PACKAGES=${work}/package-list LOOM_VERIFY_SELECT=$([ -f "${work}/select/select.json" ] && echo "${work}/select/select.json") "${bin}/verify.sh" "$(cat "${work}/gate")" "$(cat "${work}/packages")" none "${width}" > "${work}/verify.log" 2>&1
	run=$(head -1 "${work}/run.log" 2> /dev/null | awk '{print $2}' | tr -d :)
	line=$(head -1 "${work}/reds.txt" 2> /dev/null | cut -d, -f2-)
	# Kept proof's events join the record's test lines, each tagged with the run that proved it (@system_adamic, Oct 9
	# 08:00: one merged log, so the census, zerorun, red-sort and push-main all see the same proof; floor1's typeaware
	# phase groups read unknown while their sibling shards' passes sat in kept.json, out of the census's sight).
	if [ -s "${work}/kept.json" ] && [ -s "${work}/test-previous.jsonl" ] && [ -f "${work}/test.jsonl" ]; then
		python3 - "${work}" <<'PY' > "${work}/kept-merged.txt"
import json, os, sys
work = sys.argv[1]
kept = json.load(open(os.path.join(work, "kept.json")))
kept = {package: set(tests) for package, tests in kept.items()}
source = open(os.path.join(work, "previous-run")).read().strip() if os.path.exists(os.path.join(work, "previous-run")) else "earlier attempt"
fresh = set()
for line in open(os.path.join(work, "test.jsonl"), errors="replace"):
    try:
        event = json.loads(line)
    except ValueError:
        continue
    if event.get("Test"):
        fresh.add((event.get("Package", ""), event["Test"].split("/")[0]))
merged, tests = 0, set()
with open(os.path.join(work, "test.jsonl"), "a") as out:
    for line in open(os.path.join(work, "test-previous.jsonl"), errors="replace"):
        try:
            event = json.loads(line)
        except ValueError:
            continue
        top = (event.get("Test") or "").split("/")[0]
        # A test this attempt ran again keeps only its fresh events.
        if not top or top not in kept.get(event.get("Package", ""), ()) or (event.get("Package", ""), top) in fresh:
            continue
        event["LoomSource"] = source
        out.write(json.dumps(event, separators=(",", ":")) + "\n")
        merged += 1
        tests.add((event["Package"], top))
print("%d kept tests merged from %s (%d events)" % (len(tests), source, merged))
PY
	fi
	rm -f "${work}/phases-status" "${work}/phases-ref"
	# A complete job runs on past a red, as a box's complete mode does: its phases run whenever the build passed.
	if [ -s "${work}/phases-wanted" ] && { [ "$(cat "${work}/build.verdict" 2> /dev/null)|$(cat "${work}/reds.exit" 2> /dev/null)" = "passed|0" ] ||
		{ [ -s "${work}/complete" ] && [ "$(cat "${work}/build.verdict" 2> /dev/null)" = passed ]; }; }; then
		runPhases "${sha}" "${stamp}"
	fi
	case "$(cat "${work}/build.verdict" 2> /dev/null)|$(cat "${work}/reds.exit" 2> /dev/null)" in
		passed\|0) verdict="green: ${sha} fast gate on Loom's side pool, go tests only,${line} in $((SECONDS - started)) s (branch $(cat "${work}/branch"), run ${run}$([ -s "${work}/beyond" ] && echo "; not covered: $(cat "${work}/beyond")"))" ;;
		failed\|*) verdict="red: ${sha} fast gate on Loom's side pool, first failure at go build or vet (branch $(cat "${work}/branch"), run ${run})" ;;
		passed\|1) verdict="red: ${sha} fast gate on Loom's side pool, go tests only,${line}; first: $(grep -m1 '^FAIL ' "${work}/reds.txt" | cut -c6- | cut -d' ' -f1-2) (branch $(cat "${work}/branch"), run ${run})" ;;
		*) verdict="void: ${sha} fast gate on Loom's side pool broke (a unit never reported: Loom's fault), so the boxes take it (run ${run})" ;;
	esac
	# The landing's other stages (build, vet, smoke, census) decide with its Go tests: their red is the landing's red,
	# and a phase unit Loom broke sends the landing back to the boxes.
	case "$(cut -d: -f1 "${work}/phases-status" 2> /dev/null)" in
		"" | green) ;;
		red) verdict="red: ${sha} fast gate on Loom's side pool, $(cut -d: -f2- "${work}/phases-status" | cut -c1-200) (branch $(cat "${work}/branch"), run ${run})" ;;
		*) verdict="void: ${sha} fast gate on Loom's side pool: its build, vet, smoke and census unit broke (Loom's fault), so the boxes take it (run ${run})" ;;
	esac
	[ -f "${jobs}/${sha}.cancelled" ] && verdict="void: ${sha} cancelled while it ran (${sha:0:12}.cancel), so the boxes take it if it's still wanted (run ${run})"
	finish "${sha}" "${stamp}" "${verdict}" "${run}"
}

# runSelection runs the gate's own selection on a warm gate box (select-box.sh), or failing that on one side instance
# (select.sh), then makes the job from its select.json: the packages, the env with the selection directory recreated on
# every test instance at the same path, and the select.json itself for verify.sh's only_tests and deferred. A pool
# selection that exits 1 with no select.json is the change's red; anything else that leaves none is void.
runSelection() {
	local sha=$1 stamp=$2 work=${jobs}/$1.work
	# An earlier attempt's selection never stands in for this one's: a selection that fails or uploads nothing left the
	# old select.json in place, and the job ran on it (Oct 9 11:26Z: gocacheprog 77dcb095's new selection landed on a
	# black hole and failed in 0.2 s, and its 10:54Z selection, made before the job had a merge gate, named the tip's
	# changed paths while the env unpacked under the gate: 10,390 json tests failed).
	rm -f "${work}/select.tgz" "${work}/select/select.json" "${work}/select-events.jsonl" "${work}/select-run.log"
	# A warm gate box selects first (#7cmv2g3; @system_adamic, Oct 9 17:43Z: 318eef6a's select took 94 s of its 176 on a
	# cold Codex instance): the same run.py --select at the same tools, on a tree of the box's own, in seconds, held to 30 s
	# (select-box.sh). Its select.tgz lands where the pool unit's does. Anything else, a red included, is the pool's to
	# decide, as before. select-run.log names the box, which placed.py counts as the selection placed.
	local box selectStarted=${SECONDS}
	if [ -x "${bin}/select-box.sh" ] && box=$("${bin}/select-box.sh" "$(cat "${work}/gate")" "$(cat "${work}/base")" "$(cat "${work}/base_name")" "$(cat "${work}/tools")" "${work}/select.tgz" 2> "${work}/select-box.log") &&
		mkdir -p "${work}/select" && tar -xzf "${work}/select.tgz" -C "${work}/select" && [ -f "${work}/select/select.json" ]; then
		echo "box ${box}: selected in $((SECONDS - selectStarted)) s (select-box.sh)" > "${work}/select-run.log"
	else
		rm -f "${work}/select.tgz" "${work}/select/select.json"
		box=""
		selectOnPool "${sha}" "${stamp}" || return 1
	fi
	selectionMade "${sha}"
	# Select's wall in the server's log, wherever it ran, so a job's phases can be read from fast.log alone.
	echo "$(date -u +%H:%M:%S) select: ${sha:0:12} in $((SECONDS - selectStarted)) s on ${box:-the pool ($(head -1 "${work}/select-run.log" | awk '{print $2}' | tr -d :))}"
}

# selectOnPool runs the selection as one unit on the pool (select.sh), and leaves its select.tgz unpacked in
# <work>/select, or finishes the job: red when the gate's selection refused the change, void when Loom broke it.
selectOnPool() {
	local sha=$1 stamp=$2 work=${jobs}/$1.work run token hash
	"${bin}/adamic-gate" unit --sha "$(cat "${work}/gate")" --gate-inputs "$(cat "${HOME}/.loom/gate-inputs")" --id select --body "${bin}/select.sh" --output loom-out/select.tgz --output loom-out/select.stdout -- "$(cat "${work}/base")" "$(cat "${work}/base_name")" "$(cat "${work}/tools")" > "${work}/select-job.json" 2> "${work}/select-plan.log" || {
		finish "${sha}" "${stamp}" "void: ${sha} fast gate on Loom's side pool: the selection couldn't be planned, so the boxes take it" ""
		return 1
	}
	# The star's selection goes on the star's pool, as its tests do (Oct 9 09:05Z: floor1 55f556cb's select waited on a
	# starved side pool, with 9 of 37 workers asking).
	local selectPool=codex-side
	[ "$(cat "${work}/priority" 2> /dev/null || echo 0)" -ge 30 ] && selectPool=codex
	"${bin}/loom-pregate" run --uncached --slots none --pool "${selectPool}=1" --priority "$(cat "${work}/priority" 2> /dev/null || echo 0)" --record "${work}/select-record.jsonl" "${work}/select-job.json" > "${work}/select-run.log" 2>&1
	run=$(head -1 "${work}/select-run.log" | awk '{print $2}' | tr -d :)
	token=$(python3 - "${run}" <<'PY'
import base64, hashlib, hmac, json, os, sys, time
secret = open(os.path.expanduser("~/.loom/token-secret")).read().strip().encode()
payload = base64.urlsafe_b64encode(json.dumps({"run": sys.argv[1], "scope": "coordinator", "expires": int(time.time()) + 600}, separators=(",", ":")).encode()).rstrip(b"=")
print((payload + b"." + base64.urlsafe_b64encode(hmac.new(secret, payload, hashlib.sha256).digest()).rstrip(b"=")).decode())
PY
)
	curl -fsS "https://loom-wire.kirk-ouimet.workers.dev/runs/${run}/events?after=0" -H "Authorization: Bearer ${token}" > "${work}/select-events.jsonl"
	hash=$(python3 -c "
import json, sys
for line in open(sys.argv[1]):
    event = json.loads(line)['event']
    if event['type'] == 'uploaded' and event['path'].endswith('select.tgz'): print(event['sha256'])" "${work}/select-events.jsonl" | tail -1)
	mkdir -p "${work}/select"
	[ -n "${hash}" ] && curl -fsS "https://loom-wire.kirk-ouimet.workers.dev/runs/${run}/blobs/${hash}" -H "Authorization: Bearer ${token}" > "${work}/select.tgz" && tar -xzf "${work}/select.tgz" -C "${work}/select"
	if [ ! -f "${work}/select/select.json" ]; then
		if grep -q '^select: failed' "${work}/select-run.log" && grep -q 'exited 1' "${work}/select-events.jsonl"; then
			# The reason is the gate's own first failure from the selection's output (its status.txt still says
			# running when the selection stops early).
			reason=$(python3 -c "
import json, sys
text = '\n'.join(json.loads(line)['event'].get('text', '') for line in open(sys.argv[1]) if json.loads(line)['event']['type'] == 'output')
lines = [line.strip() for line in text.splitlines() if line.strip()]
start = next((index for index, line in enumerate(lines) if line.startswith('FIRST FAILURE')), None)
print(' '.join(lines[start:start + 2]) if start is not None else 'see the selection output')" "${work}/select-events.jsonl")
			finish "${sha}" "${stamp}" "red: ${sha} fast gate on Loom's side pool, the gate's selection refused the change: ${reason} (run ${run})" "${run}"
		else
			finish "${sha}" "${stamp}" "void: ${sha} fast gate on Loom's side pool: the selection broke (Loom's fault), so the boxes take it (run ${run})" "${run}"
		fi
		return 1
	fi
}

# selectionMade makes the job from <work>/select/select.json, wherever the selection ran.
selectionMade() {
	local sha=$1 work=${jobs}/$1.work
	python3 - "${work}" "${sha}" <<'PY'
import base64, json, os, re, shlex, sys
work, sha = sys.argv[1], sys.argv[2]
selection = json.load(open(work + "/select/select.json"))
packages = [package for package in selection.get("packages") or [] if package]
open(work + "/packages", "w").write("^(" + "|".join(re.escape(package) for package in packages) + ")$" if packages else "")
open(work + "/package-list", "w").write("".join(package + "\n" for package in packages))
archive = base64.b64encode(open(work + "/select.tgz", "rb").read()).decode()
# The selection ran at the gated sha (the merge, #11ymb02), and its env names /tmp/loom-select/<that sha>/ (its changed
# paths, ADAMIC_GATE_CHANGED): it unpacks there, never under the tip (Oct 9: every test reading the changed paths in
# the star 6b11cbda's merge gate cb0a07bd failed "no such file").
gated = open(work + "/gate").read().strip() if os.path.exists(work + "/gate") else sha
lines = ["mkdir -p /tmp/loom-select/%s && echo %s | base64 -d | tar -xz -C /tmp/loom-select/%s" % (gated, archive, gated)]
lines += ["export %s=%s" % (key, shlex.quote(str(value))) for key, value in sorted((selection.get("env") or {}).items()) if re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key)]
open(work + "/env", "w").write("\n".join(lines) + "\n")
beyond = selection.get("executors_beyond_go_tests") or []
open(work + "/beyond", "w").write(", ".join(str(item) for item in beyond))
PY
}

# runPhases runs a landing's fast gate beyond its Go tests as one unit on the side pool (developer tools' run.py at
# d5ccabd0: --phases build,vet,smoke,census --census over the pool's merged go test record, against the landing's base),
# then publishes run.py's out directory as gate-logs/<sha12>/<stamp>/fast-phases, the record integration's push-main
# takes as --also-gate beside the Go tests' as --fast-gate. It leaves the first line of run.py's status.txt in
# phases-status ("void: ..." when Loom broke it) and the record's ref in phases-ref.
runPhases() {
	local sha=$1 stamp=$2 work=${jobs}/$1.work tools base hash token run unitHash reference record index gitDirectory tree commit out
	# The census judges the merged go test record whole, so a job stopped at its ceiling with units unreported never
	# reaches it: its short record would read as the candidate's missing tests (developer tools, Oct 9 16:56Z:
	# hidden-boundaries 64a5179f's fast-phases merged 531 products and one smoke test, and deferred read 14 missing).
	if [ -f "${work}/ceiling" ]; then
		echo "void: stopped at its ceiling before every unit reported, so the census has no whole record to judge" > "${work}/phases-status"
		return
	fi
	tools=$(cat "${work}/tools" 2> /dev/null) base=$(cat "${work}/base" 2> /dev/null)
	[[ ${tools} =~ ^[0-9a-f]{40}$ ]] || tools=$(git -C "${gate}" ls-remote origin refs/heads/devtools/fast-gate | cut -f1)
	[[ ${base} =~ ^[0-9a-f]{40}$ ]] && [ -s "${work}/test.jsonl" ] || { echo "void: no base or no go test record for the phases" > "${work}/phases-status"; return; }
	hash=$(shasum -a 256 "${work}/test.jsonl" | cut -c1-64)
	token=$(python3 - census-input <<'PY'
import base64, hashlib, hmac, json, os, sys, time
secret = open(os.path.expanduser("~/.loom/token-secret")).read().strip().encode()
payload = base64.urlsafe_b64encode(json.dumps({"run": sys.argv[1], "scope": "coordinator", "expires": int(time.time()) + 600}, separators=(",", ":")).encode()).rstrip(b"=")
print((payload + b"." + base64.urlsafe_b64encode(hmac.new(secret, payload, hashlib.sha256).digest()).rstrip(b"=")).decode())
PY
)
	curl -fsS -X PUT --data-binary @"${work}/test.jsonl" -H "Authorization: Bearer ${token}" "https://loom-wire.kirk-ouimet.workers.dev/public/blobs/${hash}" > /dev/null || { echo "void: the go test record didn't reach the public store" > "${work}/phases-status"; return; }
	echo "fast ${hash} ${base}" > "${work}/phases-units.txt"
	reference=$(ls -t "${HOME}"/.loom/pregate/reference-*.jsonl.gz | head -1)
	"${bin}/adamic-gate" plan --target codex --gate-inputs "$(cat "${HOME}/.loom/gate-inputs")" --reference "${reference}" --sha "$(cat "${work}/gate")" --units 1 --only '^nothing-matches$' \
		--phases "${tools}" --phase-units "${work}/phases-units.txt" 2> "${work}/phases-plan.err" | python3 -c "
import json, sys
job = json.load(sys.stdin); job['name'] = 'adamic-gate-fast-phases'; json.dump(job, open(sys.argv[1], 'w'))" "${work}/phases.json" || { echo "void: the phases couldn't be planned" > "${work}/phases-status"; return; }
	# A phases unit Loom broke (no status.txt: the coordinator stopped, the worker was lost, the disk or the clone failed,
	# a kill before run.py wrote its word; or exit 2) is placed once more, automatically, and only a second break voids,
	# naming both (#ber4297; @system_adamic, Oct 9 14:21Z: the trio's phases voided on one coordinator stop and waited on
	# a hand rerun). The Go tests' verdicts are untouched: the same test.jsonl is the input both times. Never past the
	# job's ceiling or its cancel. The pool hands the unit to whichever worker asks next, so each attempt names its machine.
	local attempt suffix broken phase first=""
	for attempt in 1 2; do
		suffix=$([ "${attempt}" = 1 ] || echo "-${attempt}")
		"${bin}/loom-pregate" run --uncached --slots none --pool codex-side=1 --priority "$(cat "${work}/priority" 2> /dev/null || echo 0)" --record "${work}/phases-record${suffix}.jsonl" "${work}/phases.json" > "${work}/phases-run${suffix}.log" 2>&1
		run=$(head -1 "${work}/phases-run${suffix}.log" | awk '{print $2}' | tr -d :)
		token=$(python3 - "${run}" <<'PY'
import base64, hashlib, hmac, json, os, sys, time
secret = open(os.path.expanduser("~/.loom/token-secret")).read().strip().encode()
payload = base64.urlsafe_b64encode(json.dumps({"run": sys.argv[1], "scope": "coordinator", "expires": int(time.time()) + 600}, separators=(",", ":")).encode()).rstrip(b"=")
print((payload + b"." + base64.urlsafe_b64encode(hmac.new(secret, payload, hashlib.sha256).digest()).rstrip(b"=")).decode())
PY
)
		unitHash=$(python3 -c "
import json, sys
for line in open(sys.argv[1]):
    event = json.loads(line)
    if event.get('type') == 'uploaded' and event.get('path') == 'loom-out/phase.tar.gz': print(event['sha256'])" "${work}/phases-record${suffix}.jsonl" 2> /dev/null | tail -1)
		# run.py's out directory, unpacked into a fresh directory of its own (kept, never deleted by a computed path).
		out=$(mktemp -d)
		echo "${out}" > "${work}/phases-out"
		broken=""
		if [ -z "${unitHash}" ] || ! curl -fsS "https://loom-wire.kirk-ouimet.workers.dev/runs/${run}/blobs/${unitHash}" -H "Authorization: Bearer ${token}" | tar -xzf - -C "${out}" || [ ! -s "${out}/phase/status.txt" ]; then
			broken="left no status.txt"
		elif grep -q '"type":"exit".*"code":2' "${work}/phases-record${suffix}.jsonl"; then
			# run.py's own word, unless the unit said the fault was Loom's (exit 2).
			broken="exited 2, Loom's fault"
		elif phase=$(sed -nE '1s/^red: .*first failure at ([a-z]+) after.*/\1/p' "${out}/phase/status.txt") && [ -n "${phase}" ] && [ -f "${out}/phase/${phase}.log" ] && [ ! -s "${out}/phase/${phase}.log" ]; then
			# A phase run.py calls failed with nothing in its log was killed at its limit, no failure line: a cold, loaded
			# instance (Oct 9 14:33Z: the star's merged phases, build killed at 186 s under load 8.1, build.log empty).
			broken="was killed at ${phase} with no failure line"
		fi
		[ -z "${broken}" ] && break
		[ -f "${work}/ceiling" ] || [ -f "${jobs}/${sha}.cancelled" ] && break
		[ "${attempt}" = 2 ] && break
		first="${broken} (run ${run} on $(python3 -c "
import json, sys
print(next((event.get('machine') for event in map(json.loads, open(sys.argv[1])) if event.get('type') == 'started'), None) or 'no worker')" "${work}/phases-record${suffix}.jsonl" 2> /dev/null || echo 'no worker'))"
		echo "$(date -u +%H:%M:%S) phases: ${sha:0:12}'s unit ${first}; placed once more"
	done
	if [ -n "${broken}" ]; then
		echo "void: the phases unit ${broken} (run ${run})${first:+; its first attempt ${first}}" > "${work}/phases-status"
		[ -s "${out}/phase/status.txt" ] || return
	else
		head -1 "${out}/phase/status.txt" > "${work}/phases-status"
	fi
	[ -s "${work}/complete" ] && [ "$(cut -d: -f1 "${work}/phases-status")" != void ] && runStage3 "${sha}"
	record=${out}/phase
	cp "${out}/phase.log" "${record}/phase.log" 2> /dev/null
	echo "loom side pool (codex-side), run ${run}${first:+; placed twice, its first attempt ${first}}" > "${record}/box.txt"
	find "${record}" -type f -size +5M -name '*.jsonl' -exec gzip -9 {} \;
	index=$(mktemp -u)
	gitDirectory=$(git -C "${gate}" rev-parse --absolute-git-dir)
	tree=$(cd "${record}" && GIT_INDEX_FILE=${index} git --git-dir="${gitDirectory}" --work-tree=. add -A -f . && GIT_INDEX_FILE=${index} git --git-dir="${gitDirectory}" write-tree)
	commit=$(git -C "${gate}" commit-tree "${tree}" -m "Fast gate phases of ${sha} on Loom's side pool: $(cut -c1-80 "${work}/phases-status")")
	if git -C "${gate}" push -q origin "${commit}:refs/heads/gate-logs/${sha:0:12}/${stamp}/fast-phases"; then
		echo "gate-logs/${sha:0:12}/${stamp}/fast-phases" > "${work}/phases-ref"
	else
		echo "void: publishing the phases record failed" > "${work}/phases-status"
	fi
	rm -f "${index}"
}

# within serves one job under the ceiling: a watchdog that, once the job has run `ceiling` seconds past its first placed
# unit, marks it and stops every coordinator whose record lies in its work directory (the selection's, the tests', the
# phases'). Whatever path the job then takes to finish, finish reads the mark.
within() {
	local sha=$1 work=${jobs}/$1.work watchdog
	# The star's jobs (priority 30 and up) get 30 minutes tonight (@system_adamic, Oct 9 09:23Z): their units on the
	# 12- and 36-unit plans run up to about 16 minutes, slow and not runaway. A stopgap recorded on #vh41fpz, back to
	# 20 when the budgeted planner lands. The job's own priority is read here, before serve writes it.
	local priority
	priority=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1])).get("priority", 0))' "${jobs}/${sha}.json" 2> /dev/null || echo 0)
	[ "${priority:-0}" -ge 30 ] 2> /dev/null && ceiling=${LOOM_FAST_STAR_CEILING:-1800}
	# An earlier attempt's units-placed signal never starts this one's ceiling: cleared before the watchdog reads it.
	rm -f "${jobs}/${sha}.placed"
	(
		# The ceiling counts from the job's first placed unit, never from serve (#h16aj9a; @system_adamic, Oct 9 10:14
		# local: time queued with no unit on a worker says nothing about the change; that day five tier-30 jobs voided at
		# their ceiling with nothing run, their one selection unit never handed to a worker). placed.py's <sha>.placed
		# counts a unit once its started event is on the wire, not when the coordinator queues it. A job that ends or is
		# cancelled while it waits drops its .running, and the wait ends with it.
		until [ "$(cut -d' ' -f1 "${jobs}/${sha}.placed" 2> /dev/null)" -ge 1 ] 2> /dev/null; do
			[ -f "${jobs}/${sha}.running" ] || exit 0
			sleep 1
		done
		sleep "${ceiling}"
		[ -f "${jobs}/${sha}.running" ] || exit 0
		touch "${work}/ceiling"
		pkill -TERM -f "loom-pregate run .*${work}/" && echo "$(date -u +%H:%M:%S) ceiling: ${sha:0:12} stopped at ${ceiling} s"
	) &
	watchdog=$!
	# The units-placed signal for developer tools' watcher (#3tj643t) and the watchdog: <sha>.placed, "<placed> <total>",
	# while the job runs.
	python3 "${bin}/placed.py" "${jobs}" "${sha}" > /dev/null 2>&1 &
	local placed=$!
	serve "${sha}"
	kill "${placed}" 2> /dev/null
	pkill -P "${watchdog}" 2> /dev/null
	kill "${watchdog}" 2> /dev/null
}

# runStage3 runs a complete job's stage 3 landing lane (run.py's stage3 units: apply tests, lane tests, the lane) as one
# pool run beside its phases, the way gate.sh runs them for a whole gate. Any unit failing makes the phases red, naming it;
# one that exited 2 (Loom's fault) makes them void; all green leaves the phases' own word standing.
runStage3() {
	local sha=$1 work=${jobs}/$1.work tools reference run
	tools=$(cat "${work}/tools" 2> /dev/null)
	[[ ${tools} =~ ^[0-9a-f]{40}$ ]] || tools=$(git -C "${gate}" ls-remote origin refs/heads/devtools/fast-gate | cut -f1)
	printf 'stage3 %s\n' stage3-apply-tests stage3-lane-tests stage3-lane > "${work}/stage3-units.txt"
	reference=$(ls -t "${HOME}"/.loom/pregate/reference-*.jsonl.gz | head -1)
	"${bin}/adamic-gate" plan --target codex --gate-inputs "$(cat "${HOME}/.loom/gate-inputs")" --reference "${reference}" --sha "$(cat "${work}/gate")" --units 1 --only '^nothing-matches$' \
		--phases "${tools}" --phase-units "${work}/stage3-units.txt" 2> "${work}/stage3-plan.err" | python3 -c "
import json, sys
job = json.load(sys.stdin); job['name'] = 'adamic-gate-fast-stage3'; json.dump(job, open(sys.argv[1], 'w'))" "${work}/stage3.json" || { echo "void: the stage 3 lane couldn't be planned" > "${work}/phases-status"; return; }
	# A unit Loom broke (exit 2, broken, dropped when the coordinator stopped, never finished) is placed once more, the
	# lane's other units' verdicts kept, and only a second break voids, naming both runs (#ber4297). A unit that failed on
	# its own is the lane's red whatever else broke. Never past the job's ceiling or its cancel.
	local attempt suffix job=${work}/stage3.json classes broken failed first=""
	for attempt in 1 2; do
		suffix=$([ "${attempt}" = 1 ] || echo "-${attempt}")
		"${bin}/loom-pregate" run --uncached --slots none --pool "$([ "$(cat "${work}/priority" 2> /dev/null || echo 0)" -ge 30 ] && echo codex || echo codex-side)=3" --priority "$(cat "${work}/priority" 2> /dev/null || echo 0)" --record "${work}/stage3-record${suffix}.jsonl" "${job}" > "${work}/stage3-run${suffix}.log" 2>&1
		run=$(head -1 "${work}/stage3-run${suffix}.log" | awk '{print $2}' | tr -d :)
		# Each planned unit's class from the run's own record: passed, failed (on its own), or broken (Loom's).
		classes=$(python3 - "${job}" "${work}/stage3-record${suffix}.jsonl" <<'PY'
import json, sys
units = [unit["id"] for unit in json.load(open(sys.argv[1]))["units"]]
exits, finished = {}, {}
try:
    for line in open(sys.argv[2]):
        event = json.loads(line)
        if event.get("type") == "exit":
            exits[event.get("unit")] = event.get("code")
        elif event.get("type") == "finished":
            finished[event.get("unit")] = event.get("status")
except (OSError, ValueError):
    pass
broken = [unit for unit in units if exits.get(unit) == 2 or finished.get(unit) not in ("passed", "failed")]
failed = [unit for unit in units if unit not in broken and finished.get(unit) == "failed"]
print(" ".join(broken) + "|" + " ".join(failed))
PY
)
		broken=${classes%%|*} failed=${classes#*|}
		if [ -n "${failed}" ]; then
			echo "red: stage 3 landing lane failed: ${failed} (run ${run})" > "${work}/phases-status"
			return
		fi
		[ -z "${broken}" ] && return
		[ -f "${work}/ceiling" ] || [ -f "${jobs}/${sha}.cancelled" ] && break
		[ "${attempt}" = 2 ] && break
		first="${broken} (run ${run})"
		echo "$(date -u +%H:%M:%S) stage3: ${sha:0:12}'s ${broken} broke on Loom's side (run ${run}); placed once more"
		python3 - "${work}/stage3.json" "${work}/stage3-again.json" ${broken} <<'PY'
import json, sys
job, keep = json.load(open(sys.argv[1])), set(sys.argv[3:])
job["name"] += "-again"
job["units"] = [unit for unit in job["units"] if unit["id"] in keep]
for unit in job["units"]:
    unit["needs"] = [need for need in unit.get("needs") or [] if need in keep]
    if not unit["needs"]:
        unit.pop("needs", None)
json.dump(job, open(sys.argv[2], "w"))
PY
		job=${work}/stage3-again.json
	done
	echo "void: the stage 3 lane broke for Loom's own reasons: ${broken} (run ${run})${first:+; its first attempt broke ${first}}" > "${work}/phases-status"
}

# finish publishes the record and then writes the verdict, so the watcher never reads a verdict without its log.
finish() {
	local sha=$1 stamp=$2 verdict=$3 run=$4 work=${jobs}/$1.work record index gitDirectory tree commit gated
	# A fast gate runs only its selection (verify.sh drops the planner's unplanned remainder): its record names how many
	# packages with tests it didn't run, so nobody reads a fast green as covering them (@system_adamic, Oct 9 09:02Z).
	local unplanned=""
	if [ -s "${work}/planned-packages.txt" ]; then
		unplanned=$(python3 "${bin}/treetests.py" "$(cat "${work}/gate" 2> /dev/null || echo "${sha}")" 2> /dev/null | cut -d' ' -f1 | sort -u | comm -23 - <(sort -u "${work}/planned-packages.txt") | wc -l | tr -d ' ')
		[ -n "${unplanned}" ] && [ "${unplanned}" != 0 ] && verdict="${verdict}; unplanned: ${unplanned} packages, not run"
	fi
	# Tests kept from an earlier attempt are named in the verdict, so a green never hides that it rests on one.
	[ -s "${work}/kept-skipped.txt" ] && verdict="${verdict}; $(head -1 "${work}/kept-skipped.txt")"
	# A job stopped at its ceiling (within) says so: its red stands, and a void names the ceiling, not Loom's breakage.
	if [ -f "${work}/ceiling" ]; then
		case "${verdict%%:*}" in
			red) verdict="${verdict} (stopped at its $((ceiling / 60))-minute ceiling)" ;;
			*)
				# A red the finished units already proved stands: the units still running can't make it green (Oct 9:
				# lowering chain 97456986 had 8 units failed on named tests and still read void). But only a red of the
				# candidate's own: when every named red is main's, from the ruled list or main's own pool records, the
				# stop is Loom's and the job goes back to the pool as void (@system_adamic, Oct 9 06:43Z: C emission
				# 3fefe5b2 sat 80 minutes as red on main's TestCallTargetReaders beside 21 infra breaks).
				local mains
				mains=$(python3 - "${work}/reds.txt" "${HOME}/.loom/canary/main-reds.txt" "${HOME}/.loom/main.log" "${HOME}/.loom/pregate" <<'PY'
import os, re, sys
reds, ruled, log, pregate = sys.argv[1:]
def named(path):
    out = set()
    for line in open(path, errors="replace") if os.path.exists(path) else []:
        match = re.match(r"^FAIL (\S+) (\S+)", line)
        if match and match.group(2) != "(package)" and not match.group(2).startswith("("):
            out.add(match.group(1) + " " + match.group(2))
    return out
main = {line.strip() for line in open(ruled) if line.strip() and not line.startswith("#")} if os.path.exists(ruled) else set()
for line in open(log, errors="replace") if os.path.exists(log) else []:
    match = re.search(r" pre-gate of ([0-9a-f]{40}) ", line)
    if match:
        main |= named(os.path.join(pregate, match.group(1) + ".reds.txt"))
candidate = [line.rstrip("\n") for line in open(reds, errors="replace") if line.startswith("FAIL ")] if os.path.exists(reds) else []
mine = [line for line in candidate if " ".join(line.split()[1:3]) not in main]
# Nothing printed when any red is the candidate's own (or a package red, which names no test): the red stands.
if candidate and not mine:
    print(", ".join(sorted({line.split()[2] for line in candidate})))
PY
)
				if [ -n "${mains}" ]; then
					verdict="void: ${sha} fast gate on Loom's side pool stopped at its $((ceiling / 60))-minute ceiling (#x80gpc0) before every unit reported, its only named reds main's own (${mains}), so it goes back to the pool (run ${run})"
				# A kill isn't a proven red (provenreds.py): a ceiling stop whose only reds are units killed over budget or
				# tests whose builds were killed goes back to the pool as void, never to the lane as the change's red.
				elif grep -q '^FAIL ' "${work}/reds.txt" 2> /dev/null && ! python3 "${bin}/provenreds.py" "${work}/reds.txt" "${work}/test.jsonl" > /dev/null; then
					verdict="void: ${sha} fast gate on Loom's side pool stopped at its $((ceiling / 60))-minute ceiling (#x80gpc0) before every unit reported, its only reds kills (over budget, or a build killed), so it goes back to the pool (run ${run})"
				elif grep -q '^FAIL ' "${work}/reds.txt" 2> /dev/null; then
					verdict="red: ${sha} fast gate on Loom's side pool, first: $(grep -m1 '^FAIL ' "${work}/reds.txt" | cut -c6- | cut -d' ' -f1-2), stopped at its $((ceiling / 60))-minute ceiling with $(grep -c '^FAIL ' "${work}/reds.txt") failed tests and units unreported (branch $(cat "${work}/branch" 2> /dev/null), run ${run})"
				else
					verdict="void: ${sha} fast gate on Loom's side pool stopped at its $((ceiling / 60))-minute ceiling (#x80gpc0) before every unit reported, so the boxes take it (run ${run})"
				fi
				;;
		esac
	fi
	record=$(mktemp -d)
	echo "${verdict}" > "${record}/status.txt"
	cp "${work}/reds.txt" "${record}/reds.txt" 2> /dev/null
	cp "${work}/build.txt" "${record}/build-vet.txt" 2> /dev/null
	[ -s "${work}/test.jsonl" ] && cp "${work}/test.jsonl" "${record}/test.jsonl"
	# The job and the run's own record, so a fast record can be a rerun's base: inputs.py hashes every unit from its
	# argv and keeps the verdicts the record holds (#8f8f5y9, for push-main's rerun over a moved main, #mbexftz).
	[ -s "${work}/job.json" ] && cp "${work}/job.json" "${record}/job.json"
	[ -s "${work}/kept.json" ] && cp "${work}/kept.json" "${record}/kept.json"
	[ -s "${work}/record.jsonl" ] && cp "${work}/record.jsonl" "${record}/record.jsonl"
	echo "loom side pool (codex-side), run ${run}" > "${record}/box.txt"
	# uncached_tests is the run's own mode, read from the coordinator's first line ("... units on N slots, uncached"),
	# which push-main --fast-gate requires of a landing's Go-test record (integration, Oct 9 04:38Z).
	python3 - "${record}/fast.json" "${verdict%%:*}" "${run}" "${sha}" "$(cat "${work}/branch" 2> /dev/null)" "$(cat "${work}/base" 2> /dev/null)" "$(head -1 "${work}/run.log" 2> /dev/null)" "$(cat "${work}/gate" 2> /dev/null)" "${unplanned}" "$(cat "${work}/complete" 2> /dev/null)" "$(cat "${work}/loom-tools" 2> /dev/null)" "$(cat "${work}/tools" 2> /dev/null)" <<'PY'
import json, re, sys
path, verdict, run, sha, branch, base, header, gate, unplanned, complete, loomTools, gateTools = sys.argv[1:]
record = {"finished": True, "verdict": verdict, "runner": "pool", "pool": "codex-side", "pool_run": run, "covers": "go-tests",
          "uncached_tests": header.rstrip().endswith(", uncached"), "sha": sha, "branch": branch, "base": base}
# A gate of the tip merged onto main names both (#11ymb02), as the boxes' fast-gate.sh does, and its sha is the merge,
# the tree its units ran: the phases record run.py writes names that tree, and push-main pairs the two only when their
# shas agree (integration, Oct 9 14:45Z: the star's fast named f9fc14c1, its fast-phases c310d512, and it refused).
if gate and gate != sha:
    record.update({"sha": gate, "candidate": sha, "gated": gate})
if unplanned.isdigit():
    record["unplanned_packages_not_run"] = int(unplanned)
# A complete job ran its phases and the stage 3 lane; the Darwin leg is a box's alone and wasn't run here.
if complete == "yes":
    record.update({"complete": True, "darwin": "not run (pool is Linux only)"})
# The tools the job was served with and judged on (#8xfsf4x): Loom's, by the snapshot's content hash, with the stage
# promoted into the live set when it was served (the hash a canary pass names); and the adamic-gate commit its
# selection and phases ran at.
pinned = loomTools.split()
if pinned and re.fullmatch(r"[0-9a-f]{64}", pinned[0]):
    record["loom_tools"] = pinned[0]
    if len(pinned) > 1 and re.fullmatch(r"[0-9a-f]{64}", pinned[1]):
        record["loom_tools_promoted"] = pinned[1]
if re.fullmatch(r"[0-9a-f]{40}", gateTools):
    record["gate_tools"] = gateTools
json.dump(record, open(path, "w"), indent=2)
PY
	# The record states what its tests stage did (planned_stages, steps, exits, counts, build_ok), from its own test
	# lines and events: push-main lands only what a record says ran (integration, Oct 9 14:55Z).
	python3 "${bin}/completion.py" "${record}" "${verdict%%:*}" "$(cat "${work}/build.verdict" 2> /dev/null)"
	find "${record}" -type f -size +5M -name '*.jsonl' -exec gzip -9 {} \;
	local recordPath=gate-logs/${sha:0:12}/${stamp}/fast
	if [ "${LOOM_FAST_PUBLISH:-1}" = 0 ]; then
		# A canary's record proves tools, not a candidate (#feg6ame): it stays in the job's work directory, where no
		# watcher and no push-main reads it as a gate record.
		recordPath=${work}/record-${stamp}
		mv "${record}" "${recordPath}"
	else
		index=$(mktemp -u)
		gitDirectory=$(git -C "${gate}" rev-parse --absolute-git-dir)
		tree=$(cd "${record}" && GIT_INDEX_FILE=${index} git --git-dir="${gitDirectory}" --work-tree=. add -A -f . && GIT_INDEX_FILE=${index} git --git-dir="${gitDirectory}" write-tree)
		commit=$(git -C "${gate}" commit-tree "${tree}" -m "Fast gate of ${sha} on Loom's side pool: ${verdict%%(*}")
		git -C "${gate}" push -q origin "${commit}:refs/heads/gate-logs/${sha:0:12}/${stamp}/fast" || echo "fast: publishing ${sha:0:12}'s record failed"
		# The merge's own record path too, so push-main lands the merge by its record (#11ymb02).
		gated=$(cat "${work}/gate" 2> /dev/null)
		if [[ ${gated} =~ ^[0-9a-f]{40}$ ]] && [ "${gated}" != "${sha}" ]; then
			git -C "${gate}" push -q origin "${commit}:refs/heads/gate-logs/${gated:0:12}/${stamp}/fast" || echo "fast: publishing ${gated:0:12}'s record failed"
		fi
		rm -f "${index}" "${record}"/* && rmdir "${record}"
	fi
	{ echo "${verdict}"; echo "record: ${recordPath}"; [ -s "${work}/phases-ref" ] && echo "phases: $(cat "${work}/phases-ref")"; } > "${jobs}/${sha}.verdict.partial"
	# A void at tier 40 and up is re-placed at once, at the same tier, once (#snbpqyb; @system_adamic, Oct 9 16:00Z: the
	# operator renamed four such voids aside by hand). Its verdict goes to <sha>.verdict.void-again instead of .verdict,
	# so the watcher never reads a void for a job about to run again, and the loop below, which serves any job file with
	# no .verdict, .running or .cancelled, takes it on its next pass. That file is also the job's memory: with it present
	# a second void stands. The work directory stays as it is, so serve carries this attempt's proven tests to the next
	# (#v4cm3s7). A cancel, a green or a red never goes again, and neither does a --once run, whose caller (the canary)
	# reads its verdict when it exits and has no server behind it.
	if [ -z "${once:-}" ] && [ "${verdict%%:*}" = void ] && [ "$(cat "${work}/priority" 2> /dev/null || echo 0)" -ge 40 ] 2> /dev/null &&
		[ ! -e "${jobs}/${sha}.verdict.void-again" ] && [ ! -e "${jobs}/${sha}.cancel" ] && [ ! -e "${jobs}/${sha}.cancelled" ] && [[ ${verdict} != *" cancelled "* ]]; then
		mv "${jobs}/${sha}.verdict.partial" "${jobs}/${sha}.verdict.void-again"
		rm -f "${jobs}/${sha}.running"
		# The count serve will carry: this attempt's own top-level tests that passed and never failed (inputs.py kept).
		local kept
		kept=$(python3 - "${work}/test.jsonl" <<'PY'
import json, os, sys
passed, failed = set(), set()
for line in open(sys.argv[1], errors="replace") if os.path.exists(sys.argv[1]) else []:
    try:
        event = json.loads(line)
    except ValueError:
        continue
    test = event.get("Test") or ""
    if "LoomSource" in event or not test or "/" in test or event.get("Action") not in ("pass", "fail"):
        continue
    (passed if event["Action"] == "pass" else failed).add((event.get("Package", ""), test))
print(len(passed - failed))
PY
)
		echo "$(date -u +%H:%M:%S) void-again: ${sha:0:12} voided at tier $(cat "${work}/priority"), queued once more with ${kept} kept tests (its void: ${sha:0:12}.verdict.void-again)"
		return
	fi
	mv "${jobs}/${sha}.verdict.partial" "${jobs}/${sha}.verdict"
	rm -f "${jobs}/${sha}.running"
	echo "$(date -u +%H:%M:%S) ${verdict}"
}

# cancel <sha>: a running job's coordinator stopped (its record path names the sha), or a waiting one decided at once.
cancel() {
	local sha=$1
	if [ -f "${jobs}/${sha}.running" ]; then
		pkill -TERM -f "loom-pregate run .*${jobs}/${sha}\.work/" && echo "$(date -u +%H:%M:%S) cancelled: ${sha:0:12}'s run stopped"
	elif [ ! -f "${jobs}/${sha}.verdict" ]; then
		echo "void: ${sha} cancelled before it started (${sha:0:12}.cancel)" > "${jobs}/${sha}.verdict"
		echo "$(date -u +%H:%M:%S) cancelled: ${sha:0:12} before it started"
	fi
	mv "${jobs}/${sha}.cancel" "${jobs}/${sha}.cancelled"
}
if [ -n "${once}${served}" ]; then
	within "${once:-${served}}"
	exit
fi

# launch starts a job the loop marked running as fast.sh --served in a session of its own, its output joining the
# server's: it pins the tools as it starts, and the server's restart at a promotion (promote.sh) neither stops it nor
# changes what it runs (#8xfsf4x; until then a job ran as the server's own subshell and died with it).
launch() {
	python3 -c 'import os, sys; os.setsid(); os.execvp(sys.argv[1], sys.argv[1:])' bash "${bin}/fast.sh" --served "$1" < /dev/null &
}
while true; do
	for request in "${jobs}"/*.cancel; do
		[ -e "${request}" ] && cancel "$(basename "${request}" .cancel)"
	done
	# Waiting jobs start highest priority first (@system_adamic's tiers, Oct 9 09:25Z), and the star's (30 and up) never
	# wait on the concurrency cap: on Oct 9 the star's job sat unstarted behind eight priority-0 jobs.
	waiting=$(python3 - "${jobs}" <<'PYTHON'
import json, os, re, sys
jobs = sys.argv[1]
ready = []
for name in os.listdir(jobs):
    sha = name[:-5] if name.endswith(".json") else ""
    if not re.fullmatch(r"[0-9a-f]{40}", sha) or any(os.path.exists(os.path.join(jobs, sha + end)) for end in (".verdict", ".running", ".cancelled")):
        continue
    try:
        priority = int(json.load(open(os.path.join(jobs, name))).get("priority", 0))
    except (ValueError, OSError, TypeError):
        priority = 0
    ready.append((-priority, os.path.getmtime(os.path.join(jobs, name)), sha, priority))
for _, _, sha, priority in sorted(ready):
    print(sha, priority)
PYTHON
)
	# Admission control (@system_adamic, Oct 9 17:20Z, the queueing angel): with ~/.loom/admit holding K, at most K jobs
	# run at once, every tier included, so the candidates admitted get the whole pool and finish under their ceiling instead
	# of every candidate getting a little and the ceiling voiding the ones that never got enough. The rest wait here with
	# no unit placed, so nothing voids while it waits; tier picks who is admitted next. K is units the pool finishes in 30
	# minutes over units per candidate (Oct 9 17:22Z: 2,102 an hour over a mean of 292, so 4). Without the file, today's
	# rule: the cap holds below tier 30 only.
	admit=$(cat "${HOME}/.loom/admit" 2> /dev/null)
	[[ ${admit} =~ ^[1-9][0-9]*$ ]] || admit=""
	while read -r sha priority; do
		[ -n "${sha}" ] || continue
		running=$(find "${jobs}" -maxdepth 1 -name '*.running' | wc -l | tr -d ' ')
		if [ -n "${admit}" ]; then
			[ "${running}" -ge "${admit}" ] && break
			echo "$(date -u +%H:%M:%S) admitted ${sha:0:12} at tier ${priority}, ${running} running of ${admit}"
		else
			[ "${priority}" -lt 30 ] && [ "${running}" -ge "${concurrent}" ] && break
		fi
		touch "${jobs}/${sha}.running"
		launch "${sha}"
	done <<< "${waiting}"
	sleep 10
done
