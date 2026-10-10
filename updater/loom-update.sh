#!/bin/bash
# loom-update.sh: brings this machine's Loom binaries to what the published manifest names for it (docs/updater.md).
# The one thing installed on a house machine by hand; its timer runs it every minute, and a run with nothing new is a
# no-op. It runs unchanged on Linux and on macOS's bash 3.2, with curl, awk, and sha256sum or else shasum -a 256.
#
#	~/.loom/loom-update.sh
#
# Settings, each from the environment or else from ~/.loom/update.conf (key=value lines, # comments):
#	base    LOOM_UPDATE_BASE     where current.txt and blobs/<sha256> are served (required)
#	host    LOOM_UPDATE_HOST     this machine's name for the manifest's canary hosts (default: hostname -s)
#	report  LOOM_UPDATE_REPORT   a URL this machine's state is POSTed to (optional), whenever it changes and every 5
#	                             minutes besides: {host, version, previous, updated, hooked, held, refused, services, at},
#	                             signed with ~/.loom/report-token (`loom release report-token <host>` on Workshop)
#	hold    LOOM_UPDATE_HOLD     "current" keeps the version installed; a version keeps (or brings) this machine on
#	                             exactly that one, from <base>/manifests/<version>.txt. Either way current.txt isn't read
#	                             until the hold is removed, and the log and the report say so.
#
# Every executable in ~/.loom/health.d/ prints a line per service it watches, "<service> <state> [<key>=<value>...]",
# such as "loom-serve.service active running restarts=0", and the report carries those lines as its services: how the
# release watcher on Workshop sees a canary stay healthy (docs/releases.md). The updater itself knows no service.
#
# A version installs into ~/.loom/versions/<version>/ beside its SHA256SUMS. Services run ~/.loom/bin/<name>, a
# symlink to that version's file, each replaced by renaming a new symlink over it; ~/.loom/version names the version
# once every link is in place, and ~/.loom/previous the one before, kept for a one-step rollback. A file whose sha256
# an installed version already holds is linked from there, not downloaded. A downloaded blob that doesn't hash to its
# name is refused and nothing is switched, as is a manifest cut short. After a switch every executable in
# ~/.loom/updated.d/ runs, in name order, which is how a machine restarts its own services; until they all pass, every
# later run runs them again. This machine never builds: it holds at most two versions. Exit 0 when current (or another
# run holds the lock), 1 otherwise.
set -u
root=${HOME}/.loom
versions=${root}/versions
bin=${root}/bin
lock=${root}/update.lock
log=${root}/update.log
self=$(basename "$0")

setting() { # setting <key> <environment value>: the value, from the environment first, then update.conf.
	if [ -n "$2" ]; then
		printf '%s\n' "$2"
		return
	fi
	[ -f "${root}/update.conf" ] || return 0
	sed -n "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*//p" "${root}/update.conf" | sed 's/[[:space:]]*$//' | tail -1
}
base=$(setting base "${LOOM_UPDATE_BASE:-}")
host=$(setting host "${LOOM_UPDATE_HOST:-}")
report=$(setting report "${LOOM_UPDATE_REPORT:-}")
hold=$(setting hold "${LOOM_UPDATE_HOLD:-}")
[ -n "${host}" ] || host=$(hostname -s)
base=${base%/}

