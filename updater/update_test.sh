#!/bin/bash
# update_test.sh [<loom-update.sh>]: the updater against a local web server (python3's http.server), fed by the real
# publish.sh (with a stub go) and upload.sh, one PASS or FAIL per check; then publish.sh's guards and pruning, and
# upload.sh's R2 path against a stub curl. Machines are fake homes on this machine; nothing reaches ~/.loom or any
# network but 127.0.0.1. The updater itself needs no python3: only the server and the report checks here use it.
# Runs on Linux and on macOS's bash 3.2:
#
#	updater/update_test.sh
#	sed 's/\[ "${got}" = "${sha}" \] ||/true ||/' loom-update.sh > m.sh && updater/update_test.sh m.sh                     # fails 2: a corrupted blob installs
#	sed 's/alive "${holder}" \&\& return 1/true/' loom-update.sh > m.sh && updater/update_test.sh m.sh                     # fails 1: runs overlap
#	sed 's/mkdir "${lock}\/takeover-${holder}" 2> \/dev\/null || return 1/return 1/' loom-update.sh > m.sh && updater/update_test.sh m.sh   # fails 3: a stale lock holds forever
#	sed 's/ \&\& ps -p .*$//' loom-update.sh > m.sh && updater/update_test.sh m.sh                                        # fails 3: a reused pid holds the lock
#	sed 's/\[ "$(current hooked)" = "${version}" \] ||/true ||/' loom-update.sh > m.sh && updater/update_test.sh m.sh      # fails 2: a failed hook never runs again
#	sed 's/|| !ended\[sections - 1\]) fail/) fail/' loom-update.sh > m.sh && updater/update_test.sh m.sh                   # fails 3: a cut manifest installs
#	sed 's/ \&\&$/ \&\& true ||/' loom-update.sh > m.sh && updater/update_test.sh m.sh                                        # fails 1: a corrupt house cache blob installs
#	sed 's#"${base}/current.txt"#"${house:-${base}}/current.txt"#' loom-update.sh > m.sh && updater/update_test.sh m.sh    # fails 4: the house cache's stale current.txt
#	sed 's/\[ -n "${asking}" \] \&\& say/[ -n "${asking}" ] \&\& refuse/' loom-update.sh > m.sh && updater/update_test.sh m.sh # fails 2: no fallback past the house cache
#	sed 's/ \&\& asking=$//' loom-update.sh > m.sh && updater/update_test.sh m.sh                                         # fails 2: a failed house cache asked for every file
set -u
here=$(cd "$(dirname "$0")" && pwd) failures=0
updater=$(cd "$(dirname "${1:-${here}/loom-update.sh}")" && pwd)/$(basename "${1:-${here}/loom-update.sh}")
T=$(mktemp -d)
check() { if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; sed 's/^/    /' "${T}/run.log" 2> /dev/null; failures=$((failures + 1)); fi; }
case "$(uname -s)" in Linux) system=linux ;; *) system=darwin ;; esac
case "$(uname -m)" in x86_64 | amd64) architecture=amd64 ;; *) architecture=arm64 ;; esac
platform=${system}/${architecture} platforms="linux/amd64 darwin/arm64"
case " ${platforms} " in *" ${platform} "*) ;; *) platforms="${platforms} ${platform}" ;; esac
export LOOM_PUBLISH_PLATFORMS=${platforms} LOOM_PUBLISH_FLOOR_GB=0
unset LOOM_PUBLISH_GO LOOM_PUBLISH_KEEP

