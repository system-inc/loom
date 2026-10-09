#!/bin/bash
# fast.sh: side work's fast gates on Loom's side pool (#qpc471v; @system_adamic, Oct 8: side work goes to the
# pool's spare capacity, the boxes keep the star and landings). The interface, agreed with developer tools:
#
#	the gate watcher writes  ~/.loom/jobs/fast/<sha>.json     {"branch", "sha", "base", "packages": [...], "env": {...}}
#	Loom writes              ~/.loom/jobs/fast/<sha>.verdict  first line as a fast gate's status.txt (green:/red:/void:)
#	and publishes            gate-logs/<sha12>/<stamp>/fast    the fast gate's shape, fast.json marked runner pool
#
# Each job is verify.sh's run on codex-side (go build and go vet as one unit, the listed packages' tests at once),
# at most `concurrent` jobs at a time and 5 of the pool's slots each. The pool covers the Go tests only: the record
# says so ("covers": "go-tests"), and a void verdict (Loom's fault, or nothing for the pool to run) sends the tip
# back to the boxes.
#
#	pilots/adamic-gate/fast.sh            # the server, as LaunchAgent com.loom.fast (a copy in ~/.loom/bin)
#	pilots/adamic-gate/fast.sh --once <sha>   # one job now, beside the server's (its .running keeps the server off it)
#
# A job is cancelled by writing ~/.loom/jobs/fast/<sha>.cancel (the watcher's skip list, developer tools): a job not
# started never starts, and a running one's coordinator is stopped within 10 s, which drops its units still queued on
# the pool. Its verdict reads void, cancelled, so the tip goes back to the boxes if anyone still wants it.
set -uo pipefail

jobs=${LOOM_FAST_JOBS:-${HOME}/.loom/jobs/fast}
concurrent=${LOOM_FAST_CONCURRENT:-2}
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
	rm -f "${work}"/run.log "${work}"/reds.txt "${work}"/reds.exit
	mkdir -p "${work}"
	python3 - "${jobs}/${sha}.json" "${work}" <<'PY'
import json, re, shlex, sys
job = json.load(open(sys.argv[1]))
work = sys.argv[2]
open(work + "/select-mode", "w").write("yes" if job.get("packages") == "select" else "")
for field in ("base_name", "tools"):
    open(work + "/" + field, "w").write(str(job.get(field, "")))
packages = [] if job.get("packages") == "select" else [package for package in job.get("packages") or [] if package]
open(work + "/packages", "w").write("^(" + "|".join(re.escape(package) for package in packages) + ")$" if packages else "")
open(work + "/package-list", "w").write("".join(package + "\n" for package in packages))
open(work + "/env", "w").write("".join("export %s=%s\n" % (key, shlex.quote(str(value))) for key, value in sorted((job.get("env") or {}).items()) if re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key)))
open(work + "/branch", "w").write(str(job.get("branch", "")))
open(work + "/base", "w").write(str(job.get("base", "")))
PY
	if [ -s "${work}/select-mode" ]; then
		runSelection "${sha}" "${stamp}" || return
	fi
	if [ ! -s "${work}/packages" ]; then
		finish "${sha}" "${stamp}" "void: ${sha} fast gate on Loom's side pool: the job names no Go package, so the boxes take it" ""
		return
	fi
	LOOM_VERIFY_WORK=${work} LOOM_VERIFY_ENV=${work}/env LOOM_VERIFY_PACKAGES=${work}/package-list LOOM_VERIFY_SELECT=$([ -f "${work}/select/select.json" ] && echo "${work}/select/select.json") "${HOME}/.loom/bin/verify.sh" "${sha}" "$(cat "${work}/packages")" none 5 > "${work}/verify.log" 2>&1
	run=$(head -1 "${work}/run.log" 2> /dev/null | awk '{print $2}' | tr -d :)
	line=$(head -1 "${work}/reds.txt" 2> /dev/null | cut -d, -f2-)
	case "$(cat "${work}/build.verdict" 2> /dev/null)|$(cat "${work}/reds.exit" 2> /dev/null)" in
		passed\|0) verdict="green: ${sha} fast gate on Loom's side pool, go tests only,${line} in $((SECONDS - started)) s (branch $(cat "${work}/branch"), run ${run}$([ -s "${work}/beyond" ] && echo "; not covered: $(cat "${work}/beyond")"))" ;;
		failed\|*) verdict="red: ${sha} fast gate on Loom's side pool, first failure at go build or vet (branch $(cat "${work}/branch"), run ${run})" ;;
		passed\|1) verdict="red: ${sha} fast gate on Loom's side pool, go tests only,${line}; first: $(grep -m1 '^FAIL ' "${work}/reds.txt" | cut -c6- | cut -d' ' -f1-2) (branch $(cat "${work}/branch"), run ${run})" ;;
		*) verdict="void: ${sha} fast gate on Loom's side pool broke (a unit never reported: Loom's fault), so the boxes take it (run ${run})" ;;
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
	"${HOME}/.loom/bin/adamic-gate" unit --sha "${sha}" --gate-inputs "$(cat "${HOME}/.loom/gate-inputs")" --id select --body "${HOME}/.loom/bin/select.sh" --output loom-out/select.tgz --output loom-out/select.stdout -- "$(cat "${work}/base")" "$(cat "${work}/base_name")" "$(cat "${work}/tools")" > "${work}/select-job.json" 2> "${work}/select-plan.log" || {
		finish "${sha}" "${stamp}" "void: ${sha} fast gate on Loom's side pool: the selection couldn't be planned, so the boxes take it" ""
		return 1
	}
	"${HOME}/.loom/bin/loom-pregate" run --uncached --slots none --pool codex-side=1 --record "${work}/select-record.jsonl" "${work}/select-job.json" > "${work}/select-run.log" 2>&1
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