say() { # say <line>: one line to update.log and to stderr.
	local line
	line="$(date -u +%Y-%m-%dT%H:%M:%SZ) ${host} $*"
	printf '%s\n' "${line}" >> "${log}"
	printf '%s\n' "${line}" >&2
}
refuse() { # refuse <why>: logs it, removes a half-made version, reports it once the lock is held, and exits.
	say "refused: $*"
	[ -n "${staging:-}" ] && [ -d "${staging}" ] && clear "${staging}"
	[ -n "${reporting:-}" ] && post "$*"
	exit 1
}
# clear <directory>: removes a version's files one by one, then the directory; something else in it keeps it.
clear() {
	local file
	for file in "$1"/* "$1"/.[!.]*; do
		[ -f "${file}" ] || [ -L "${file}" ] || continue
		rm -f "${file}"
	done
	rmdir "$1" 2> /dev/null || say "kept $1: it holds something that isn't a file"
}
hash() {
	if command -v sha256sum > /dev/null 2>&1; then
		sha256sum "$1" 2> /dev/null | cut -c1-64
	else
		shasum -a 256 "$1" 2> /dev/null | cut -c1-64
	fi
}
digest() { hash /dev/stdin; } # digest: the sha256 of stdin, in hex
bytes() { printf "$(printf '%s' "$1" | sed 's/../\\x&/g')"; } # bytes <hex>: the bytes the hex spells
# keyed <hex key> <byte>: each byte of a 64-byte key XORed with the byte, in hex.
keyed() {
	local hex=$1 out= byte
	while [ -n "${hex}" ]; do
		printf -v byte '%02x' $((0x${hex:0:2} ^ $2))
		out=${out}${byte}
		hex=${hex:2}
	done
	printf '%s' "${out}"
}
# sign <token> <file>: HMAC-SHA256 of the file keyed by the token, in hex, as Workshop's receiver checks it
# (release.SignReport), made from the sha256 this script already needs rather than openssl: a key over 64 bytes is its
# hash, padded with zeros to 64, and each pass hashes the key XORed with 0x36, then 0x5c, before what it signs. The
# token never reaches a command's arguments.
sign() {
	local key inner
	key=$(printf '%s' "$1" | od -An -v -tx1 | tr -d ' \n')
	[ ${#key} -gt 128 ] && key=$(printf '%s' "$1" | digest)
	while [ ${#key} -lt 128 ]; do key=${key}00; done
	inner=$({ bytes "$(keyed "${key}" 0x36)"; cat "$2"; } | digest)
	{ bytes "$(keyed "${key}" 0x5c)"; bytes "${inner}"; } | digest
}
# place <path> <symlink target | -> [text]: replaces <path> by renaming a new symlink, or with "-" a new file holding
# the text, over it. mv renames a file or a symlink to a file atomically on Linux and macOS alike, so <path> is
# never missing; only a directory there would take the new one inside it, so one is refused.
place() {
	[ -d "$1" ] && refuse "$1 is a directory"
	if [ "$2" = - ]; then
		printf '%s\n' "$3" > "$1.new.$$"
	else
		ln -s "$2" "$1.new.$$"
	fi && mv -f "$1.new.$$" "$1" || refuse "replacing $1"
}

mkdir -p "${versions}" "${bin}" "${root}/updated.d" || exit 1
case "${host}" in *[!A-Za-z0-9._-]*) refuse "host '${host}' is not a plain host name" ;; esac
[ -n "${base}" ] || refuse "no base URL: set LOOM_UPDATE_BASE or base= in ${root}/update.conf"
case "${hold}" in "" | current) ;; *)
	printf '%s\n' "${hold}" | grep -Eq '^[0-9A-Za-z][0-9A-Za-z._-]{0,127}$' || refuse "hold '${hold}' is neither current nor a version's plain name"
	;;
esac

# One run at a time: update.lock is a directory, made by mkdir (atomic on every system), holding its run's pid. A pid
# that is no longer a running loom-update (dead, or its number reused by something else) leaves the lock stale, and
# the first run to make takeover-<pid> inside it takes it over; that directory stays until release, so a run that
# judged the same pid stale a moment later can't take it again. A lock with no pid yet is a run between its mkdir and
# its pid, unless it is over a minute old.
alive() {
	kill -0 "$1" 2> /dev/null && ps -p "$1" -o command= 2> /dev/null | grep -qF "${self}"
}
release() {
	rm -f "${manifest:-}" "${manifest:-}.wanted" "${root}/report.$$" "${root}/report.$$.answer" "${root}/report.$$.answer.err"
	[ "$(cat "${lock}/pid" 2> /dev/null)" = "$$" ] || return 0
	rm -f "${lock}/pid" "${lock}"/pid.*
	for directory in "${lock}"/takeover-*; do [ -d "${directory}" ] && rmdir "${directory}"; done
	rmdir "${lock}"
}
locked() {
	mkdir "${lock}" 2> /dev/null && return 0
	holder=$(cat "${lock}/pid" 2> /dev/null)
	if [ -n "${holder}" ]; then
		alive "${holder}" && return 1
	else
		[ -n "$(find "${lock}" -prune -mmin +1 2> /dev/null)" ] || return 1
		holder=none
	fi
	mkdir "${lock}/takeover-${holder}" 2> /dev/null || return 1
	say "took over the lock of run ${holder}, no longer running"
}
if ! locked; then
	echo "loom-update: another run holds ${lock}" >&2
	exit 0
fi
trap release EXIT
echo $$ > "${lock}/pid.$$" && mv -f "${lock}/pid.$$" "${lock}/pid" || refuse "writing ${lock}/pid"
# update.log keeps its last 2000 lines once it passes 1 MB: an unreachable base logs every minute, and hooks write here.
if [ -f "${log}" ] && [ "$(wc -c < "${log}")" -gt 1048576 ]; then
	tail -n 2000 "${log}" > "${log}.$$" && mv -f "${log}.$$" "${log}"
fi

current() { cat "${root}/$1" 2> /dev/null; } # current <version | previous | hooked | updated | held>: its line, or nothing.
quoted() { LC_ALL=C tr -cd ' -~' | sed 's/[\\"]/\\&/g'; } # quoted: stdin as the inside of a JSON string, printable ASCII
# services: each health.d probe's lines, "<service> <state> [<key>=<value>...]", as JSON strings joined by commas. A
# probe that exits non-zero adds "<probe> probe-failed"; a line of any other shape is dropped, and at most 32 are kept.
services() {
	local probe line
	for probe in "${root}/health.d"/*; do
		[ -f "${probe}" ] && [ -x "${probe}" ] || continue
		"${probe}" < /dev/null 2> /dev/null || echo "$(basename "${probe}") probe-failed"
	done | LC_ALL=C grep -E '^[A-Za-z0-9][A-Za-z0-9._@-]* [a-z][a-z-]*( [ -~]*)?$' | head -n 32 | cut -c1-200 |
		while IFS= read -r line; do printf '"%s"\n' "$(printf '%s' "${line}" | quoted)"; done | paste -sd, -
}
# post [refusal]: reports this machine's state: the version it runs, the one before, when it last switched, the version
# whose hooks all passed, its hold, this run's refusal if any, and its services. Sent when that differs from what the
# report URL last accepted (kept in reported) and every 5 minutes besides, so a machine gone quiet reads as quiet; a
# failed or refused report is logged with the receiver's answer and sent again by the next run, and never fails the
# update. Each is signed with report-token, whose claims go beside the signature and the token itself never: anything
# on the house's network can reach the receiver, which takes only what this machine's own token signed.
post() {
	[ -n "${report}" ] || return 0
	local state body token code
	state=$(printf '{"host":"%s","version":"%s","previous":"%s","updated":"%s","hooked":"%s","held":"%s","refused":"%s","services":[%s]' \
		"${host}" "$(current version)" "$(current previous)" "$(current updated)" "$(current hooked)" "${hold}" \
		"$(printf '%s' "${1:-}" | cut -c1-400 | quoted)" "$(services)")
	[ "$(cat "${root}/reported" 2> /dev/null)" = "${state}" ] && [ -z "$(find "${root}/reported" -mmin +4 2> /dev/null)" ] && return 0
	token=$(LC_ALL=C tr -d ' \t\r\n' < "${root}/report-token" 2> /dev/null)
	case "${token}" in *[!A-Za-z0-9_.-]* | "" | *.*.* | .* | *.) say "report of $(current version) to ${report} not sent: no report token in ${root}/report-token (on Workshop: loom release report-token ${host})"; return 0 ;; esac
	body=${root}/report.$$
	printf '%s,"at":"%s"}' "${state}" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "${body}"
	code=$(curl -sS -m 20 -o "${body}.answer" -w '%{http_code}' -X POST -H 'content-type: application/json' -H "X-Loom-Report-Claims: ${token%%.*}" \
		-H "X-Loom-Report-Signature: $(sign "${token}" "${body}")" --data-binary @"${body}" "${report}" 2> "${body}.answer.err")
	case "${code}" in
	2??) printf '%s\n' "${state}" > "${root}/reported.$$" && mv -f "${root}/reported.$$" "${root}/reported" ;;
	*) say "report of $(current version) to ${report} failed (${code:-no answer}: $(cat "${body}.answer" "${body}.answer.err" 2> /dev/null | LC_ALL=C tr -cd ' -~' | cut -c1-300)); the next run tries again" ;;
	esac
	rm -f "${body}" "${body}.answer" "${body}.answer.err"
}
reporting=1
# A hold is logged once when it is set, changed or removed, and kept in held while it stands.
if [ "${hold}" != "$(current held)" ]; then
	if [ -n "${hold}" ]; then
		say "held at ${hold} by the hold setting: current.txt is not read until it is removed"
		place "${root}/held" - "${hold}"
	else
		say "hold at $(current held) removed: following current.txt again"
		rm -f "${root}/held"
	fi
fi

case "$(uname -s)" in Linux) system=linux ;; Darwin) system=darwin ;; *) refuse "unknown system $(uname -s)" ;; esac
case "$(uname -m)" in x86_64 | amd64) architecture=amd64 ;; aarch64 | arm64) architecture=arm64 ;; *) refuse "unknown architecture $(uname -m)" ;; esac
platform=${system}/${architecture}

manifest=${root}/current.txt.$$
# What this machine follows: current.txt; under a hold on a version, that version's own manifest, which upload.sh keeps
# beside the blobs for every version it ever published; under a hold on the current version, the version installed.
source=current.txt
[ -n "${hold}" ] && [ "${hold}" != current ] && source=manifests/${hold}.txt
if [ "${hold}" = current ]; then
	standing=$(current version)
	if [ -z "${standing}" ]; then
		post ""
		echo "loom-update: held with nothing installed" >&2
		exit 0
	fi
	[ -f "${versions}/${standing}/SHA256SUMS" ] || refuse "held at ${standing}, whose ${versions}/${standing}/SHA256SUMS is missing"
	awk -v version="${standing}" -v platform="${platform}" 'BEGIN { print "version " version } { print $2, platform, $1; n++ } END { print "end", n + 0 }' \
		"${versions}/${standing}/SHA256SUMS" > "${manifest}"
else
	curl -fsS -m 60 -o "${manifest}" "${base}/${source}" || refuse "fetching ${base}/${source}"
fi
# The entry for this host: the top section, or the canary section when the canary line names this host. Each section
# is a version line, its file lines and an end line counting them, and a canary line comes first and promises a second
# section, so a manifest cut short at any line is refused rather than read as a smaller one. Printed as "version <v>"
# then one "<sha256> <name>" line per file built for this platform.
awk -v host="${host}" -v platform="${platform}" '
BEGIN { sections = 0 }
function fail(message) { print message; bad = 1; exit 1 }
function plain(name) { return name ~ /^[0-9A-Za-z][0-9A-Za-z._-]*$/ && length(name) <= 128 }
NF == 0 { next }
$1 == "canary" && NF >= 2 {
	if (sections || canary) fail("line " NR ": a canary line not first")
	canary = 1
	for (i = 2; i <= NF; i++) if (tolower($i) == tolower(host)) mine = 1
	next
}
$1 == "version" && NF == 2 {
	if (sections && !ended[sections - 1]) fail("line " NR ": a version line before the last section ended")
	if (sections == 1 + canary) fail("line " NR ": a section more than the manifest names")
	if (!plain($2)) fail("line " NR ": version " $2 " is not a plain name")
	version[sections] = $2
	sections++
	next
}
$1 == "end" && NF == 2 {
	s = sections - 1
	if (s < 0 || ended[s]) fail("line " NR ": an end line outside a section")
	if ($2 !~ /^[0-9]+$/ || $2 + 0 != count[s] + 0) fail("line " NR ": the end line counts " $2 " files, the section holds " count[s] + 0)
	ended[s] = 1
	next
}
NF == 3 {
	s = sections - 1
	if (s < 0 || ended[s]) fail("line " NR ": a file outside a section")
	if (!plain($1) || $1 == "SHA256SUMS") fail("line " NR ": file name " $1 " is not a plain name")
	if (length($3) != 64 || $3 ~ /[^0-9a-f]/) fail("line " NR ": " $3 " is not a sha256")
	if (seen[s, $1, $2]++) fail("line " NR ": " $1 " for " $2 " twice")
	count[s]++
	if ($2 == platform) files[s] = files[s] $3 " " $1 "\n"
	next
}
{ fail("line " NR " is not a manifest line: " $0) }
END {
	if (bad) exit 1
	if (sections < 1 + canary || !ended[sections - 1]) fail("it ends before its last end line: cut short")
	entry = mine ? 1 : 0
	if (files[entry] == "") fail("version " version[entry] " has no file for " platform)
	printf "version %s\n%s", version[entry], files[entry]
}' "${manifest}" > "${manifest}.wanted" || refuse "${base}/${source}: $(cat "${manifest}.wanted")"
version=$(sed -n '1s/^version //p' "${manifest}.wanted")
case "${hold}" in "" | current | "${version}") ;; *) refuse "${base}/${source} names version ${version}, not the held ${hold}" ;; esac
sed '1d' "${manifest}.wanted" | LC_ALL=C sort > "${manifest}"
installed=$(current version)

pointed() { # pointed: every file of the version has its bin link.
	local sha name
	while read -r sha name; do
		[ "$(readlink "${bin}/${name}")" = "../versions/${version}/${name}" ] || return 1
	done < "${manifest}"
}
# hooks <previous>: runs every hook in updated.d; only when each exits 0 is the version recorded in hooked, so hooks
# that failed run again on every later run until they pass, and a service never stays on old code unnoticed.
hooks() {
	local hook code failed=0
	for hook in "${root}/updated.d"/*; do
		[ -f "${hook}" ] && [ -x "${hook}" ] || continue
		LOOM_UPDATE_VERSION=${version} LOOM_UPDATE_PREVIOUS=$1 LOOM_UPDATE_BIN=${bin} \
			"${hook}" < /dev/null >> "${log}" 2>&1
		code=$?
		if [ "${code}" != 0 ]; then
			say "hook $(basename "${hook}") exited ${code} for ${version}; the next run runs the hooks again"
			failed=1
		fi
	done
	[ "${failed}" = 0 ] || return 1
	place "${root}/hooked" - "${version}"
}
if [ "${installed}" = "${version}" ] && cmp -s "${manifest}" "${versions}/${version}/SHA256SUMS" && pointed; then
	status=0
	[ "$(current hooked)" = "${version}" ] || { hooks "$(current previous)"; status=$?; }
	post ""
	exit "${status}"
fi

target=${versions}/${version}
if [ -d "${target}" ]; then
	# A version is never rebuilt: kept on disk, it is the same files, and switching back to it is the rollback.
	cmp -s "${manifest}" "${target}/SHA256SUMS" || refuse "${target} holds other files than ${version} names now; a version never changes"
	fetched="already on disk"
else
	staging=${versions}/.${version}.$$
	mkdir "${staging}" || refuse "making ${staging}"
	downloaded=0 linked=0
	while read -r sha name; do
		# A file an installed version holds, still hashing to its name, is linked from it.
		kept=
		for sums in "${versions}"/*/SHA256SUMS; do
			[ -f "${sums}" ] || continue
			held=$(awk -v sha="${sha}" '$1 == sha { print $2; exit }' "${sums}")
			[ -n "${held}" ] && [ "$(hash "$(dirname "${sums}")/${held}")" = "${sha}" ] && kept=$(dirname "${sums}")/${held} && break
		done
		if [ -n "${kept}" ]; then
			ln "${kept}" "${staging}/${name}" 2> /dev/null || cp -p "${kept}" "${staging}/${name}" || refuse "copying ${kept}"
			linked=$((linked + 1))
			continue
		fi
		temporary=${staging}/.${name}.download
		curl -fsS -m 600 -o "${temporary}" "${base}/blobs/${sha}" || refuse "downloading ${name} (${base}/blobs/${sha})"
		got=$(hash "${temporary}")
		[ "${got}" = "${sha}" ] || refuse "${name} from ${base}/blobs/${sha} hashes to ${got}; nothing installed, ${installed:-nothing} stays"
		chmod 755 "${temporary}" && mv "${temporary}" "${staging}/${name}" || refuse "installing ${name}"
		downloaded=$((downloaded + 1))
	done < "${manifest}"
	cp "${manifest}" "${staging}/SHA256SUMS" && mv "${staging}" "${target}" || refuse "installing ${target}"
	staging=
	fetched="${downloaded} downloaded, ${linked} linked"
fi

# Each bin link to the new version's file, then the version files, version last.
while read -r sha name; do
	place "${bin}/${name}" "../versions/${version}/${name}"
done < "${manifest}"
[ -n "${installed}" ] && [ "${installed}" != "${version}" ] && place "${root}/previous" - "${installed}"
place "${root}/version" - "${version}"
place "${root}/updated" - "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
previous=$(current previous)
say "installed ${version}, previous ${installed:-none} (${fetched})"

# A bin link into a version for a name the version no longer ships goes, then every version but this one and the
# previous, file by file, with any staging a killed run left behind.
for link in "${bin}"/* "${bin}"/.[!.]*; do
	[ -L "${link}" ] || continue
	case "$(readlink "${link}")" in ../versions/*) ;; *) continue ;; esac
	awk -v name="$(basename "${link}")" '$2 == name { found = 1 } END { exit !found }' "${manifest}" || rm -f "${link}"
done
for directory in "${versions}"/* "${versions}"/.[!.]*; do
	[ -d "${directory}" ] && [ ! -L "${directory}" ] || continue
	case "$(basename "${directory}")" in "${version}" | "${previous}") continue ;; esac
	clear "${directory}"
done

hooks "${installed}"
status=$?
post ""
exit "${status}"
