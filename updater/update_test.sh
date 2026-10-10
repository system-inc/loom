#!/bin/bash
# update_test.sh [<loom-update.sh>]: the updater against a local web server (python3's http.server), fed by the real
# publish.sh (with a stub go) and upload.sh, one PASS or FAIL per check. Machines are fake homes on this machine;
# nothing reaches ~/.loom. The updater itself needs no python3: only the server and the report checks here use it.
# Runs on Linux and on macOS's bash 3.2:
#
#	updater/update_test.sh
#	sed 's/\[ "${got}" = "${sha}" \] ||/true ||/' loom-update.sh > m.sh && updater/update_test.sh m.sh                     # fails 2: a corrupted blob installs
#	sed 's/alive "${holder}" \&\& return 1/true/' loom-update.sh > m.sh && updater/update_test.sh m.sh                     # fails 1: runs overlap
#	sed 's/mkdir "${lock}\/takeover-${holder}" 2> \/dev\/null || return 1/return 1/' loom-update.sh > m.sh && updater/update_test.sh m.sh   # fails 3: a stale lock holds forever
#	sed 's/ \&\& ps -p .*$//' loom-update.sh > m.sh && updater/update_test.sh m.sh                                        # fails 3: a reused pid holds the lock
set -u
here=$(cd "$(dirname "$0")" && pwd) failures=0
updater=$(cd "$(dirname "${1:-${here}/loom-update.sh}")" && pwd)/$(basename "${1:-${here}/loom-update.sh}")
T=$(mktemp -d)
check() { if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; sed 's/^/    /' "${T}/run.log" 2> /dev/null; failures=$((failures + 1)); fi; }
case "$(uname -s)" in Linux) system=linux ;; *) system=darwin ;; esac
case "$(uname -m)" in x86_64 | amd64) architecture=amd64 ;; *) architecture=arm64 ;; esac
platform=${system}/${architecture} platforms="linux/amd64 darwin/arm64"
case " ${platforms} " in *" ${platform} "*) ;; *) platforms="${platforms} ${platform}" ;; esac
export LOOM_PUBLISH_PLATFORMS=${platforms}

