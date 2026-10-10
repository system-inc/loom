#!/bin/bash
# publish.sh: Workshop builds every Loom binary once for a commit, for every house platform, and writes what the
# machines' updaters read (docs/updater.md) into an output directory. Nothing leaves the directory: upload.sh sends it.
#
#	updater/publish.sh [--canary <Host,Host>] <loom commit> <out directory>
#
# The commit builds in a detached worktree of this repository, with `go` from PATH (CGO off, -trimpath, the runner's
# version stamped git-<sha12> as `loom run` stamps it). Each binary lands at blobs/<sha256> and the commit's own
# manifest at manifests/<commit>.txt. current.txt becomes that manifest, or with --canary keeps its top section and
# names this commit for those hosts only. Rolling back is copying an older manifests/<commit>.txt over current.txt.
# LOOM_PUBLISH_PLATFORMS replaces the platforms (default "linux/amd64 darwin/arm64").
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
# The binaries Loom ships: the coordinator and its tools, the runner, and the gate pilot pregate.sh runs.
binaries="loom:./cmd/loom loom-runner:./runner/cmd/loom-runner adamic-gate:./pilots/adamic-gate"
repository=$(git -C "${here}" rev-parse --show-toplevel) || exit 1
commit=$(git -C "${repository}" rev-parse --verify --quiet "$1^{commit}") || { echo "publish: $1 is not a commit in ${repository}" >&2; exit 1; }
out=$2
mkdir -p "${out}/blobs" "${out}/manifests" || exit 1
out=$(cd "${out}" && pwd)
[ -z "${canary}" ] || [ -f "${out}/current.txt" ] || { echo "publish: --canary keeps the published top section, and ${out}/current.txt doesn't exist" >&2; exit 1; }

hash() {
	if command -v sha256sum > /dev/null 2>&1; then sha256sum "$1" | cut -c1-64; else shasum -a 256 "$1" | cut -c1-64; fi
}

work=$(mktemp -d)
source=${work}/loom
git -C "${repository}" worktree add -q --detach "${source}" "${commit}" || exit 1
built=${work}/built
: > "${built}"
failed=0
for binary in ${binaries}; do
	name=${binary%%:*} package=${binary#*:}
	for platform in ${platforms}; do
		output=${work}/${name}-${platform%/*}-${platform#*/}
		if ! (cd "${source}" && CGO_ENABLED=0 GOOS=${platform%/*} GOARCH=${platform#*/} go build -trimpath \
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

# The commit's manifest: its version line, then one "<name> <os>/<arch> <sha256>" line per binary.
manifest=${out}/manifests/${commit}.txt
{ printf 'version %s\n' "${commit}"; LC_ALL=C sort "${built}"; } > "${manifest}.$$" && mv -f "${manifest}.$$" "${manifest}" || failed=1
current=${out}/current.txt
if [ -n "${canary}" ]; then
	# The published top section as it is, then a canary line naming the hosts, then this commit's manifest.
	{ awk '$1 == "canary" { exit } { print }' "${current}"; printf 'canary %s\n' "$(printf '%s' "${canary}" | tr ',' ' ')"; cat "${manifest}"; } > "${current}.$$"
else
	cp "${manifest}" "${current}.$$"
fi && mv -f "${current}.$$" "${current}" || failed=1
[ "${failed}" = 0 ] && echo "published ${commit}${canary:+ for ${canary}} in ${out}"
rm -f "${built}"
rmdir "${work}"
exit "${failed}"
