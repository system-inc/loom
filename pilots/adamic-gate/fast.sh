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
# A job is cancelled by writing ~/.loom/jobs/fast/<sha>.cancel (the watcher's skip list, developer tools): a job not
# started never starts, and a running one's coordinator is stopped within 10 s, which drops its units still queued on
# the pool. Its verdict reads void, cancelled, so the tip goes back to the boxes if anyone still wants it.
#
# Every job runs under a ceiling (#x80gpc0; @system_adamic, Oct 9: 20 minutes for a fast gate, never a job that runs
# away). Past it, every coordinator the job started is stopped, which drops its units still queued on the pool, and
# the verdict says so: a red already in hand stays red, anything else is void with the ceiling as its reason.
set -uo pipefail

jobs=${LOOM_FAST_JOBS:-${HOME}/.loom/jobs/fast}
concurrent=${LOOM_FAST_CONCURRENT:-8}
ceiling=${LOOM_FAST_CEILING:-1200}
gate=${HOME}/Projects/system/adamic-gate
mkdir -p "${jobs}"
once=
if [ "${1:-}" = --once ]; then
	once=$2
	[[ ${once} =~ ^[0-9a-f]{40}$ ]] && [ -f "${jobs}/${once}.json" ] || { echo "fast: --once takes the sha of a job in ${jobs}"; exit 2; }
	[ -f "${jobs}/${once}.verdict" ] || [ -f "${jobs}/${once}.running" ] && { echo "fast: ${once:0:12} is already decided or running"; exit 2; }
	touch "${jobs}/${once}.running"
