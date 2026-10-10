#!/bin/bash
# upload.sh: sends what publish.sh wrote to where the updaters read it. Every blob current.txt names goes first and
# current.txt last, so a machine never reads a manifest naming a blob that isn't there yet. It deletes nothing remote.
#
#	updater/upload.sh <out directory> <destination directory>   # a directory a web server serves, as the tests do
#	updater/upload.sh <out directory> r2:<bucket>/<prefix>       # R2: r2:loom-artifacts/releases
#
# The r2: path PUTs each object to R2's S3 endpoint with curl's own request signing (curl 7.75 or later), blobs as
# immutable and current.txt as no-cache, since every machine polls it. Its keys come from a key=value file,
# LOOM_UPLOAD_CREDENTIALS or else ~/.loom/r2-releases.conf: account_id, access_key_id and secret_access_key. The
# key pair reaches curl on its standard input, never its command line, so ps never shows it. Releases go under
# releases/: the bucket's blobs/ and refs/ are test products its lifecycle expires after 7 days.
#
# It holds <out>/.release.lock throughout (release-lock.sh), so a person's upload never interleaves with the release
# watcher's, and refuses when the lock is held.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
. "${here}/release-lock.sh"
[ $# = 2 ] && [ -f "$1/current.txt" ] || { echo "usage: updater/upload.sh <out directory with current.txt> <destination>" >&2; exit 2; }
out=$1 destination=$2
release_lock "${out}"
case "${destination}" in
r2:*/?*)
	credentials=${LOOM_UPLOAD_CREDENTIALS:-${HOME}/.loom/r2-releases.conf}
	field() { sed -n "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*//p" "${credentials}" 2> /dev/null | sed 's/[[:space:]]*$//' | tail -1; }
	account=$(field account_id) key=$(field access_key_id) secret=$(field secret_access_key)
	[ -n "${account}" ] && [ -n "${key}" ] && [ -n "${secret}" ] ||
		{ echo "upload: ${credentials} needs account_id, access_key_id and secret_access_key" >&2; exit 2; }
	case "${key}${secret}" in *'"'* | *\\*) echo "upload: a key in ${credentials} holds a quote or a backslash" >&2; exit 2 ;; esac
	endpoint=https://${account}.r2.cloudflarestorage.com/${destination#r2:}
	;;
r2:*) echo "upload: ${destination} names no prefix, and a bucket's own blobs/ may expire; use r2:<bucket>/releases" >&2; exit 2 ;;
esac
# Every blob the published manifest names, top section and canary, then the versions it names.
named=$(awk 'NF == 3 && $1 != "canary" { print $3 }' "${out}/current.txt" | sort -u)
versions=$(awk '$1 == "version" { print $2 }' "${out}/current.txt")

put() { # put <file> <key> <cache control>
	case "${destination}" in
	r2:*)
		printf 'user = "%s:%s"\n' "${key}" "${secret}" |
			curl -fsS --retry 2 --aws-sigv4 "aws:amz:auto:s3" -K - -X PUT -T "$1" -H "Cache-Control: $3" "${endpoint}/$2" > /dev/null
		;;
	*)
		mkdir -p "$(dirname "${destination}/$2")" && cp "$1" "${destination}/$2.$$" && mv "${destination}/$2.$$" "${destination}/$2"
		;;
	esac
}
for sha in ${named}; do
	[ -f "${out}/blobs/${sha}" ] || { echo "upload: current.txt names blobs/${sha}, which ${out} doesn't hold" >&2; exit 1; }
	put "${out}/blobs/${sha}" "blobs/${sha}" "public, max-age=31536000, immutable" || { echo "upload: blobs/${sha} failed" >&2; exit 1; }
done
# Each named version's own manifest, kept beside the blobs so rolling back is uploading it as current.txt.
for version in ${versions}; do
	[ -f "${out}/manifests/${version}.txt" ] || continue
	put "${out}/manifests/${version}.txt" "manifests/${version}.txt" "no-cache" || { echo "upload: manifests/${version}.txt failed" >&2; exit 1; }
done
put "${out}/current.txt" current.txt "no-cache" || { echo "upload: current.txt failed" >&2; exit 1; }
echo "uploaded $(echo "${named}" | wc -l | tr -d ' ') blobs and current.txt to ${destination}"