# A source repository shaped like Loom's: each binary's package holds a stamp, and the stub go's "binary" is that
# stamp and the platform, so a commit changes exactly the binaries whose stamp it changes. The stub reports itself as
# STUB_GO_VERSION (go1.27.1, what the source's go.mod names) and its caches under the scratch directory.
mkdir -p "${T}/bin" "${T}/source/updater" "${T}/source/cmd/loom" "${T}/source/runner/cmd/loom-runner"
cat > "${T}/bin/go" << 'STUB'
#!/bin/bash
case "$1" in
version) echo "go version ${STUB_GO_VERSION:-go1.27.1} ${GOOS:-stub}/${GOARCH:-stub}"; exit 0 ;;
env) echo "${TMPDIR:-/tmp}/stub-gocache"; echo "${TMPDIR:-/tmp}/stub-gomodcache"; exit 0 ;;
esac
while [ $# -gt 1 ]; do [ "$1" = -o ] && output=$2; shift; done
printf '%s %s/%s\n' "$(cat "$1/stamp")" "${GOOS}" "${GOARCH}" > "${output}"
STUB
printf '#!/bin/bash\nSTUB_GO_VERSION=go1.27.0 exec %s "$@"\n' "${T}/bin/go" > "${T}/go-old"
chmod +x "${T}/bin/go" "${T}/go-old"
export PATH=${T}/bin:${PATH}
cp "${here}/publish.sh" "${here}/upload.sh" "${T}/source/updater/"
printf 'module github.com/system-inc/loom\n\ngo 1.27.0\n\ntoolchain go1.27.1\n' > "${T}/source/go.mod"
git -C "${T}/source" init -q
commit() { # commit <loom stamp> <runner stamp>: the source's new commit
	echo "loom $1" > "${T}/source/cmd/loom/stamp"
	echo "runner $2" > "${T}/source/runner/cmd/loom-runner/stamp"
	git -C "${T}/source" add -A && git -C "${T}/source" -c user.name=test -c user.email=test@example.com -c core.hooksPath=/dev/null \
		-c commit.gpgsign=false commit -q -m "stamps $*"
	git -C "${T}/source" rev-parse HEAD
}
publish() { # publish [--canary <hosts>] <commit>: publish.sh then upload.sh into the served directory
	"${T}/source/updater/publish.sh" "$@" "${T}/out" > /dev/null && "${T}/source/updater/upload.sh" "${T}/out" "${T}/www" > /dev/null
}
republish() { # republish <commit>: that commit's kept manifest becomes current.txt again, as a rollback is done
	cp "${T}/out/manifests/$1.txt" "${T}/out/current.txt" && "${T}/source/updater/upload.sh" "${T}/out" "${T}/www" > /dev/null
}
serve() { cp "$1" "${T}/www/current.txt"; } # serve <file>: what the server hands out as current.txt, as it is

# The web server: python3's http.server on the served directory, plus a POST /report that keeps each body.
mkdir -p "${T}/www"
cat > "${T}/server.py" << 'PYTHON'
import functools, http.server, sys
www, reports, port_file = sys.argv[1:]
class Handler(http.server.SimpleHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("content-length", 0)))
        if self.path != "/report":
            self.send_error(404)
            return
        with open(reports, "ab") as handle:
            handle.write(body + b"\n")
        self.send_response(204)
        self.end_headers()
    def log_message(self, format, *arguments):
        sys.stderr.write("%s\n" % (format % arguments))
server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), functools.partial(Handler, directory=www))
open(port_file, "w").write(str(server.server_address[1]))
server.serve_forever()
PYTHON
python3 "${T}/server.py" "${T}/www" "${T}/reports" "${T}/port" 2> "${T}/access.log" &
server=$!
trap 'kill ${server} 2> /dev/null' EXIT
for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do [ -s "${T}/port" ] && break; sleep 0.2; done
base=http://127.0.0.1:$(cat "${T}/port")

# update <home> <host> [report path]: one updater run as that machine; its exit code lands in ${T}/code.
update() {
	HOME=${T}/$1 LOOM_UPDATE_HOST=$2 LOOM_UPDATE_BASE=${base} LOOM_UPDATE_REPORT=${3:+${base}$3} "${updater}" > "${T}/run.log" 2>&1
	echo $? > "${T}/code"
}
hook() { # hook <home>: an updated.d hook that records each run's version, previous and the runner it sees
	mkdir -p "${T}/$1/.loom/updated.d"
	printf '#!/bin/bash\necho "${LOOM_UPDATE_VERSION} ${LOOM_UPDATE_PREVIOUS:-none} $(cut -d" " -f2 "${LOOM_UPDATE_BIN}/loom-runner")" >> "%s"\n' "${T}/$1.hooks" > "${T}/$1/.loom/updated.d/10-restart"
	chmod +x "${T}/$1/.loom/updated.d/10-restart"
}
now() { cat "${T}/$1/.loom/$2" 2> /dev/null; } # now <home> <version | previous | hooked>
runs() { cat "${T}/$1/.loom/bin/$2"; }        # runs <home> <name>: what ~/.loom/bin/<name> is
blobs() { grep -c '"GET /blobs/' "${T}/access.log"; }
code() { [ "$(cat "${T}/code")" = "$1" ]; }

