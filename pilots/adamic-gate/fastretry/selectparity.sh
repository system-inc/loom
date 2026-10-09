#!/bin/bash
# selectparity.sh [<select-box.sh>] [<job sha>...]: select-box.sh on the real gate boxes against real fast jobs whose pool
# selection is on disk (#7cmv2g3): each box selects each job's gated sha at its base and tools, in turn, and its
# select.json must be byte for byte the pool unit's (<job>.work/select/select.json). Jobs alternate on one tree, so a
# box that never moves its tree to the gated sha selects the wrong commit. Read-only on the live jobs; each box writes
# only its own ~/loom-select. The mutant (the fetch and switch to the gated sha dropped) must fail:
#
#	pilots/adamic-gate/fastretry/selectparity.sh
#	sed 's/git -C "${tree}" fetch -q origin "${sha}" "${base}" "${tools}" < \/dev\/null \&\& git -C "${tree}" switch -q --detach "${sha}" \&\&/true \&\&/' select-box.sh > m.sh && fastretry/selectparity.sh m.sh   # fails 1 per box: the job its tree is not at
#
# LOOM_SELECT_BOXES names the boxes (each tried alone); the default jobs are two of Oct 9's with the same tools, one a
# merge gate (75473437 gated as 60d15d6a) and one its own tip (0ff31e41). Each line says the wall, the bar is 10 s.
set -u
here=$(cd "$(dirname "$0")" && pwd) failures=0
selectBox=${1:-${here}/../select-box.sh}
shift $(( $# > 0 ? 1 : 0 ))
jobs=${LOOM_FAST_JOBS:-${HOME}/.loom/jobs/fast}
picked=("$@")
[ ${#picked[@]} -gt 0 ] || picked=(75473437eacc7be16cf50111f228e74b8491e45f 0ff31e41802d0a5bbc9fef743ee0ef02930cde5e)
scratch=$(mktemp -d)
for box in ${LOOM_SELECT_BOXES:-server home workshop cloud}; do
	for job in "${picked[@]}"; do
		work=${jobs}/${job}.work
		[ -f "${work}/select/select.json" ] || { echo "FAIL ${box} ${job:0:12}: no pool select.json in ${work}"; failures=$((failures + 1)); continue; }
		archive=${scratch}/${box}-${job}.tgz
		started=${SECONDS}
		LOOM_SELECT_BOXES=${box} bash "${selectBox}" "$(cat "${work}/gate")" "$(cat "${work}/base")" "$(cat "${work}/base_name")" "$(cat "${work}/tools")" "${archive}" > /dev/null 2> "${scratch}/${box}-${job}.log"
		code=$?
		wall=$((SECONDS - started))
		mkdir -p "${scratch}/${box}-${job}"
		if [ "${code}" = 0 ] && tar -xzf "${archive}" -C "${scratch}/${box}-${job}" && cmp -s "${scratch}/${box}-${job}/select.json" "${work}/select/select.json" &&
			cmp -s "${scratch}/${box}-${job}/changed-paths.txt" "${work}/select/changed-paths.txt"; then
			echo "PASS ${box} ${job:0:12} (gated $(cut -c1-12 "${work}/gate")): select.json and changed-paths.txt identical to the pool's, ${wall} s"
		else
			echo "FAIL ${box} ${job:0:12} (gated $(cut -c1-12 "${work}/gate")): exit ${code} after ${wall} s"
			sed 's/^/    /' "${scratch}/${box}-${job}.log" | tail -6
			[ -f "${scratch}/${box}-${job}/select.json" ] && diff "${work}/select/select.json" "${scratch}/${box}-${job}/select.json" | head -10 | sed 's/^/    /'
			failures=$((failures + 1))
		fi
	done
done
echo "failures: ${failures}"
exit $((failures > 0))
