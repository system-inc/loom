#!/bin/bash
# prepare.sh: the runner's own preparation for a test job (strict.go), compiled into the binary and never sent by the
# server. It readies a checkout of the public adamic repository at a verified commit for go test, and writes the
# environment the tests run in. Every value it takes is a positional argument the runner checked first (CheckTestJob),
# quoted at every use; nothing of the job is ever part of this text.
#
#	prepare.sh <tree> <sha> <base or ""> <gate inputs sha256 or ""> <environment file> <trim | keep> <root> <exclusive | shared>
#	prepare.sh environment <tree> <gate inputs sha256 or ""> <environment file> <root>
#	prepare.sh trim-only <root> <exclusive | shared>
#	prepare.sh pin-url <url>
#	prepare.sh pin-urls <tree>
#
# environment readies only what a prebuilt test job's binaries run with (prebuilt.go), over <tree>, the tree's source
# the runner already unpacked from the action store, its npm packages among it: the instance's adamic toolchain
# (env.sh, which must exist: exit 2 without it) and the gate inputs. No checkout, no setup, no npm, and nothing of Go;
# it writes nothing into <tree>.
#
# <root> holds everything it keeps between units beside the tree (the npm trees the runner placed for a checkout, the
# gate inputs, the setup marker) and is where it looks for what earlier units left. trim (a strict runner's: one unit at
# a time) first removes what earlier units left there; keep removes nothing. exclusive says the machine is the runner's
# alone (a Codex instance, --exclusive): only then does it clear HOME's adamic runtime builds and Go's build cache too,
# and run adamic's own cloud/setup.sh, which installs into HOME. shared (a house box, which other work shares) touches
# nothing of HOME but what go test itself writes, and a checkout on a machine without adamic's toolchain is unfit there,
# exit 2, named.
#
# Exit 0: the tree is at <sha> and <environment file> holds the environment, NUL separated. Exit 3: the job is refused
# (the sha or base can't be fetched from the public repository, a submodule isn't public on GitHub); nothing ran. Exit 2:
# the instance couldn't be readied (disk, network, setup): Loom's fault, never the change's.
#
# trim-only runs the trim alone and exits 0: a strict serve that finds its disk too full to take a unit runs it once
# (serve.go, #zzmz489), then looks again.
#
# pin-url prints the https url a submodule url is fetched from and exits 0, or prints why it can't be and exits 3: the
# one rule for pin urls, which the queue bridge's PinUrl follows to the letter (queuebridge/pins_test.go runs both on one
# table). pin-urls checks every url the .gitmodules of <tree> and of each submodule checked out under it names, at
# every depth, and exits 3 naming the first it refuses.
set -uo pipefail
say() { echo "loom-runner prepare: $*"; }
# pinUrl <url>: a repository on github.com over https, or git@github.com: read as https, owner/name and nothing else.
# Any other host (a house address among them), protocol, credential in the url or relative path is refused.
pinUrl() {
	local LC_ALL=C
	if [[ $1 =~ ^(https://github\.com/|git@github\.com:)([A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+)$ ]] && [[ ${BASH_REMATCH[2]} != *..* ]]; then
		echo "https://github.com/${BASH_REMATCH[2]}"
		return 0
	fi
	echo "it isn't a github.com repository over https (or git@github.com:, read as https), the only place a runner fetches a pin from"
	return 3
}
# pinUrls <tree>: every submodule url at every depth under <tree> follows pinUrl, or the first that doesn't is named.
pinUrls() {
	local directory entry url why
	# One directory a line, read whole, so a submodule path with a space is one directory, never two; and each url read
	# from git's NUL-separated "key, newline, value" entries, so a submodule name with a space never shifts it.
	while IFS= read -r directory; do
		while IFS= read -r -d '' entry; do
			url=${entry#*$'\n'}
			why=$(pinUrl "${url}") || { say "refused: submodule ${url} in ${directory}: ${why}"; return 3; }
		done < <(git -C "${directory}" config -z -f .gitmodules --get-regexp '^submodule\..*\.url$' 2> /dev/null)
	done < <(printf '%s\n' "$1"; git -C "$1" submodule foreach --quiet --recursive 'printf "%s\n" "${toplevel}/${sm_path}"' 2> /dev/null)
	return 0
}
if [ "${1:-}" = pin-url ]; then
	pinUrl "${2:-}"
	exit $?
fi
if [ "${1:-}" = pin-urls ]; then
	pinUrls "${2:-}"
	exit $?
fi
# Disk: on an instance that runs one unit at a time, what earlier units left on its root is no one's, and on a machine
# that is the runner's alone (exclusive), what they left in HOME's caches too. A shared machine's HOME is other work's.
freeMegabytes() { df -Pm "${HOME}" "${root}" | awk 'NR > 1 {print $4}' | sort -n | head -1; }
trimLeftovers() {
	rm -rf "${root}"/go-build* "${root}"/Test* "${root}"/adamic-npm/replaced-* "${root}"/adamic-npm/*.staging-* "${root}"/adamic-tools/staging-* 2> /dev/null
	find "${root}/adamic-gate" -mindepth 1 -maxdepth 1 ! -name 'markdown-width-*' -exec rm -rf {} + 2> /dev/null
	rm -rf "${root}"/adamic-stage3-lane-* 2> /dev/null
	if [ "${owner}" = exclusive ]; then
		rm -rf "${HOME}/.cache/adamic/runtime"/.build-* 2> /dev/null
		[ "$(freeMegabytes)" -ge 3000 ] || rm -rf "${HOME}/.cache/go-build"
	fi
}
if [ "${1:-}" = trim-only ]; then
	root=${2:-} owner=${3:-shared}
	case ${root} in /*) ;; *) say "the root must be an absolute path"; exit 2 ;; esac
	[ -d "${root}" ] || exit 0
	trimLeftovers
	say "trimmed ${root}: $(freeMegabytes) MB free"
	exit 0
fi
mode=checkout
if [ "${1:-}" = environment ]; then
	mode=environment tree=${2:-} sha= base= gateInputs=${3:-} environmentFile=${4:-} trim=keep root=${5:-} owner=shared
else
	tree=$1 sha=$2 base=$3 gateInputs=$4 environmentFile=$5 trim=$6 root=$7 owner=${8:-shared}
fi
case ${root} in /*) ;; *) echo "loom-runner prepare: the root must be an absolute path"; exit 2 ;; esac
mkdir -p "${root}"
repository=https://github.com/system-inc/adamic
started=${SECONDS}

# Git reads no configuration but what this script gives it: no system or global file (no credential helper, no URL
# rewrite, no hooks), never prompts, and never asks a helper for a password. Submodules recorded over ssh are fetched
# over HTTPS instead, with no credentials.
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 GIT_ASKPASS=/bin/false SSH_ASKPASS=/bin/false
export GIT_CONFIG_COUNT=3 GIT_CONFIG_KEY_0=url.https://github.com/.insteadOf GIT_CONFIG_VALUE_0=git@github.com:
export GIT_CONFIG_KEY_1=credential.helper GIT_CONFIG_VALUE_1= GIT_CONFIG_KEY_2=core.hooksPath GIT_CONFIG_VALUE_2=/dev/null

[ "${trim}" = trim ] && trimLeftovers
free=$(freeMegabytes)
[ "${free:-0}" -ge 1500 ] || { say "only ${free} MB free after trimming"; exit 2; }

if [ "${mode}" = checkout ]; then
	# The checkout: kept from earlier units, but only one whose own configuration names no remote helper, rewrite, hook or
	# credential; anything else is made again from the public repository.
	if [ -d "${tree}/.git" ] && git -C "${tree}" config --local --name-only --get-regexp '^(url\.|credential|http\.|core\.(askpass|sshcommand|fsmonitor|hookspath)|include)' > /dev/null 2>&1; then
		say "the kept checkout's configuration names a helper or rewrite; making it again"
		rm -rf "${tree}"
	fi
	# GitHub turns away anonymous fetches when many instances check out at once, so each step retries with backoff.
	# LOOM_PREPARE_ATTEMPTS is for the runner's tests; the runner never passes it, so a unit always gets four.
	retry() {
		local attempt attempts=${LOOM_PREPARE_ATTEMPTS:-4}
		for attempt in $(seq 1 "${attempts}"); do
			"$@" && return 0
			[ "${attempt}" -lt "${attempts}" ] && sleep $((attempt * 10 + RANDOM % 10))
		done
		return 1
	}
	if [ ! -d "${tree}/.git" ]; then
		[ "${free:-0}" -ge 4500 ] || { say "only ${free} MB free, too little to clone"; exit 2; }
		retry git clone -q --filter=blob:none "${repository}" "${tree}" || { say "cloning ${repository} failed"; exit 2; }
	fi
	find "${tree}/.git" -maxdepth 6 -name index.lock -delete 2> /dev/null
	# The commit must be one the public repository holds. git fetch of a sha the checkout already has succeeds without
	# asking the remote, so each commit is first fetched into an empty object store, where only GitHub can supply it (the
	# commit object alone: --depth=1 --filter=tree:0), and only then into the checkout.
	probe=$(mktemp -d "${root}/loom-probe-XXXXXX")
	git init -q --bare "${probe}"
	for commit in "${sha}" ${base:+"${base}"}; do
		if ! retry git -C "${probe}" fetch -q --depth=1 --filter=tree:0 "${repository}" "${commit}"; then
			git -C "${probe}" fetch -q --depth=1 --filter=tree:0 "${repository}" "${commit}" 2>&1 | tail -3
			rm -rf "${probe}"
			say "refused: ${commit} can't be fetched from ${repository} without credentials"
			exit 3
		fi
		retry git -C "${tree}" fetch -q "${repository}" "${commit}" || { rm -rf "${probe}"; say "fetching ${commit} into the checkout failed"; exit 2; }
	done
	rm -rf "${probe}"
	git -C "${tree}" switch -q --detach "${sha}" && [ "$(git -C "${tree}" rev-parse HEAD)" = "${sha}" ] || { say "checking out ${sha} failed"; exit 2; }
	if [ -n "${base}" ] && ! git -C "${tree}" merge-base --is-ancestor "${base}" "${sha}"; then
		say "refused: ${sha} doesn't descend from its base ${base}"
		exit 3
	fi
	# Submodules: each must be on GitHub over HTTPS (after the ssh rewrite), public, fetched with no credentials, by
	# pinUrl's rule at every depth: the tree's own before anything is fetched, and the deeper ones once their parents are
	# checked out (the queue bridge refused any change that breaks it before a runner saw it, so this is the second line).
	pinUrls "${tree}" || exit 3
	if ! git -C "${tree}" submodule update -q --init --recursive; then
		say "the submodule update failed; making the submodules again"
		git -C "${tree}" submodule deinit -q -f --all 2> /dev/null
		rm -rf "${tree}/.git/modules"
		retry git -C "${tree}" submodule update -q --init --recursive || { say "the submodules of ${sha} can't be fetched"; exit 2; }
	fi
	pinUrls "${tree}" || exit 3

	# The toolchain: adamic's own cloud/setup.sh at this commit, once per instance, and only on a machine that is the
	# runner's alone, since it installs into HOME. A shared machine's own toolchain serves, or the unit is unfit there.
	if [ "${owner}" != exclusive ]; then
		[ -f "${HOME}/adamic-tools/env.sh" ] || [ -f "${HOME}/.adamic-tools/env.sh" ] || {
			say "unfit: this machine isn't the runner's alone, so it runs no cloud/setup.sh in its shared HOME, and it has no adamic toolchain (adamic-tools/env.sh)"
			exit 2
		}
	elif [ ! -f "${root}/adamic-setup-done" ]; then
		(cd "${tree}" && bash cloud/setup.sh --wasi-sdk > "${root}/adamic-setup.log" 2>&1) && touch "${root}/adamic-setup-done" || { say "cloud/setup.sh failed"; tail -20 "${root}/adamic-setup.log"; exit 2; }
	fi
fi
toolchain=
for environment in "${HOME}/adamic-tools/env.sh" "${HOME}/.adamic-tools/env.sh"; do
	[ -f "${environment}" ] && { source "${environment}"; toolchain=${environment}; break; }
done
if [ "${mode}" = checkout ]; then
	(cd / && go list fmt testing > /dev/null 2>&1) || { say "the Go toolchain lacks its standard library after setup"; rm -f "${root}/adamic-setup-done"; exit 2; }
elif [ -z "${toolchain}" ]; then
	# A prebuilt unit's tests still run clang and node; an instance without adamic's toolchain would fail them red.
	say "the instance has no adamic toolchain (adamic-tools/env.sh): its tests' clang and node would be missing"
	exit 2
fi
mkdir -p -m 1777 "${TMPDIR:-${root}}"

# stage3/api's npm packages: nothing here installs them (#v03v751). A prebuilt unit's tree holds them already, installed
# on Workshop and unpacked from its source (builder/node.go). A checkout's are linked in from <root>/adamic-npm/<lockfile
# sha256>/node_modules, where the runner placed them from the build its job names (runner/node.go) before this ran; a
# checkout whose lockfile has none placed can't be readied, Loom's, named.
lockfile=${tree}/stage3/api/package-lock.json
if [ "${mode}" = checkout ] && [ -f "${lockfile}" ]; then
	key=$(sha256sum "${lockfile}" | cut -c1-64)
	cache=${root}/adamic-npm/${key} target=${tree}/stage3/api/node_modules
	[ -d "${cache}/node_modules" ] || {
		say "stage3/api's npm packages for lockfile ${key} aren't on this runner: the job names no tree build holding them, and a runner never installs"
		exit 2
	}
	if [ "$(cat "${target}/.fast-gate-lockfile-sha256" 2> /dev/null)" != "${key}" ]; then
		[ -e "${target}" ] && mv "${target}" "${root}/adamic-npm/replaced-$$-${SECONDS}"
		cp -al "${cache}/node_modules" "${target}" && echo "${key}" > "${target}/.fast-gate-lockfile-sha256"
	fi
fi

# The gate inputs, from Loom's public store under gate-inputs/, where `loom gate-inputs publish` writes them and no
# lifecycle expires them (gateinputs/gateinputs.go). Their name, the job's, is the sha256 of their uncompressed tar; the
# manifest by that name lists the chunks of a tar.gz of it, then "total <sha256> <bytes>" of the tar.gz and "tar <name>
# <bytes>". Every chunk, the total and the tar itself are checked by sha256, the last against the job's name, so the
# manifest needn't be trusted. Nothing is fetched until the root has room for the tar.gz and the unpacked inputs above
# the 1500 MB floor. The marker naming what is unpacked goes first and comes back last, and the inputs are unpacked in
# staging and moved into place whole, so a unit that fails anywhere between (a full disk, its deadline) leaves no marker
# naming inputs that aren't there, and the next unit fetches them again. Staging goes when this ends, however it ends,
# short of a kill, whose leavings the next trim takes. With LOOM_HOUSE_CACHE set (docs/house-cache.md), each chunk is
# asked of the house cache first, within 2 s to connect and never under 64 KB a second for 10 s, the Go clients' floor
# (a chunk is up to 90 MiB, so no total limit); the first chunk it doesn't give whole and hashing to its name is read
# from the store, and so is every chunk after it. The manifest, whose name isn't its own hash, always comes from the store.
tools=${root}/adamic-tools inputs=${root}/adamic-tools/gate-inputs
if [ -n "${gateInputs}" ] && [ "$(cat "${tools}/gate-inputs.manifest" 2> /dev/null)" != "${gateInputs}" ]; then
	fetch() { curl -fsS --retry 3 -o "$2" "https://artifacts.loom.system.inc/gate-inputs/$1"; }
	house=${LOOM_HOUSE_CACHE:-}
	chunk() { # chunk <sha256> <path>: from the house cache while it gives each chunk whole, else from the store
		if [ -n "${house}" ]; then
			curl -fsS --connect-timeout 2 --speed-limit 65536 --speed-time 10 -o "$2" "${house%/}/gate-inputs/$1" 2> /dev/null &&
				echo "$1  $2" | sha256sum -c --quiet > /dev/null 2>&1 && return 0
			say "the house cache didn't give gate inputs chunk $1; the store gives it and the rest"
			house=
		fi
		fetch "$1" "$2"
	}
	staging=${tools}/staging-$$ archive=${tools}/staging-$$/gate-inputs.tar.gz
	trap 'rm -rf "${staging}"' EXIT
	rm -f "${tools}/gate-inputs.manifest"
	mkdir -p "${staging}/unpacked" && fetch "${gateInputs}" "${staging}/manifest" || { say "gate inputs manifest ${gateInputs} unreadable"; exit 2; }
	read -r _ total compressed < <(grep '^total ' "${staging}/manifest")
	read -r _ name size < <(grep '^tar ' "${staging}/manifest")
	[[ ${total:-} =~ ^[0-9a-f]{64}$ && ${compressed:-} =~ ^[0-9]{1,15}$ && ${size:-} =~ ^[0-9]{1,15}$ && ${name:-} = "${gateInputs}" ]] ||
		{ say "gate inputs manifest ${gateInputs} names no total, or a tar other than ${gateInputs}"; exit 2; }
	need=$(((compressed + size) / 1048576 + 1500)) free=$(freeMegabytes)
	[ "${free:-0}" -ge "${need}" ] || { say "only ${free} MB free, and the gate inputs need ${need} MB"; exit 2; }
	while read -r hash; do
		[[ ${hash} =~ ^[0-9a-f]{64}$ ]] || { say "gate inputs manifest holds a line that isn't a hash"; exit 2; }
		chunk "${hash}" "${staging}/part" && echo "${hash}  ${staging}/part" | sha256sum -c --quiet && cat "${staging}/part" >> "${archive}" ||
			{ say "gate inputs chunk ${hash} failed"; exit 2; }
	done < <(grep -v -e '^total ' -e '^tar ' "${staging}/manifest")
	rm -f "${staging}/part"
	echo "${total}  ${archive}" | sha256sum -c --quiet || { say "gate inputs total hash differs"; exit 2; }
	[ "$(gzip -dc "${archive}" | sha256sum | cut -c1-64)" = "${gateInputs}" ] || { say "gate inputs tar isn't ${gateInputs}"; exit 2; }
	tar -C "${staging}/unpacked" -xzf "${archive}" && [ -d "${staging}/unpacked/gate-inputs" ] || { say "gate inputs unpack failed"; exit 2; }
	{ [ ! -e "${inputs}" ] || mv "${inputs}" "${staging}/replaced"; } && mv "${staging}/unpacked/gate-inputs" "${inputs}" &&
		echo "${gateInputs}" > "${tools}/gate-inputs.manifest" || { say "the gate inputs couldn't be moved into place"; exit 2; }
	rm -rf "${staging}"
	trap - EXIT
fi
if [ -n "${gateInputs}" ]; then
	export ADAMIC_TYPESCRIPT_SOURCE=${inputs}/typescript ADAMIC_CYCLE_LEDGER_ROOT=${inputs}/cycle-ledger
	export ADAMIC_CYCLE_LEDGER_OUTPUT=${inputs}/cycle-ledger-output.json ADAMIC_CSS_FIXTURES=${inputs}/css-fixtures
	export ADAMIC_CSSNUMBERS_LIBRARY=${inputs}/css-printer ADAMIC_CSSSTRINGS_LIBRARY=${inputs}/css-printer
	export ADAMIC_MARKDOWNINLINE_LIBRARY=${inputs}/css-printer/node_modules/prettier ADAMIC_CSS_LIBRARY=${inputs}/css
	export ADAMIC_GRAPHQL_LIBRARY=${inputs}/graphql ADAMIC_MEDIA_QUERY_LIBRARY=${inputs}/media-query
	export ADAMIC_SELECTOR_LIBRARY=${inputs}/selector ADAMIC_VALUES_LIBRARY=${inputs}/values
	export ADAMIC_GRAPHQL_PRETTIER=${inputs}/css-printer ADAMIC_JSON_PRETTIER=${inputs}/json-prettier
	export ADAMIC_CSS_PRINTER_LIBRARY=${inputs}/css-printer ADAMIC_ESTREE_LIBRARY=${inputs}/css-printer
	export ADAMIC_TS_PRETTIER=${inputs}/css-printer ADAMIC_YAML_LIBRARY=${inputs}/css-printer
	export ADAMIC_GITIGNORE_LARGEST=${inputs}/gitignore/.gitignore
	export PATH="${ADAMIC_TYPESCRIPT_SOURCE}/bin:${PATH}"
fi
# The checker archive the tests link is the tree's own product, built from the bridge they compile against (#nee3cfe).
# A box's env.sh may still export one it built from another tree for its own gate, and a tree that still reads it would
# judge its bridge against that one, so none passes through.
unset ADAMIC_CLANG_TSGO_ARCHIVE

# Every Go module's dependencies from Go's public module proxy first: tests that build a nested module read any
# "go: downloading" on stderr as a failure.
if [ "${mode}" = checkout ]; then
	git -C "${tree}" ls-files --recurse-submodules '*go.mod' | grep -v -e testdata/ -e node_modules/ | while read -r module; do
		(cd "${tree}/$(dirname "${module}")" && go mod download > /dev/null 2>&1)
	done
	say "${sha} ready in $((SECONDS - started)) s"
else
	say "the environment is ready in $((SECONDS - started)) s"
fi
env -0 > "${environmentFile}"
