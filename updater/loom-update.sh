#!/bin/bash
# loom-update.sh: brings this machine's Loom binaries to what the published manifest names for it (docs/updater.md).
# The one thing installed on a house machine by hand; its timer runs it every minute, and a run with nothing new is a
# no-op. It runs unchanged on Linux and on macOS's bash 3.2, and needs curl and python3.
#
#	~/.loom/loom-update.sh
#
# Settings, each from the environment or else from ~/.loom/update.conf (key=value lines, # comments):
#	base    LOOM_UPDATE_BASE     where current.json and blobs/<sha256> are served (required)
#	host    LOOM_UPDATE_HOST     this machine's name for the manifest's canary hosts (default: hostname -s)
#	report  LOOM_UPDATE_REPORT   a URL each new version is POSTed to as {host, version, previous, at} (optional)
#
# A version installs into ~/.loom/versions/<version>/ beside its SHA256SUMS; ~/.loom/current and ~/.loom/previous are
# symlinks to the version running and the one before, each switched by a rename. A file whose sha256 an installed
# version already holds is linked from there, not downloaded. A downloaded blob that doesn't hash to its name is
# refused and nothing is switched. After a switch every executable in ~/.loom/updated.d/ runs, in name order, which
# is how a machine restarts its own services. Exit 0 when current (or another run holds the lock), 1 otherwise.
set -u
root=${HOME}/.loom
versions=${root}/versions
log=${root}/update.log

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

mkdir -p "${versions}" "${root}/updated.d" || exit 1
case "${host}" in *[!A-Za-z0-9._-]*) refuse "host '${host}' is not a plain host name" ;; esac
[ -n "${base}" ] || refuse "no base URL: set LOOM_UPDATE_BASE or base= in ${root}/update.conf"

# One run at a time: an flock on update.lock, held by this shell's descriptor 9 and released when it exits, however it
# exits. Hooks run with 9 closed, so a service a hook starts never holds it.
exec 9>> "${root}/update.lock"
if ! python3 -c 'import fcntl; fcntl.flock(9, fcntl.LOCK_EX | fcntl.LOCK_NB)' 2> /dev/null; then
	echo "loom-update: another run holds ${root}/update.lock" >&2
	exit 0
fi

hash() {
	if command -v sha256sum > /dev/null 2>&1; then
		sha256sum "$1" 2> /dev/null | cut -c1-64
	else
		shasum -a 256 "$1" 2> /dev/null | cut -c1-64
	fi
}
# swap <link> <target>: points <link> at <target> by renaming a fresh symlink over it, so it is never missing.
swap() {
	python3 -c 'import os, sys; os.symlink(sys.argv[2], sys.argv[1] + ".new"); os.replace(sys.argv[1] + ".new", sys.argv[1])' "$1" "$2"
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
current() { # current <link>: the version a link points at, or nothing.
	local target
	target=$(readlink "${root}/$1" 2> /dev/null) || return 0
	basename "${target}"
}
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

manifest=${root}/current.json.$$
trap 'rm -f "${manifest}" "${manifest}.wanted"' EXIT
curl -fsS -m 60 -o "${manifest}" "${base}/current.json" || refuse "fetching ${base}/current.json"
# The entry for this host, as "version <v>" then one "<sha256> <name>" line per file built for this platform, sorted.
python3 - "${manifest}" "${host}" "${platform}" > "${manifest}.wanted" << 'PYTHON' || refuse "${base}/current.json: $(cat "${manifest}.wanted")"
import json, re, sys
path, host, platform = sys.argv[1:]
def fail(message):
    print(message)
    sys.exit(1)
try:
    manifest = json.load(open(path))
except ValueError as error:
    fail("not JSON: %s" % error)
entry = manifest
canary = manifest.get("canary")
if canary and host.lower() in [name.lower() for name in canary.get("hosts", [])]:
    entry = canary
version, files = entry.get("version"), entry.get("files")
if not isinstance(version, str) or not re.fullmatch(r"[0-9A-Za-z][0-9A-Za-z._-]{0,127}", version):
    fail("version %r is not a plain name" % (version,))
if not isinstance(files, dict):
    fail("version %s has no files" % version)
lines = []
for name, platforms in files.items():
    if not re.fullmatch(r"[0-9A-Za-z][0-9A-Za-z._-]{0,127}", name) or name == "SHA256SUMS":
        fail("file name %r is not a plain name" % (name,))
    sha = platforms.get(platform) if isinstance(platforms, dict) else None
    if sha is None:
        continue
    if not isinstance(sha, str) or not re.fullmatch(r"[0-9a-f]{64}", sha):
        fail("%s for %s has sha256 %r" % (name, platform, sha))
    lines.append("%s %s" % (sha, name))
if not lines:
    fail("version %s has no file for %s" % (version, platform))
print("version " + version)
print("\n".join(sorted(lines)))
PYTHON
version=$(sed -n '1s/^version //p' "${manifest}.wanted")
sed '1d' "${manifest}.wanted" > "${manifest}"
installed=$(current current)

if [ "${installed}" = "${version}" ] && cmp -s "${manifest}" "${versions}/${version}/SHA256SUMS"; then
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
	fetched="${downloaded} downloaded, ${linked} linked"
fi

[ -n "${installed}" ] && [ "${installed}" != "${version}" ] && swap "${root}/previous" "versions/${installed}"
swap "${root}/current" "versions/${version}" || refuse "switching ${root}/current to ${version}"
previous=$(current previous)
say "installed ${version}, previous ${installed:-none} (${fetched})"

# Every version but current and previous goes, file by file, with any staging a killed run left behind.
for directory in "${versions}"/* "${versions}"/.[!.]*; do
	[ -d "${directory}" ] && [ ! -L "${directory}" ] || continue
	case "$(basename "${directory}")" in "${version}" | "${previous}") continue ;; esac
	clear "${directory}"
done

failed=0
for hook in "${root}/updated.d"/*; do
	[ -f "${hook}" ] && [ -x "${hook}" ] || continue
	LOOM_UPDATE_VERSION=${version} LOOM_UPDATE_PREVIOUS=${installed} LOOM_UPDATE_CURRENT=${root}/current \
		"${hook}" < /dev/null >> "${log}" 2>&1 9>&-
	code=$?
	if [ "${code}" != 0 ]; then
		say "hook $(basename "${hook}") exited ${code} after installing ${version}"
		failed=1
	fi
done
post "${version}" "${installed}"
exit "${failed}"