a=$(commit 1 1)
publish "${a}"
hook workshop
update workshop Workshop /report
check first-install 'code 0 && [ "$(now workshop version)" = "${a}" ] && [ "$(runs workshop loom-runner)" = "runner 1 ${platform}" ] && [ -x ${T}/workshop/.loom/bin/loom ] && [ "$(cat ${T}/workshop.hooks)" = "${a} none 1" ] && [ ! -e ${T}/workshop/.loom/previous ] && [ "$(blobs)" = 2 ] && [ "$(now workshop hooked)" = "${a}" ]'
check first-install-links '[ "$(readlink ${T}/workshop/.loom/bin/loom)" = "../versions/${a}/loom" ] && [ ! -e ${T}/workshop/.loom/update.lock ] && [ "$(tail -1 ${T}/out/current.txt)" = "end $(awk "NF == 3" ${T}/out/current.txt | wc -l | tr -d " ")" ]'
check first-install-reports 'python3 -c "import json,sys; r=[json.loads(l) for l in open(\"${T}/reports\")]; sys.exit(0 if len(r) == 1 and (r[0][\"host\"], r[0][\"version\"], r[0][\"previous\"]) == (\"Workshop\", \"${a}\", \"\") and r[0][\"at\"].endswith(\"Z\") else 1)" && [ "$(cat ${T}/workshop/.loom/reported)" = "${a}" ]'

lines=$(wc -l < "${T}/workshop/.loom/update.log")
update workshop Workshop /report
check no-op-rerun 'code 0 && [ "$(now workshop version)" = "${a}" ] && [ "$(blobs)" = 2 ] && [ $(wc -l < ${T}/workshop.hooks) -eq 1 ] && [ $(wc -l < ${T}/workshop/.loom/update.log) -eq ${lines} ] && [ $(wc -l < ${T}/reports) -eq 1 ]'

b=$(commit 1 2)
publish "${b}"
update workshop Workshop /report
check update-swaps-and-runs-hooks 'code 0 && [ "$(now workshop version)" = "${b}" ] && [ "$(now workshop previous)" = "${a}" ] && [ "$(tail -1 ${T}/workshop.hooks)" = "${b} ${a} 2" ] && [ "$(runs workshop loom-runner)" = "runner 2 ${platform}" ] && [ "$(readlink ${T}/workshop/.loom/bin/loom)" = "../versions/${b}/loom" ]'
check update-downloads-only-changes '[ "$(blobs)" = 3 ] && grep -q "installed ${b}, previous ${a} (1 downloaded, 1 linked)" ${T}/workshop/.loom/update.log && [ ${T}/workshop/.loom/versions/${a}/loom -ef ${T}/workshop/.loom/versions/${b}/loom ]'