else
	# A job left running by a server that died starts again, unless something still runs it (a --once).
	for marker in "${jobs}"/*.running; do
		[ -e "${marker}" ] || continue
		pgrep -f "${jobs}/$(basename "${marker}" .running)\.work/" > /dev/null || rm -f "${marker}"
	done
fi

serve() {
	local sha=$1 started=${SECONDS} work=${jobs}/$1.work stamp verdict line run
	stamp=$(date -u +%Y%m%dT%H%M%SZ)
	rm -f "${work}"/run.log "${work}"/reds.txt "${work}"/reds.exit "${work}"/ceiling
	mkdir -p "${work}"
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
open(work + "/phases-wanted", "w").write("yes" if job.get("phases") is True or str(job.get("branch", "")).startswith("cloud/land-") else "")
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
	if [ -s "${work}/select-mode" ]; then
		runSelection "${sha}" "${stamp}" || return
	fi
	if [ ! -s "${work}/packages" ]; then
		finish "${sha}" "${stamp}" "void: ${sha} fast gate on Loom's side pool: the job names no Go package, so the boxes take it" ""
		return
	fi
	LOOM_PRIORITY=$(cat "${work}/priority") LOOM_VERIFY_WORK=${work} LOOM_VERIFY_ENV=${work}/env LOOM_VERIFY_PACKAGES=${work}/package-list LOOM_VERIFY_SELECT=$([ -f "${work}/select/select.json" ] && echo "${work}/select/select.json") "${HOME}/.loom/bin/verify.sh" "$(cat "${work}/gate")" "$(cat "${work}/packages")" none auto > "${work}/verify.log" 2>&1
	run=$(head -1 "${work}/run.log" 2> /dev/null | awk '{print $2}' | tr -d :)
	line=$(head -1 "${work}/reds.txt" 2> /dev/null | cut -d, -f2-)
	rm -f "${work}/phases-status" "${work}/phases-ref"
	if [ -s "${work}/phases-wanted" ] && [ "$(cat "${work}/build.verdict" 2> /dev/null)|$(cat "${work}/reds.exit" 2> /dev/null)" = "passed|0" ]; then
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

# runSelection runs the gate's own selection on one side instance (select.sh), then makes the job from its select.json:
# the packages, the env with the selection directory recreated on every test instance at the same path, and the
# select.json itself for verify.sh's only_tests and deferred. A selection that exits 1 with no select.json is the
# change's red; anything else that leaves none is void.
runSelection() {
	local sha=$1 stamp=$2 work=${jobs}/$1.work run token hash
	"${HOME}/.loom/bin/adamic-gate" unit --sha "$(cat "${work}/gate")" --gate-inputs "$(cat "${HOME}/.loom/gate-inputs")" --id select --body "${HOME}/.loom/bin/select.sh" --output loom-out/select.tgz --output loom-out/select.stdout -- "$(cat "${work}/base")" "$(cat "${work}/base_name")" "$(cat "${work}/tools")" > "${work}/select-job.json" 2> "${work}/select-plan.log" || {
		finish "${sha}" "${stamp}" "void: ${sha} fast gate on Loom's side pool: the selection couldn't be planned, so the boxes take it" ""
		return 1
	}
	# The star's selection goes on the star's pool, as its tests do (Oct 9 09:05Z: floor1 55f556cb's select waited on a
	# starved side pool, with 9 of 37 workers asking).
	local selectPool=codex-side
	[ "$(cat "${work}/priority" 2> /dev/null || echo 0)" -ge 30 ] && selectPool=codex
	"${HOME}/.loom/bin/loom-pregate" run --uncached --slots none --pool "${selectPool}=1" --priority "$(cat "${work}/priority" 2> /dev/null || echo 0)" --record "${work}/select-record.jsonl" "${work}/select-job.json" > "${work}/select-run.log" 2>&1
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
	python3 - "${work}" "${sha}" <<'PY'
import base64, json, re, shlex, sys
work, sha = sys.argv[1], sys.argv[2]
selection = json.load(open(work + "/select/select.json"))
packages = [package for package in selection.get("packages") or [] if package]
open(work + "/packages", "w").write("^(" + "|".join(re.escape(package) for package in packages) + ")$" if packages else "")
open(work + "/package-list", "w").write("".join(package + "\n" for package in packages))
archive = base64.b64encode(open(work + "/select.tgz", "rb").read()).decode()
lines = ["mkdir -p /tmp/loom-select/%s && echo %s | base64 -d | tar -xz -C /tmp/loom-select/%s" % (sha, archive, sha)]
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
	"${HOME}/.loom/bin/adamic-gate" plan --target codex --gate-inputs "$(cat "${HOME}/.loom/gate-inputs")" --reference "${reference}" --sha "$(cat "${work}/gate")" --units 1 --only '^nothing-matches$' \
		--phases "${tools}" --phase-units "${work}/phases-units.txt" 2> "${work}/phases-plan.err" | python3 -c "
import json, sys
job = json.load(sys.stdin); job['name'] = 'adamic-gate-fast-phases'; json.dump(job, open(sys.argv[1], 'w'))" "${work}/phases.json" || { echo "void: the phases couldn't be planned" > "${work}/phases-status"; return; }
	"${HOME}/.loom/bin/loom-pregate" run --uncached --slots none --pool codex-side=1 --priority "$(cat "${work}/priority" 2> /dev/null || echo 0)" --record "${work}/phases-record.jsonl" "${work}/phases.json" > "${work}/phases-run.log" 2>&1
	run=$(head -1 "${work}/phases-run.log" | awk '{print $2}' | tr -d :)
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
    if event.get('type') == 'uploaded' and event.get('path') == 'loom-out/phase.tar.gz': print(event['sha256'])" "${work}/phases-record.jsonl" 2> /dev/null | tail -1)
	# run.py's out directory, unpacked into a fresh directory of its own (kept, never deleted by a computed path).
	out=$(mktemp -d)
	echo "${out}" > "${work}/phases-out"
	if [ -z "${unitHash}" ] || ! curl -fsS "https://loom-wire.kirk-ouimet.workers.dev/runs/${run}/blobs/${unitHash}" -H "Authorization: Bearer ${token}" | tar -xzf - -C "${out}" || [ ! -s "${out}/phase/status.txt" ]; then
		echo "void: the phases unit left no status.txt (run ${run})" > "${work}/phases-status"
		return
	fi
	# run.py's own word, unless the unit said the fault was Loom's (exit 2).
	if grep -q '"type":"exit".*"code":2' "${work}/phases-record.jsonl"; then
		echo "void: the phases unit exited 2, Loom's fault (run ${run})" > "${work}/phases-status"
	else
		head -1 "${out}/phase/status.txt" > "${work}/phases-status"
	fi
	record=${out}/phase
	cp "${out}/phase.log" "${record}/phase.log" 2> /dev/null
	echo "loom side pool (codex-side), run ${run}" > "${record}/box.txt"
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

# within serves one job under the ceiling: a watchdog that, once the job has run `ceiling` seconds, marks it and stops
# every coordinator whose record lies in its work directory (the selection's, the tests', the phases'). Whatever path
# the job then takes to finish, finish reads the mark.
within() {
	local sha=$1 work=${jobs}/$1.work watchdog
	(
		sleep "${ceiling}"
		[ -f "${jobs}/${sha}.running" ] || exit 0
		touch "${work}/ceiling"
		pkill -TERM -f "loom-pregate run .*${work}/" && echo "$(date -u +%H:%M:%S) ceiling: ${sha:0:12} stopped at ${ceiling} s"
	) &
	watchdog=$!
	serve "${sha}"
	pkill -P "${watchdog}" 2> /dev/null
	kill "${watchdog}" 2> /dev/null
}

# finish publishes the record and then writes the verdict, so the watcher never reads a verdict without its log.
finish() {
	local sha=$1 stamp=$2 verdict=$3 run=$4 work=${jobs}/$1.work record index gitDirectory tree commit gated
	# A fast gate runs only its selection (verify.sh drops the planner's unplanned remainder): its record names how many
	# packages with tests it didn't run, so nobody reads a fast green as covering them (@system_adamic, Oct 9 09:02Z).
	local unplanned=""
	if [ -s "${work}/planned-packages.txt" ]; then
		unplanned=$(python3 "${HOME}/.loom/bin/treetests.py" "$(cat "${work}/gate" 2> /dev/null || echo "${sha}")" 2> /dev/null | cut -d' ' -f1 | sort -u | comm -23 - <(sort -u "${work}/planned-packages.txt") | wc -l | tr -d ' ')
		[ -n "${unplanned}" ] && [ "${unplanned}" != 0 ] && verdict="${verdict}; unplanned: ${unplanned} packages, not run"
	fi
	# A job stopped at its ceiling (within) says so: its red stands, and a void names the ceiling, not Loom's breakage.
	if [ -f "${work}/ceiling" ]; then
		case "${verdict%%:*}" in
			red) verdict="${verdict} (stopped at its $((ceiling / 60))-minute ceiling)" ;;
			*)
				# A red the finished units already proved stands: the units still running can't make it green (Oct 9:
				# lowering chain 97456986 had 8 units failed on named tests and still read void).
				if grep -q '^FAIL ' "${work}/reds.txt" 2> /dev/null; then
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
	[ -s "${work}/record.jsonl" ] && cp "${work}/record.jsonl" "${record}/record.jsonl"
	echo "loom side pool (codex-side), run ${run}" > "${record}/box.txt"
	# uncached_tests is the run's own mode, read from the coordinator's first line ("... units on N slots, uncached"),
	# which push-main --fast-gate requires of a landing's Go-test record (integration, Oct 9 04:38Z).
	python3 - "${record}/fast.json" "${verdict%%:*}" "${run}" "${sha}" "$(cat "${work}/branch" 2> /dev/null)" "$(cat "${work}/base" 2> /dev/null)" "$(head -1 "${work}/run.log" 2> /dev/null)" "$(cat "${work}/gate" 2> /dev/null)" "${unplanned}" <<'PY'
import json, sys
path, verdict, run, sha, branch, base, header, gate, unplanned = sys.argv[1:]
record = {"finished": True, "verdict": verdict, "runner": "pool", "pool": "codex-side", "pool_run": run, "covers": "go-tests",
          "uncached_tests": header.rstrip().endswith(", uncached"), "sha": sha, "branch": branch, "base": base}
# A gate of the tip merged onto main names both (#11ymb02), as the boxes' fast-gate.sh does.
if gate and gate != sha:
    record.update({"candidate": sha, "gated": gate})
if unplanned.isdigit():
    record["unplanned_packages_not_run"] = int(unplanned)
json.dump(record, open(path, "w"), indent=2)
PY
	find "${record}" -type f -size +5M -name '*.jsonl' -exec gzip -9 {} \;
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
	{ echo "${verdict}"; echo "record: gate-logs/${sha:0:12}/${stamp}/fast"; [ -s "${work}/phases-ref" ] && echo "phases: $(cat "${work}/phases-ref")"; } > "${jobs}/${sha}.verdict.partial"
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
if [ -n "${once}" ]; then
	within "${once}"
	exit
fi
while true; do
	for request in "${jobs}"/*.cancel; do
		[ -e "${request}" ] && cancel "$(basename "${request}" .cancel)"
	done
	for job in "${jobs}"/*.json; do
		[ -e "${job}" ] || continue
		sha=$(basename "${job}" .json)
		[[ ${sha} =~ ^[0-9a-f]{40}$ ]] || continue
		[ -f "${jobs}/${sha}.verdict" ] || [ -f "${jobs}/${sha}.running" ] || [ -f "${jobs}/${sha}.cancelled" ] && continue
		[ "$(find "${jobs}" -maxdepth 1 -name '*.running' | wc -l)" -ge "${concurrent}" ] && break
		touch "${jobs}/${sha}.running"
		within "${sha}" &
	done
	sleep 10
done
