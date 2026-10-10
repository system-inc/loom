#!/bin/bash
# publish.sh: Workshop builds every Loom binary once for a commit, for every house platform, and writes what the
# machines' updaters read (docs/updater.md) into an output directory. Nothing leaves the directory: upload.sh sends it.
#
#	updater/publish.sh [--canary <Host,Host>] <loom commit> <out directory>
#
# The commit builds in a detached worktree of this repository with exactly the Go its go.mod's toolchain line names,
# LOOM_PUBLISH_GO or else `go` on PATH (CGO off, -trimpath, GOTOOLCHAIN=local, the runner's version stamped
# git-<sha12> as `loom run` stamps it). Each binary lands at blobs/<sha256> and the commit's own
# manifest at manifests/<commit>.txt. current.txt becomes that manifest, or with --canary keeps its top section and
# names this commit for those hosts only. Rolling back is copying an older manifests/<commit>.txt over current.txt.
#
# Workshop is the only builder, so its disk is guarded both ways. It refuses to start while the temporary directory,
# the out directory, Go's build cache or its module cache has under LOOM_PUBLISH_FLOOR_GB free (default 10; Workshop
# runs it with LOOM_PUBLISH_FLOOR_GB=100, the floor build-tree keeps there, #ckv0pmg). After a
# publish the out directory keeps the manifests current.txt names and the newest LOOM_PUBLISH_KEEP others (default
# 10), and only the blobs those name; the rest go, file by file. LOOM_PUBLISH_PLATFORMS replaces the platforms
# (default "linux/amd64 darwin/arm64").
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
usage() { echo "usage: updater/publish.sh [--canary <Host,Host>] <loom commit> <out directory>" >&2; exit 2; }
canary=
if [ "${1:-}" = --canary ]; then
	canary=${2:-}
	[ -n "${canary}" ] || usage
	shift 2
fi
[ $# = 2 ] && [ -n "$1" ] && [ -n "$2" ] || usage
platforms=${LOOM_PUBLISH_PLATFORMS:-linux/amd64 darwin/arm64}
floor=${LOOM_PUBLISH_FLOOR_GB:-10}
keep=${LOOM_PUBLISH_KEEP:-10}
# The binaries Loom ships: every Go main in the repository, the coordinator and its tools, and the runner.
binaries="loom:./cmd/loom loom-runner:./runner/cmd/loom-runner"
repository=$(git -C "${here}" rev-parse --show-toplevel) || exit 1
commit=$(git -C "${repository}" rev-parse --verify --quiet "$1^{commit}") || { echo "publish: $1 is not a commit in ${repository}" >&2; exit 1; }
out=$2
mkdir -p "${out}/blobs" "${out}/manifests" || exit 1
out=$(cd "${out}" && pwd)
[ -z "${canary}" ] || [ -f "${out}/current.txt" ] || { echo "publish: --canary keeps the published top section, and ${out}/current.txt doesn't exist" >&2; exit 1; }

hash() {
	if command -v sha256sum > /dev/null 2>&1; then sha256sum "$1" | cut -c1-64; else shasum -a 256 "$1" | cut -c1-64; fi
}
# free <path>: whole GB free on the filesystem holding <path>, or its nearest existing parent.
free() {
	local path=$1
	while [ ! -e "${path}" ] && [ "${path}" != / ]; do path=$(dirname "${path}"); done
	df -Pk "${path}" | awk 'NR == 2 { print int($4 / 1048576) }'
}
# The compiler is pinned: a different Go can make different binaries from the same commit, so the one used must be
# exactly the toolchain the commit's go.mod names. LOOM_PUBLISH_GO names it (Workshop's isn't on a service's PATH).
go=${LOOM_PUBLISH_GO:-go}
toolchain=$(git -C "${repository}" show "${commit}:go.mod" | awk '$1 == "toolchain" { print $2 }')
compiler=$(GOTOOLCHAIN=local "${go}" version 2> /dev/null | awk '{ print $3 }')
[ -n "${toolchain}" ] || { echo "publish: ${commit}'s go.mod names no toolchain, so nothing says which Go builds it" >&2; exit 1; }
[ "${compiler}" = "${toolchain}" ] || { echo "publish: ${go} is ${compiler:-not a Go}, and ${commit}'s go.mod names ${toolchain}; set LOOM_PUBLISH_GO" >&2; exit 1; }
for path in "${TMPDIR:-/tmp}" "${out}" $(GOTOOLCHAIN=local "${go}" env GOCACHE GOMODCACHE); do
	available=$(free "${path}")
	[ -n "${available}" ] && [ "${available}" -ge "${floor}" ] && continue
	echo "publish: ${path} has ${available:-unknown} GB free, under the ${floor} GB floor (LOOM_PUBLISH_FLOOR_GB); nothing built" >&2
	exit 1
done

work=$(mktemp -d)
source=${work}/loom
git -C "${repository}" worktree add -q --detach "${source}" "${commit}" || exit 1
built=${work}/built
: > "${built}"
failed=0
# A Go main the list doesn't name would never reach a machine: refused, so a new command can't be left behind.
for directory in $(grep -rl --include='*.go' '^package main$' "${source}" 2> /dev/null | sed "s#^${source}/##" | xargs -n1 dirname | sort -u); do
	case " ${binaries} " in *":./${directory} "*) ;; *)
		echo "publish: ${directory} is a Go main publish.sh doesn't ship; add it to binaries" >&2
		failed=1
		;;
	esac