# A manifest cut short at a line boundary still parses line by line; its missing end line refuses it whole.
c=$(commit 1 3)
publish "${c}"
sed '$d' "${T}/out/current.txt" > "${T}/cut.txt"
serve "${T}/cut.txt"
update workshop Workshop /report
check cut-manifest-refused 'code 1 && grep -q "cut short" ${T}/run.log && [ "$(now workshop version)" = "${b}" ] && [ ! -e ${T}/workshop/.loom/versions/${c} ] && [ "$(readlink ${T}/workshop/.loom/bin/loom-runner)" = "../versions/${b}/loom-runner" ] && [ $(wc -l < ${T}/workshop.hooks) -eq 2 ]'
serve "${T}/out/current.txt"
corrupt=$(awk -v platform="${platform}" '$1 == "loom-runner" && $2 == platform { print $3 }' "${T}/out/current.txt")
echo "runner evil ${platform}" > "${T}/www/blobs/${corrupt}"
update workshop Workshop /report
check corrupted-blob-refused 'code 1 && grep -q "refused: loom-runner from .*/blobs/${corrupt} hashes to " ${T}/run.log && [ "$(now workshop version)" = "${b}" ] && [ "$(now workshop previous)" = "${a}" ]'
check corrupted-blob-installs-nothing '[ ! -e ${T}/workshop/.loom/versions/${c} ] && [ -z "$(ls -A ${T}/workshop/.loom/versions | grep -v -e "^${a}$" -e "^${b}$")" ] && [ $(wc -l < ${T}/workshop.hooks) -eq 2 ] && [ "$(runs workshop loom-runner)" = "runner 2 ${platform}" ] && [ ! -e ${T}/workshop/.loom/update.lock ]'
publish "${c}"
update workshop Workshop /report
check repaired-blob-installs-and-prunes 'code 0 && [ "$(now workshop version)" = "${c}" ] && [ "$(now workshop previous)" = "${b}" ] && [ ! -e ${T}/workshop/.loom/versions/${a} ] && [ $(ls ${T}/workshop/.loom/versions | wc -l) -eq 2 ]'

d=$(commit 4 4)
publish --canary Cloud,Sun1 "${d}"
check canary-line-first '[ "$(head -1 ${T}/out/current.txt)" = "canary Cloud Sun1" ] && [ $(grep -c "^end " ${T}/out/current.txt) -eq 2 ]'
# Cut after the top section's end line, a canary manifest reads whole but for its promised second section.
awk '$1 == "end" { print; exit } { print }' "${T}/out/current.txt" > "${T}/cut.txt"
serve "${T}/cut.txt"
update cloud cloud /report
check cut-canary-refused 'code 1 && grep -q "cut short" ${T}/run.log && [ -z "$(now cloud version)" ]'
serve "${T}/out/current.txt"
update workshop Workshop /report
check canary-skips-others 'code 0 && [ "$(now workshop version)" = "${c}" ]'
update cloud cloud /report
check canary-host-gets-canary 'code 0 && [ "$(now cloud version)" = "${d}" ] && [ "$(runs cloud loom)" = "loom 4 ${platform}" ]'
update server Server /report
check canary-others-get-top-level 'code 0 && [ "$(now server version)" = "${c}" ]'

republish "${b}"
before=$(blobs)
update workshop Workshop /report
check rollback-one-step 'code 0 && [ "$(now workshop version)" = "${b}" ] && [ "$(now workshop previous)" = "${c}" ] && [ "$(blobs)" = "${before}" ] && [ "$(tail -1 ${T}/workshop.hooks)" = "${b} ${c} 2" ] && grep -q "installed ${b}, previous ${c} (already on disk)" ${T}/workshop/.loom/update.log && [ "$(runs workshop loom-runner)" = "runner 2 ${platform}" ]'
update cloud cloud /report
check rollback-ends-the-canary 'code 0 && [ "$(now cloud version)" = "${b}" ]'

# A report that fails never fails the update, and the next run sends it.
update home Home /nowhere
check report-failure-is-not-fatal 'code 0 && [ "$(now home version)" = "${b}" ] && grep -q "report of ${b} to .* failed" ${T}/home/.loom/update.log && [ ! -e ${T}/home/.loom/reported ]'
update home Home /report
check report-retried 'code 0 && [ "$(cat ${T}/home/.loom/reported)" = "${b}" ] && [ "$(tail -1 ${T}/reports | python3 -c "import json,sys; print(json.load(sys.stdin)[\"host\"])")" = Home ]'

