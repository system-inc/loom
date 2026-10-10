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
#	report  LOOM_UPDATE_REPORT   a URL each new version is POSTed to as {host, version, previous, at} (optional)
#
# A version installs into ~/.loom/versions/<version>/ beside its SHA256SUMS. Services run ~/.loom/bin/<name>, a
# symlink to that version's file, each replaced by renaming a new symlink over it; ~/.loom/version names the version
# once every link is in place, and ~/.loom/previous the one before, kept for a one-step rollback. A file whose sha256
# an installed version already holds is linked from there, not downloaded. A downloaded blob that doesn't hash to its
# name is refused and nothing is switched. After a switch every executable in ~/.loom/updated.d/ runs, in name order,
# which is how a machine restarts its own services. Exit 0 when current (or another run holds the lock), 1 otherwise.
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
[ -n "${host}" ] || host=$(hostname -s)
base=${base%/}

say() { # say <line>: one line to update.log and to stderr.
	local line
	line="$(date -u +%Y-%m-%dT%H:%M:%SZ) ${host} $*"
	printf '%s\n' "${line}" >> "${log}"
	printf '%s\n' "${line}" >&2
}
refuse() { # refuse <why>: logs it, removes a half-made version, and exits; nothing was switched.
	say "refused: $*"
	[ -n "${staging:-}" ] && [ -d "${staging}" ] && clear "${staging}"
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

# One run at a time: update.lock is a directory, made by mkdir (atomic on every system), holding its run's pid. A pid
# that is no longer a running loom-update (dead, or its number reused by something else) leaves the lock stale, and
# the first run to make takeover-<pid> inside it takes it over; that directory stays until release, so a run that
# judged the same pid stale a moment later can't take it again. A lock with no pid yet is a run between its mkdir and
# its pid, unless it is over a minute old.
alive() {
	kill -0 "$1" 2> /dev/null && ps -p "$1" -o command= 2> /dev/null | grep -qF "${self}"
}
release() {
	rm -f "${manifest:-}" "${manifest:-}.wanted"
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

current() { cat "${root}/$1" 2> /dev/null; } # current <version | previous>: that version, or nothing.
# post <version> <previous>: reports the version once; a failed report is retried by the next run, never fatal.
post() {
	[ -n "${report}" ] || return 0
	[ "$(cat "${root}/reported" 2> /dev/null)" = "$1" ] && return 0
	local body
	body=$(printf '{"host":"%s","version":"%s","previous":"%s","at":"%s"}' "${host}" "$1" "$2" "$(date -u +%Y-%m-%dT%H:%M:%SZ)")
	if curl -fsS -m 20 -X POST -H 'content-type: application/json' --data "${body}" "${report}" > /dev/null 2>&1; then
		echo "$1" > "${root}/reported"
	else
		say "report of $1 to ${report} failed; the next run tries again"
	fi
}

case "$(uname -s)" in Linux) system=linux ;; Darwin) system=darwin ;; *) refuse "unknown system $(uname -s)" ;; esac
case "$(uname -m)" in x86_64 | amd64) architecture=amd64 ;; aarch64 | arm64) architecture=arm64 ;; *) refuse "unknown architecture $(uname -m)" ;; esac
platform=${system}/${architecture}

manifest=${root}/current.txt.$$
curl -fsS -m 60 -o "${manifest}" "${base}/current.txt" || refuse "fetching ${base}/current.txt"
# The entry for this host: the top section, or the canary section when its canary line names this host. Printed as
# "version <v>" then one "<sha256> <name>" line per file built for this platform.
awk -v host="${host}" -v platform="${platform}" '
BEGIN { section = 0 }
function fail(message) { print message; bad = 1; exit 1 }
function plain(name) { return name ~ /^[0-9A-Za-z][0-9A-Za-z._-]*$/ && length(name) <= 128 }
NF == 0 { next }
$1 == "version" && NF == 2 {
	if (section in version) fail("line " NR ": a second version line in one section")
	if (!plain($2)) fail("line " NR ": version " $2 " is not a plain name")
	version[section] = $2
	next
}
$1 == "canary" && NF >= 2 {
	if (section) fail("line " NR ": a second canary line")
	section = 1
	for (i = 2; i <= NF; i++) if (tolower($i) == tolower(host)) mine = 1
	next
}
NF == 3 {
	if (!(section in version)) fail("line " NR ": a file before its version line")
	if (!plain($1) || $1 == "SHA256SUMS") fail("line " NR ": file name " $1 " is not a plain name")
	if (length($3) != 64 || $3 ~ /[^0-9a-f]/) fail("line " NR ": " $3 " is not a sha256")
	if (seen[section, $1, $2]++) fail("line " NR ": " $1 " for " $2 " twice")
	if ($2 == platform) files[section] = files[section] $3 " " $1 "\n"
	next
}
{ fail("line " NR " is not a manifest line: " $0) }
END {
	if (bad) exit 1
	entry = mine ? 1 : 0
	if (!(entry in version)) fail("no version line")
	if (files[entry] == "") fail("version " version[entry] " has no file for " platform)
	printf "version %s\n%s", version[entry], files[entry]
}' "${manifest}" > "${manifest}.wanted" || refuse "${base}/current.txt: $(cat "${manifest}.wanted")"
version=$(sed -n '1s/^version //p' "${manifest}.wanted")
sed '1d' "${manifest}.wanted" | LC_ALL=C sort > "${manifest}"
installed=$(current version)

pointed() { # pointed: every file of the version has its bin link.
	local sha name
	while read -r sha name; do
		[ "$(readlink "${bin}/${name}")" = "../versions/${version}/${name}" ] || return 1
	done < "${manifest}"
}
if [ "${installed}" = "${version}" ] && cmp -s "${manifest}" "${versions}/${version}/SHA256SUMS" && pointed; then
	post "${version}" "$(current previous)"
	exit 0
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

failed=0
for hook in "${root}/updated.d"/*; do
	[ -f "${hook}" ] && [ -x "${hook}" ] || continue
	LOOM_UPDATE_VERSION=${version} LOOM_UPDATE_PREVIOUS=${installed} LOOM_UPDATE_BIN=${bin} \
		"${hook}" < /dev/null >> "${log}" 2>&1
	code=$?
	if [ "${code}" != 0 ]; then
		say "hook $(basename "${hook}") exited ${code} after installing ${version}"
		failed=1
	fi
done
post "${version}" "${installed}"
exit "${failed}"
