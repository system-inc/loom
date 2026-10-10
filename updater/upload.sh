#!/bin/bash
# upload.sh: sends what publish.sh wrote to where the updaters read it. Every blob current.txt names goes first and
# current.txt last, so a machine never reads a manifest naming a blob that isn't there yet.
#
#	updater/upload.sh <out directory> <destination directory>   # a directory a web server serves, as the tests do
#	updater/upload.sh <out directory> r2:<bucket>/<prefix>       # R2 through wrangler: r2:loom-artifacts/releases
#
# The r2: path has not run yet. It puts each object with `wrangler r2 object put --remote`, blobs as immutable and
# current.txt as no-cache, since every machine polls it. Releases go under releases/: the bucket's blobs/ and refs/
# are test products its lifecycle expires after 7 days.
set -uo pipefail
[ $# = 2 ] && [ -f "$1/current.txt" ] || { echo "usage: updater/upload.sh <out directory with current.txt> <destination>" >&2; exit 2; }
out=$1 destination=$2
case "${destination}" in
r2:*/?*) ;;
r2:*) echo "upload: ${destination} names no prefix, and a bucket's own blobs/ may expire; use r2:<bucket>/releases" >&2; exit 2 ;;
esac
# Every blob the published manifest names, top section and canary, then the versions it names.
named=$(awk 'NF == 3 && $1 != "canary" { print $3 }' "${out}/current.txt" | sort -u)
versions=$(awk '$1 == "version" { print $2 }' "${out}/current.txt")

put() { # put <file> <key> <cache control>
	case "${destination}" in
	r2:*)
		wrangler r2 object put "${destination#r2:}/$2" --file "$1" --cache-control "$3" --remote > /dev/null
		;;
	*)
		mkdir -p "$(dirname "${destination}/$2")" && cp "$1" "${destination}/$2.$$" && mv "${destination}/$2.$$" "${destination}/$2"
		;;
	esac
}
for sha in ${named}; do
	[ -f "${out}/blobs/${sha}" ] || { echo "upload: current.txt names blobs/${sha}, which ${out} doesn't hold" >&2; exit 1; }
	put "${out}/blobs/${sha}" "blobs/${sha}" "public, max-age=31536000, immutable" || exit 1
done
# Each named version's own manifest, kept beside the blobs so rolling back is uploading it as current.txt.
for version in ${versions}; do
	[ -f "${out}/manifests/${version}.txt" ] || continue
	put "${out}/manifests/${version}.txt" "manifests/${version}.txt" "no-cache" || exit 1
done
put "${out}/current.txt" current.txt "no-cache" || exit 1
echo "uploaded $(echo "${named}" | wc -l | tr -d ' ') blobs and current.txt to ${destination}"