# A hook that fails runs again on the next run, which finds the version installed, and stops once it passes.
mkdir -p "${T}/flaky/.loom/updated.d"
printf '#!/bin/bash\necho "${LOOM_UPDATE_VERSION}" >> %s\n[ -e %s ] && exit 0\ntouch %s\nexit 3\n' "${T}/flaky.runs" "${T}/flaky.once" "${T}/flaky.once" > "${T}/flaky/.loom/updated.d/10-flaky"
chmod +x "${T}/flaky/.loom/updated.d/10-flaky"
update flaky Flaky
check failed-hook-fails-the-run 'code 1 && [ "$(now flaky version)" = "${b}" ] && [ -z "$(now flaky hooked)" ] && grep -q "hook 10-flaky exited 3 for ${b}" ${T}/flaky/.loom/update.log'
update flaky Flaky
check failed-hook-runs-again 'code 0 && [ $(wc -l < ${T}/flaky.runs) -eq 2 ] && [ "$(tail -1 ${T}/flaky.runs)" = "${b}" ] && [ "$(now flaky hooked)" = "${b}" ]'
update flaky Flaky
check passed-hook-stops 'code 0 && [ $(wc -l < ${T}/flaky.runs) -eq 2 ]'

# update.log past 1 MB keeps its last 2000 lines.
mkdir -p "${T}/logs/.loom"
awk 'BEGIN { for (i = 1; i <= 3000; i++) printf "filler %d %400s\n", i, "x" }' > "${T}/logs/.loom/update.log"
update logs Logs
check log-capped 'code 0 && [ "$(head -1 ${T}/logs/.loom/update.log | cut -d" " -f1-2)" = "filler 1001" ] && [ $(wc -l < ${T}/logs/.loom/update.log) -eq 2001 ] && tail -1 ${T}/logs/.loom/update.log | grep -q "installed ${b}" && [ $(wc -c < ${T}/logs/.loom/update.log) -lt 1048576 ]'

# A machine's own settings come from update.conf when the environment names none.
mkdir -p "${T}/sun2/.loom" && printf '# the release store\nbase = %s/\nhost=Sun2\n' "${base}" > "${T}/sun2/.loom/update.conf"
HOME=${T}/sun2 "${updater}" > "${T}/run.log" 2>&1
echo $? > "${T}/code"
check settings-from-update-conf 'code 0 && [ "$(now sun2 version)" = "${b}" ] && grep -q " Sun2 installed ${b}" ${T}/sun2/.loom/update.log'

# Overlap: a run held in its hook keeps the lock; a second run beside it changes nothing. The hook leaves a service
# running, which must not keep the lock once the first run ends.
e=$(commit 5 5)
publish "${e}"
printf '#!/bin/bash\n[ -e %s ] && exit 0\nsleep 60 &\necho $! > %s\ntouch %s\nwhile [ ! -e %s ]; do sleep 0.1; done\n' "${T}/held" "${T}/service" "${T}/held" "${T}/release" > "${T}/workshop/.loom/updated.d/20-slow"
chmod +x "${T}/workshop/.loom/updated.d/20-slow"
(HOME=${T}/workshop LOOM_UPDATE_HOST=Workshop LOOM_UPDATE_BASE=${base} "${updater}" > "${T}/first.log" 2>&1; echo $? > "${T}/first.code") &
for i in $(seq 1 100); do [ -e "${T}/held" ] && break; sleep 0.1; done
republish "${c}"
update workshop Workshop /report
check overlapping-run-blocked 'code 0 && grep -q "another run holds" ${T}/run.log && [ "$(now workshop version)" = "${e}" ]'
touch "${T}/release"
for i in $(seq 1 100); do [ -e "${T}/first.code" ] && break; sleep 0.1; done
check first-run-finishes '[ "$(cat ${T}/first.code)" = 0 ]'
rm -f "${T}/workshop/.loom/updated.d/20-slow"
update workshop Workshop /report
check lock-released-with-service-running 'code 0 && [ "$(now workshop version)" = "${c}" ] && kill -0 $(cat ${T}/service) 2> /dev/null'
kill "$(cat "${T}/service")" 2> /dev/null