done
for binary in ${binaries}; do
	[ "${failed}" = 0 ] || break
	name=${binary%%:*} package=${binary#*:}
	for platform in ${platforms}; do
		output=${work}/${name}-${platform%/*}-${platform#*/}
		if ! (cd "${source}" && GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=${platform%/*} GOARCH=${platform#*/} "${go}" build -trimpath \
			-ldflags "-X github.com/system-inc/loom/runner.Version=git-${commit:0:12}" -o "${output}" "${package}"); then
			echo "publish: building ${name} for ${platform} failed" >&2
			failed=1
			continue
		fi
		sha=$(hash "${output}")
		if [ ! -f "${out}/blobs/${sha}" ]; then
			cp "${output}" "${out}/blobs/.${sha}.$$" && mv "${out}/blobs/.${sha}.$$" "${out}/blobs/${sha}" || failed=1
		fi
		echo "${name} ${platform} ${sha}" >> "${built}"
		rm -f "${output}"
	done
done
git -C "${repository}" worktree remove --force "${source}"
[ "${failed}" = 0 ] || { rm -f "${built}"; rmdir "${work}"; exit 1; }

# The commit's manifest: its version line, one "<name> <os>/<arch> <sha256>" line per binary, and an end line
# counting them, so a manifest cut short is never read as a smaller one.
manifest=${out}/manifests/${commit}.txt
{ printf 'version %s\n' "${commit}"; LC_ALL=C sort "${built}"; printf 'end %s\n' "$(wc -l < "${built}" | tr -d ' ')"; } > "${manifest}.$$" &&
	mv -f "${manifest}.$$" "${manifest}" || failed=1
current=${out}/current.txt
if [ -n "${canary}" ]; then
	# A canary line first, naming the hosts and promising a second section; then the published top section as it is,
	# through its end line; then this commit's manifest.
	{
		printf 'canary %s\n' "$(printf '%s' "${canary}" | tr ',' ' ')"
		awk '$1 == "version" { on = 1 } on { print } on && $1 == "end" { exit }' "${current}"
		cat "${manifest}"
	} > "${current}.$$"
else
	cp "${manifest}" "${current}.$$"
fi && mv -f "${current}.$$" "${current}" || failed=1
rm -f "${built}"
rmdir "${work}"
[ "${failed}" = 0 ] || exit 1
echo "published ${commit}${canary:+ for ${canary}} in ${out}, built with ${compiler}"

# Pruning: the manifests current.txt names and the newest ${keep} others stay, and every blob one of them names.
named=$(awk '$1 == "version" { print $2 }' "${current}")
for kept in "${out}"/manifests/*.txt; do
	[ -f "${kept}" ] || continue
	version=$(basename "${kept}" .txt)
	case " $(echo ${named}) " in *" ${version} "*) continue ;; esac
	ls -t "${out}"/manifests/*.txt | head -n "${keep}" | grep -qxF "${kept}" || rm -f "${kept}"
done
blobs=$(cat "${current}" "${out}"/manifests/*.txt | awk 'NF == 3 && $1 != "canary" { print $3 }' | sort -u)
for blob in "${out}"/blobs/*; do
	[ -f "${blob}" ] || continue
	case " $(echo ${blobs}) " in *" $(basename "${blob}") "*) ;; *) rm -f "${blob}" ;; esac
done