# finish publishes the record and then writes the verdict, so the watcher never reads a verdict without its log.
finish() {
	local sha=$1 stamp=$2 verdict=$3 run=$4 work=${jobs}/$1.work record index gitDirectory tree commit
	record=$(mktemp -d)
	echo "${verdict}" > "${record}/status.txt"
	cp "${work}/reds.txt" "${record}/reds.txt" 2> /dev/null
	cp "${work}/build.txt" "${record}/build-vet.txt" 2> /dev/null
	[ -s "${work}/test.jsonl" ] && cp "${work}/test.jsonl" "${record}/test.jsonl"
	echo "loom side pool (codex-side), run ${run}" > "${record}/box.txt"
	python3 - "${record}/fast.json" "${verdict%%:*}" "${run}" "${sha}" "$(cat "${work}/branch" 2> /dev/null)" "$(cat "${work}/base" 2> /dev/null)" <<'PY'
import json, sys
path, verdict, run, sha, branch, base = sys.argv[1:]
json.dump({"finished": True, "verdict": verdict, "runner": "pool", "pool": "codex-side", "pool_run": run, "covers": "go-tests",
           "sha": sha, "branch": branch, "base": base}, open(path, "w"), indent=2)
PY
	find "${record}" -type f -size +5M -name '*.jsonl' -exec gzip -9 {} \;
	index=$(mktemp -u)
	gitDirectory=$(git -C "${gate}" rev-parse --absolute-git-dir)
	tree=$(cd "${record}" && GIT_INDEX_FILE=${index} git --git-dir="${gitDirectory}" --work-tree=. add -A -f . && GIT_INDEX_FILE=${index} git --git-dir="${gitDirectory}" write-tree)
	commit=$(git -C "${gate}" commit-tree "${tree}" -m "Fast gate of ${sha} on Loom's side pool: ${verdict%%(*}")
	git -C "${gate}" push -q origin "${commit}:refs/heads/gate-logs/${sha:0:12}/${stamp}/fast" || echo "fast: publishing ${sha:0:12}'s record failed"
	rm -f "${index}" "${record}"/* && rmdir "${record}"
	{ echo "${verdict}"; echo "record: gate-logs/${sha:0:12}/${stamp}/fast"; } > "${jobs}/${sha}.verdict.partial"
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
	serve "${once}"
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
		serve "${sha}" &
	done
	sleep 10
done