# Stale locks: a run killed holding the lock left its pid behind. A pid no longer running, a pid since reused by
# something that isn't loom-update, and a lock older than a minute with no pid are each taken over.
stale() { # stale [pid]: a lock as a killed run left it on Workshop
	mkdir "${T}/workshop/.loom/update.lock"
	[ -z "${1:-}" ] || echo "$1" > "${T}/workshop/.loom/update.lock/pid"
}
sleep 0 & dead=$!
wait "${dead}"
stale "${dead}"
republish "${e}"
update workshop Workshop /report
check stale-lock-taken-over 'code 0 && [ "$(now workshop version)" = "${e}" ] && grep -q "took over the lock of run ${dead}" ${T}/workshop/.loom/update.log && [ ! -e ${T}/workshop/.loom/update.lock ]'
sleep 60 & other=$!
stale "${other}"
republish "${c}"
update workshop Workshop /report
check reused-pid-lock-taken-over 'code 0 && [ "$(now workshop version)" = "${c}" ] && grep -q "took over the lock of run ${other}" ${T}/workshop/.loom/update.log'
{ kill "${other}" && wait "${other}"; } 2> /dev/null
stale ""
republish "${e}"
update workshop Workshop /report
check new-pidless-lock-holds 'code 0 && grep -q "another run holds" ${T}/run.log && [ "$(now workshop version)" = "${c}" ]'
touch -t 202001010000 "${T}/workshop/.loom/update.lock"
update workshop Workshop /report
check old-pidless-lock-taken-over 'code 0 && [ "$(now workshop version)" = "${e}" ] && grep -q "took over the lock of run none" ${T}/workshop/.loom/update.log && [ ! -e ${T}/workshop/.loom/update.lock ]'

# The house cache (docs/house-cache.md): each blob is asked of it first, at its address and the base's path, and taken
# only whole and hashing to its name; current.txt is always the base's. Here it is a second server on a copy of the
# served blobs, holding a stale current.txt that names an older release.
mkdir -p "${T}/house/blobs"
cp "${T}/www/blobs/"* "${T}/house/blobs/"
cp "${T}/out/manifests/${c}.txt" "${T}/house/current.txt"
python3 "${T}/server.py" "${T}/house" "${T}/house-reports" "${T}/house-port" 2> "${T}/house-access.log" &
houseServer=$!
for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do [ -s "${T}/house-port" ] && break; sleep 0.2; done
house=http://127.0.0.1:$(cat "${T}/house-port")
housed() { # housed <home>: one updater run as that machine, its update.conf naming the house cache
	mkdir -p "${T}/$1/.loom" && printf 'base = %s\nhouse-cache = %s/\n' "${base}" "${house}" > "${T}/$1/.loom/update.conf"
	HOME=${T}/$1 LOOM_UPDATE_HOST=$1 "${updater}" > "${T}/run.log" 2>&1
	echo $? > "${T}/code"
}
before=$(blobs)
housed chonchon
check house-cache-gives-the-blobs 'code 0 && [ "$(now chonchon version)" = "${e}" ] && [ "$(blobs)" = "${before}" ] && [ $(grep -c "\"GET /blobs/" ${T}/house-access.log) -eq 2 ] && grep -q "installed ${e}, previous none (2 downloaded (2 through the house cache), 0 linked)" ${T}/chonchon/.loom/update.log'
check house-cache-never-gives-current '! grep -q current.txt ${T}/house-access.log && [ "$(runs chonchon loom-runner)" = "runner 5 ${platform}" ]'
houseRunner=$(awk -v platform="${platform}" '$1 == "loom-runner" && $2 == platform { print $3 }' "${T}/out/manifests/${e}.txt")
echo "runner evil ${platform}" > "${T}/house/blobs/${houseRunner}"
before=$(blobs)
housed corrupted
# The files go in sha256 order: the corrupt one, and every one after it, come from the base.
fromBase=$(LC_ALL=C awk -v platform="${platform}" -v bad="${houseRunner}" 'NF == 3 && $2 == platform && $3 >= bad' "${T}/out/manifests/${e}.txt" | wc -l | tr -d ' ')
check house-cache-corrupt-blob-refused 'code 0 && [ "$(now corrupted version)" = "${e}" ] && [ "$(runs corrupted loom-runner)" = "runner 5 ${platform}" ] && [ "$(blobs)" = $((before + fromBase)) ] && grep -q "the house cache .* didn.t give loom-runner whole" ${T}/corrupted/.loom/update.log && grep -q "($((2 - fromBase)) through the house cache)" ${T}/corrupted/.loom/update.log'
kill "${houseServer}" 2> /dev/null
wait "${houseServer}" 2> /dev/null
before=$(blobs)
housed downstairs
check house-cache-down-falls-back 'code 0 && [ "$(now downstairs version)" = "${e}" ] && [ "$(blobs)" = $((before + 2)) ] && [ $(grep -c "the house cache .* didn.t give" ${T}/downstairs/.loom/update.log) -eq 1 ]'