# A source repository shaped like Loom's: each binary's package holds a stamp, and the stub go's "binary" is that
# stamp and the platform, so a commit changes exactly the binaries whose stamp it changes.
mkdir -p "${T}/bin" "${T}/source/updater" "${T}/source/cmd/loom" "${T}/source/runner/cmd/loom-runner" "${T}/source/pilots/adamic-gate"
cat > "${T}/bin/go" << 'STUB'
#!/bin/bash
while [ $# -gt 1 ]; do [ "$1" = -o ] && output=$2; shift; done
printf '%s %s/%s\n' "$(cat "$1/stamp")" "${GOOS}" "${GOARCH}" > "${output}"
STUB
chmod +x "${T}/bin/go"
export PATH=${T}/bin:${PATH}
cp "${here}/publish.sh" "${here}/upload.sh" "${T}/source/updater/"
git -C "${T}/source" init -q
commit() { # commit <loom stamp> <runner stamp> <gate stamp>: the source's new commit
	echo "loom $1" > "${T}/source/cmd/loom/stamp"
	echo "runner $2" > "${T}/source/runner/cmd/loom-runner/stamp"
	echo "gate $3" > "${T}/source/pilots/adamic-gate/stamp"
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
now() { cat "${T}/$1/.loom/$2" 2> /dev/null; } # now <home> <version | previous>
runs() { cat "${T}/$1/.loom/bin/$2"; }        # runs <home> <name>: what ~/.loom/bin/<name> is
blobs() { grep -c '"GET /blobs/' "${T}/access.log"; }
code() { [ "$(cat "${T}/code")" = "$1" ]; }

a=$(commit 1 1 1)
publish "${a}"
hook workshop
update workshop Workshop /report
check first-install 'code 0 && [ "$(now workshop version)" = "${a}" ] && [ "$(runs workshop loom-runner)" = "runner 1 ${platform}" ] && [ -x ${T}/workshop/.loom/bin/adamic-gate ] && [ "$(cat ${T}/workshop.hooks)" = "${a} none 1" ] && [ ! -e ${T}/workshop/.loom/previous ] && [ "$(blobs)" = 3 ]'
check first-install-links '[ "$(readlink ${T}/workshop/.loom/bin/loom)" = "../versions/${a}/loom" ] && [ ! -e ${T}/workshop/.loom/update.lock ]'
check first-install-reports 'python3 -c "import json,sys; r=[json.loads(l) for l in open(\"${T}/reports\")]; sys.exit(0 if len(r) == 1 and (r[0][\"host\"], r[0][\"version\"], r[0][\"previous\"]) == (\"Workshop\", \"${a}\", \"\") and r[0][\"at\"].endswith(\"Z\") else 1)" && [ "$(cat ${T}/workshop/.loom/reported)" = "${a}" ]'

lines=$(wc -l < "${T}/workshop/.loom/update.log")
update workshop Workshop /report
check no-op-rerun 'code 0 && [ "$(now workshop version)" = "${a}" ] && [ "$(blobs)" = 3 ] && [ $(wc -l < ${T}/workshop.hooks) -eq 1 ] && [ $(wc -l < ${T}/workshop/.loom/update.log) -eq ${lines} ] && [ $(wc -l < ${T}/reports) -eq 1 ]'

b=$(commit 1 2 1)
publish "${b}"
update workshop Workshop /report
check update-swaps-and-runs-hooks 'code 0 && [ "$(now workshop version)" = "${b}" ] && [ "$(now workshop previous)" = "${a}" ] && [ "$(tail -1 ${T}/workshop.hooks)" = "${b} ${a} 2" ] && [ "$(runs workshop loom-runner)" = "runner 2 ${platform}" ] && [ "$(readlink ${T}/workshop/.loom/bin/loom)" = "../versions/${b}/loom" ]'
check update-downloads-only-changes '[ "$(blobs)" = 4 ] && grep -q "installed ${b}, previous ${a} (1 downloaded, 2 linked)" ${T}/workshop/.loom/update.log && [ ${T}/workshop/.loom/versions/${a}/loom -ef ${T}/workshop/.loom/versions/${b}/loom ]'

c=$(commit 1 3 1)
publish "${c}"
corrupt=$(awk -v platform="${platform}" '$1 == "loom-runner" && $2 == platform { print $3 }' "${T}/out/current.txt")
echo "runner evil ${platform}" > "${T}/www/blobs/${corrupt}"
update workshop Workshop /report
check corrupted-blob-refused 'code 1 && grep -q "refused: loom-runner from .*/blobs/${corrupt} hashes to " ${T}/run.log && [ "$(now workshop version)" = "${b}" ] && [ "$(now workshop previous)" = "${a}" ]'
check corrupted-blob-installs-nothing '[ ! -e ${T}/workshop/.loom/versions/${c} ] && [ -z "$(ls -A ${T}/workshop/.loom/versions | grep -v -e "^${a}$" -e "^${b}$")" ] && [ $(wc -l < ${T}/workshop.hooks) -eq 2 ] && [ "$(runs workshop loom-runner)" = "runner 2 ${platform}" ] && [ ! -e ${T}/workshop/.loom/update.lock ]'
publish "${c}"
update workshop Workshop /report
check repaired-blob-installs-and-prunes 'code 0 && [ "$(now workshop version)" = "${c}" ] && [ "$(now workshop previous)" = "${b}" ] && [ ! -e ${T}/workshop/.loom/versions/${a} ] && [ $(ls ${T}/workshop/.loom/versions | wc -l) -eq 2 ]'

d=$(commit 4 4 1)
publish --canary Cloud,Sun1 "${d}"
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

# A machine's own settings come from update.conf when the environment names none.
mkdir -p "${T}/sun2/.loom" && printf '# the release store\nbase = %s/\nhost=Sun2\n' "${base}" > "${T}/sun2/.loom/update.conf"
HOME=${T}/sun2 "${updater}" > "${T}/run.log" 2>&1
echo $? > "${T}/code"
check settings-from-update-conf 'code 0 && [ "$(now sun2 version)" = "${b}" ] && grep -q " Sun2 installed ${b}" ${T}/sun2/.loom/update.log'

# Overlap: a run held in its hook keeps the lock; a second run beside it changes nothing. The hook leaves a service
# running, which must not keep the lock once the first run ends.
e=$(commit 5 5 5)
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

echo "failures: ${failures} (scratch ${T})"
exit $((failures > 0))
