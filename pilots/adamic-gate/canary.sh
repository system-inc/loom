#!/bin/bash
# canary.sh: main's tip through a tools directory's exact fast pipeline, before those tools may go live (#feg6ame,
# under #66qvxdd; @system_adamic, Oct 9 10:21Z: every gate fault tonight was found on a candidate because tools went
# live untested).
#
#	pilots/adamic-gate/canary.sh [<tools directory>]     # default ~/.loom/stage; a copy in ~/.loom/bin
#
# Main's tip M is a landing merge: its first parent the main it landed on, its second the landed tip T. The canary is
# T's fast gate merged onto that main (gate M), the merge-gated shape every candidate's job has, so a fault anywhere in
# select, unpack, plan, run, kept verdicts or the verdict itself shows here first (the changed-paths unpack, ff519d3,
# broke only merge-gated jobs). It runs that directory's fast.sh --once at priority 40 on the star's pool, into its own
# jobs directory (~/.loom/jobs/canary), and publishes nothing to gate-logs (LOOM_FAST_PUBLISH=0).
#
# Pass: the verdict is green. Red, void or no verdict fails, its reds listed: main's own whole gate rarely exists for
# the tip it moved to, so a red can't be checked against main's and the canary fails closed. The result is written to
# ~/.loom/canary/<stamp>.json, and a pass also to ~/.loom/canary/pass/<tools hash>, which promote.sh requires.
set -uo pipefail
tools=$(cd "${1:-${HOME}/.loom/stage}" && pwd) || { echo "canary: no tools directory ${1:-${HOME}/.loom/stage}"; exit 2; }
gate=${HOME}/Projects/system/adamic-gate
jobs=${LOOM_CANARY_JOBS:-${HOME}/.loom/jobs/canary}
out=${HOME}/.loom/canary
mkdir -p "${jobs}" "${out}/pass"
# The tools' content hash: every regular file in the directory by name and sha256, so a pass names exactly what ran.
hash=$(python3 - "${tools}" <<'PY'
import hashlib, os, sys
root = sys.argv[1]
lines = []
for name in sorted(os.listdir(root)):
    path = os.path.join(root, name)
    if name.startswith(".") or name == "__pycache__" or name.endswith(".pyc") or not os.path.isfile(path):
        continue
    lines.append("%s %s\n" % (hashlib.sha256(open(path, "rb").read()).hexdigest(), name))
print(hashlib.sha256("".join(lines).encode()).hexdigest())
PY
)
# The run uses a snapshot of the directory, named by that hash, so an edit made to it while the canary runs (fast.sh
# reads verify.sh minutes after it starts) can't make a pass name tools it didn't run.
snapshot=${out}/tools-${hash}
if [ ! -d "${snapshot}" ]; then
	# Regular files only, as the hash counts them (cp leaves a directory such as __pycache__ out).
	mkdir -p "${snapshot}.partial" && cp -p "${tools}"/* "${snapshot}.partial"/ 2> /dev/null
	mv "${snapshot}.partial" "${snapshot}"
fi
tools=${snapshot}
main=$(git -C "${gate}" ls-remote origin refs/heads/main | cut -f1)
git -C "${gate}" fetch -q origin "${main}" || { echo "canary: can't fetch main ${main:0:12}"; exit 2; }
read -r landedOn landed < <(git -C "${gate}" rev-list --parents -n 1 "${main}" | cut -d' ' -f2-)
[[ ${landed:-} =~ ^[0-9a-f]{40}$ ]] || { echo "canary: main ${main:0:12} isn't a landing merge, so it has no merge gate to run"; exit 2; }
# select.sh runs at the tools gate.sh uses for a whole gate: devtools/fast-gate's tip.
gateTools=$(git -C "${gate}" ls-remote origin refs/heads/devtools/fast-gate | cut -f1)
stamp=$(date -u +%Y%m%dT%H%M%SZ)
# A fresh job every time: an earlier canary of the same landing moves aside, never reused.
for earlier in "${jobs}/${landed}".*; do
	[ -e "${earlier}" ] && mv "${earlier}" "${earlier}.before-${stamp}"
done
python3 - "${jobs}/${landed}.json" "${landed}" "${landedOn}" "${main}" "${gateTools}" <<'PY'
import json, sys
path, landed, landedOn, main, gateTools = sys.argv[1:]
json.dump({"branch": "canary/main-" + main[:12], "sha": landed, "base": landedOn, "base_name": "main", "tools": gateTools,
           "packages": "select", "env": {}, "gate": main, "priority": 40}, open(path, "w"))
PY
echo "canary ${stamp}: tools ${hash:0:12} (${tools}) on main ${main:0:12}, the landing ${landed:0:12} gated onto ${landedOn:0:12}"
started=${SECONDS}
LOOM_BIN=${tools} LOOM_FAST_JOBS=${jobs} LOOM_FAST_PUBLISH=0 bash "${tools}/fast.sh" --once "${landed}" > "${out}/${stamp}.log" 2>&1
verdict=$(head -1 "${jobs}/${landed}.verdict" 2> /dev/null)
python3 - "${out}/${stamp}.json" "${hash}" "${tools}" "${main}" "${landed}" "${landedOn}" "${verdict}" "$((SECONDS - started))" "${jobs}/${landed}.work/reds.txt" <<'PY'
import json, os, sys
path, hash, tools, main, landed, landedOn, verdict, seconds, reds = sys.argv[1:]
failed = []
if os.path.exists(reds):
    failed = sorted({line[5:].split(" (")[0].strip() for line in open(reds) if line.startswith("FAIL ")})
passed = verdict.startswith("green:")
json.dump({"tools_hash": hash, "tools": tools, "main": main, "landed": landed, "landed_on": landedOn, "verdict": verdict,
           "seconds": int(seconds), "failed": failed, "pass": passed,
           "reason": "green" if passed else ("no verdict" if not verdict else verdict.split(":")[0] + (", %d failed tests" % len(failed) if failed else ""))},
          open(path, "w"), indent=2)
PY
if [[ ${verdict} == green:* ]]; then
	cp "${out}/${stamp}.json" "${out}/pass/${hash}"
	echo "canary ${stamp}: pass, ${verdict:0:200}"
	exit 0
fi
echo "canary ${stamp}: FAIL, ${verdict:-no verdict}"
exit 1
