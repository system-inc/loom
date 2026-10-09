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
set -uo pipefail

jobs=${LOOM_FAST_JOBS:-${HOME}/.loom/jobs/fast}
concurrent=${LOOM_FAST_CONCURRENT:-3}
gate=${HOME}/Projects/system/adamic-gate
mkdir -p "${jobs}"
# A job left running by a server that died starts again.
rm -f "${jobs}"/*.running

serve() {
	local sha=$1 started=${SECONDS} work=${jobs}/$1.work stamp verdict line run
	stamp=$(date -u +%Y%m%dT%H%M%SZ)
	rm -f "${work}"/run.log "${work}"/reds.txt "${work}"/reds.exit
	mkdir -p "${work}"
	python3 - "${jobs}/${sha}.json" "${work}" <<'PY'
import json, re, shlex, sys
job = json.load(open(sys.argv[1]))
work = sys.argv[2]
packages = [package for package in job.get("packages") or [] if package]
open(work + "/packages", "w").write("^(" + "|".join(re.escape(package) for package in packages) + ")$" if packages else "")
open(work + "/package-list", "w").write("".join(package + "\n" for package in packages))
open(work + "/env", "w").write("".join("export %s=%s\n" % (key, shlex.quote(str(value))) for key, value in sorted((job.get("env") or {}).items()) if re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key)))
open(work + "/branch", "w").write(str(job.get("branch", "")))
open(work + "/base", "w").write(str(job.get("base", "")))
PY
	if [ ! -s "${work}/packages" ]; then
		finish "${sha}" "${stamp}" "void: ${sha} fast gate on Loom's side pool: the job names no Go package, so the boxes take it" ""
		return
	fi
	LOOM_VERIFY_WORK=${work} LOOM_VERIFY_ENV=${work}/env LOOM_VERIFY_PACKAGES=${work}/package-list "${HOME}/.loom/bin/verify.sh" "${sha}" "$(cat "${work}/packages")" none 5 > "${work}/verify.log" 2>&1
	run=$(head -1 "${work}/run.log" 2> /dev/null | awk '{print $2}' | tr -d :)
	line=$(head -1 "${work}/reds.txt" 2> /dev/null | cut -d, -f2-)
	case "$(cat "${work}/build.verdict" 2> /dev/null)|$(cat "${work}/reds.exit" 2> /dev/null)" in
		passed\|0) verdict="green: ${sha} fast gate on Loom's side pool, go tests only,${line} in $((SECONDS - started)) s (branch $(cat "${work}/branch"), run ${run})" ;;
		failed\|*) verdict="red: ${sha} fast gate on Loom's side pool, first failure at go build or vet (branch $(cat "${work}/branch"), run ${run})" ;;
		passed\|1) verdict="red: ${sha} fast gate on Loom's side pool, go tests only,${line}; first: $(grep -m1 '^FAIL ' "${work}/reds.txt" | cut -c6- | cut -d' ' -f1-2) (branch $(cat "${work}/branch"), run ${run})" ;;
		*) verdict="void: ${sha} fast gate on Loom's side pool broke (a unit never reported: Loom's fault), so the boxes take it (run ${run})" ;;
	esac
	finish "${sha}" "${stamp}" "${verdict}" "${run}"
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

while true; do
	for job in "${jobs}"/*.json; do
		[ -e "${job}" ] || continue
		sha=$(basename "${job}" .json)
		[[ ${sha} =~ ^[0-9a-f]{40}$ ]] || continue
		[ -f "${jobs}/${sha}.verdict" ] || [ -f "${jobs}/${sha}.running" ] && continue
		[ "$(find "${jobs}" -maxdepth 1 -name '*.running' | wc -l)" -ge "${concurrent}" ] && break
		touch "${jobs}/${sha}.running"
		serve "${sha}" &
	done
	sleep 10
done
