# release-lock.sh, sourced by publish.sh and upload.sh (docs/releases.md).
#
# release_lock <out directory>: holds <out>/.release.lock until the script exits, without waiting, as the release
# watcher on Workshop takes it (release.Lock, an flock), and exits 1 when it is held: a person's publish or upload never
# runs beside a watcher's pass, nor the watcher's beside theirs. The watcher hands its own hold down as
# LOOM_RELEASE_LOCK_FD, a descriptor open on that same file, which is taken again in place; a descriptor open on any
# other file is refused. flock(1) takes it where there is one (Linux), perl where there isn't (macOS).
release_lock() {
	local path=$1/.release.lock fd=${LOOM_RELEASE_LOCK_FD:-} name
	name=$(basename "$0" .sh)
	if [ -n "${fd}" ]; then
		case "${fd}" in *[!0-9]*) echo "${name}: LOOM_RELEASE_LOCK_FD=${fd} isn't a descriptor" >&2; exit 1 ;; esac
		perl -e 'my @held = stat(STDIN); my @lock = stat($ARGV[0]); exit !(@held && @lock && $held[0] == $lock[0] && $held[1] == $lock[1])' "${path}" <&"${fd}" 2> /dev/null ||
			{ echo "${name}: descriptor ${fd} (LOOM_RELEASE_LOCK_FD) isn't open on ${path}" >&2; exit 1; }
	else
		fd=9
		exec 9>> "${path}" || { echo "${name}: can't open ${path}" >&2; exit 1; }
	fi
	if command -v flock > /dev/null 2>&1; then
		flock -n "${fd}"
	else
		perl -MFcntl=:flock -e 'open(my $lock, ">>&=", $ARGV[0]) or exit 2; flock($lock, LOCK_EX | LOCK_NB) or exit 1' "${fd}"
	fi || { echo "${name}: ${path} is held: the release watcher, or another publish or upload, is at work; nothing done" >&2; exit 1; }
}