# publish.sh's guards: the wrong compiler, too little disk, and a Go main it doesn't ship each refuse before any
# build, and change nothing in the out directory.
f=$(commit 6 6)
cp "${T}/out/current.txt" "${T}/current-before.txt"
LOOM_PUBLISH_GO=${T}/go-old "${T}/source/updater/publish.sh" "${f}" "${T}/out" > "${T}/run.log" 2>&1
echo $? > "${T}/code"
check publish-refuses-other-toolchain 'code 1 && grep -q "is go1.27.0, and .*go.mod names go1.27.1" ${T}/run.log && [ ! -e ${T}/out/manifests/${f}.txt ] && cmp -s ${T}/out/current.txt ${T}/current-before.txt'
LOOM_PUBLISH_FLOOR_GB=999999 "${T}/source/updater/publish.sh" "${f}" "${T}/out" > "${T}/run.log" 2>&1
echo $? > "${T}/code"
check publish-refuses-under-floor 'code 1 && grep -q "GB free, under the 999999 GB floor" ${T}/run.log && [ ! -e ${T}/out/manifests/${f}.txt ] && cmp -s ${T}/out/current.txt ${T}/current-before.txt'
mkdir -p "${T}/source/cmd/extra" && printf 'package main\n\nfunc main() {}\n' > "${T}/source/cmd/extra/main.go"
x=$(commit 6 6)
"${T}/source/updater/publish.sh" "${x}" "${T}/out" > "${T}/run.log" 2>&1
echo $? > "${T}/code"
check publish-refuses-unshipped-main 'code 1 && grep -q "cmd/extra is a Go main publish.sh doesn.t ship" ${T}/run.log && [ ! -e ${T}/out/manifests/${x}.txt ]'
git -C "${T}/source" rm -q "cmd/extra/main.go"
f=$(commit 6 6)
"${T}/source/updater/publish.sh" "${f}" "${T}/out" > "${T}/run.log" 2>&1
echo $? > "${T}/code"
check publish-names-its-compiler 'code 0 && grep -q "built with go1.27.1" ${T}/run.log'

# Pruning: the out directory keeps the manifests current.txt names and the newest LOOM_PUBLISH_KEEP others, and only
# the blobs they name. The served copy loses nothing: upload.sh never deletes.
runnerOf() { awk -v platform="${platform}" '$1 == "loom-runner" && $2 == platform { print $3 }' "${T}/out/manifests/$1.txt"; }
oldRunner=$(runnerOf "${e}")
g=$(commit 7 7)
LOOM_PUBLISH_KEEP=1 publish "${g}"
check prune-keeps-newest '[ "$(ls ${T}/out/manifests)" = "${g}.txt" ] && [ ! -e ${T}/out/blobs/${oldRunner} ] && [ -f ${T}/out/blobs/$(runnerOf ${g}) ] && [ $(ls ${T}/out/blobs | wc -l) -eq $(awk "NF == 3" ${T}/out/current.txt | wc -l) ] && [ -f ${T}/www/blobs/${oldRunner} ]'
h=$(commit 8 8)
LOOM_PUBLISH_KEEP=1 publish --canary Cloud "${h}"
check prune-keeps-what-current-names '[ $(ls ${T}/out/manifests | wc -l) -eq 2 ] && [ -f ${T}/out/manifests/${g}.txt ] && [ -f ${T}/out/blobs/$(runnerOf ${g}) ] && [ -f ${T}/out/blobs/$(runnerOf ${h}) ]'

# upload.sh to R2: a stub curl records each call's arguments and its standard input. Blobs go first, then the named
# manifests, current.txt last; each URL is R2's S3 endpoint under the prefix, signed by curl, with its cache header;
# and the secret reaches curl only on standard input.
mkdir -p "${T}/stubcurl"
printf '#!/bin/bash\nprintf "%%s\\n" "$*" >> %s\ncat >> %s\n' "${T}/curl.calls" "${T}/curl.stdin" > "${T}/stubcurl/curl"
chmod +x "${T}/stubcurl/curl"
printf 'account_id = acct0123\naccess_key_id=key0123\nsecret_access_key = secret-never-in-argv-0123\n' > "${T}/r2.conf"
PATH=${T}/stubcurl:${PATH} LOOM_UPLOAD_CREDENTIALS=${T}/r2.conf "${T}/source/updater/upload.sh" "${T}/out" r2:loom-artifacts/releases > "${T}/run.log" 2>&1
echo $? > "${T}/code"
endpoint=https://acct0123.r2.cloudflarestorage.com/loom-artifacts/releases
awk '{ print $NF }' "${T}/curl.calls" > "${T}/curl.urls"
blobCount=$(awk 'NF == 3 && $1 != "canary" { print $3 }' "${T}/out/current.txt" | sort -u | wc -l | tr -d ' ')
check r2-order 'code 0 && [ $(wc -l < ${T}/curl.urls) -eq $((blobCount + 3)) ] && [ $(head -n ${blobCount} ${T}/curl.urls | grep -c "^${endpoint}/blobs/[0-9a-f]\{64\}$") -eq ${blobCount} ] && [ "$(sed -n "$((blobCount + 1))p" ${T}/curl.urls)" = "${endpoint}/manifests/${g}.txt" ] && [ "$(sed -n "$((blobCount + 2))p" ${T}/curl.urls)" = "${endpoint}/manifests/${h}.txt" ]'
check r2-current-last '[ "$(tail -1 ${T}/curl.urls)" = "${endpoint}/current.txt" ] && tail -1 ${T}/curl.calls | grep -q "Cache-Control: no-cache"'
check r2-blob-cache-control '[ $(grep "/blobs/" ${T}/curl.calls | grep -c "Cache-Control: public, max-age=31536000, immutable") -eq ${blobCount} ]'
check r2-signed-put '[ $(grep -c -- "--aws-sigv4 aws:amz:auto:s3 -K - -X PUT -T " ${T}/curl.calls) -eq $(wc -l < ${T}/curl.calls) ]'
check r2-secret-off-argv '! grep -q secret-never-in-argv ${T}/curl.calls && [ $(grep -c "^user = \"key0123:secret-never-in-argv-0123\"$" ${T}/curl.stdin) -eq $(wc -l < ${T}/curl.calls) ]'
: > "${T}/curl.calls"
PATH=${T}/stubcurl:${PATH} LOOM_UPLOAD_CREDENTIALS=${T}/missing.conf "${T}/source/updater/upload.sh" "${T}/out" r2:loom-artifacts/releases > "${T}/run.log" 2>&1
echo $? > "${T}/code"
check r2-needs-credentials 'code 2 && grep -q "needs account_id" ${T}/run.log && [ ! -s ${T}/curl.calls ]'
PATH=${T}/stubcurl:${PATH} LOOM_UPLOAD_CREDENTIALS=${T}/r2.conf "${T}/source/updater/upload.sh" "${T}/out" r2:loom-artifacts > "${T}/run.log" 2>&1
echo $? > "${T}/code"
check r2-needs-prefix 'code 2 && [ ! -s ${T}/curl.calls ]'

echo "failures: ${failures} (scratch ${T})"
exit $((failures > 0))
