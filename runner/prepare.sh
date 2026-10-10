#!/bin/bash
# prepare.sh: the runner's own preparation for a test job (strict.go), compiled into the binary and never sent by the
# server. It readies a checkout of the public adamic repository at a verified commit for go test, and writes the
# environment the tests run in. Every value it takes is a positional argument the runner checked first (CheckTestJob),
# quoted at every use; nothing of the job is ever part of this text.
#
#	prepare.sh <tree> <sha> <base or ""> <gate inputs sha256 or ""> <environment file> <trim | keep> <root> <exclusive | shared>
#	prepare.sh environment <tree> <gate inputs sha256 or ""> <environment file> <root>
#	prepare.sh trim-only <root> <exclusive | shared>
#
# environment readies only what a prebuilt test job's binaries run with (prebuilt.go), over <tree>, the tree's source
# the runner already unpacked from the action store: the instance's adamic toolchain (env.sh, which must exist: exit 2
# without it), stage3/api's npm packages and the gate inputs. No checkout, no setup, and nothing of Go.
#
# <root> holds everything it keeps between units beside the tree (the npm trees, the gate inputs, the setup marker) and
# is where it looks for what earlier units left. trim (a strict runner's: one unit at a time) first removes what
# earlier units left there; keep removes nothing. exclusive says the machine is the runner's alone (a Codex instance,
# --exclusive): only then does it clear HOME's adamic runtime builds and Go's build cache too, and run adamic's own
# cloud/setup.sh, which installs into HOME. shared (a house box, which other work shares) touches nothing of HOME but
# what go test itself writes, and a checkout on a machine without adamic's toolchain is unfit there, exit 2, named.
#
# Exit 0: the tree is at <sha> and <environment file> holds the environment, NUL separated. Exit 3: the job is refused
# (the sha or base can't be fetched from the public repository, a submodule isn't public on GitHub); nothing ran. Exit 2:
# the instance couldn't be readied (disk, network, setup): Loom's fault, never the change's.
#
# trim-only runs the trim alone and exits 0: a strict serve that finds its disk too full to take a unit runs it once
# (serve.go, #zzmz489), then looks again.
set -uo pipefail
say() { echo "loom-runner prepare: $*"; }
# Disk: on an instance that runs one unit at a time, what earlier units left on its root is no one's, and on a machine
# that is the runner's alone (exclusive), what they left in HOME's caches too. A shared machine's HOME is other work's.
freeMegabytes() { df -Pm "${HOME}" "${root}" | awk 'NR > 1 {print $4}' | sort -n | head -1; }
trimLeftovers() {
	rm -rf "${root}"/go-build* "${root}"/Test* "${root}"/adamic-npm/replaced-* "${root}"/adamic-npm/*.staging-* 2> /dev/null
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
	# Submodules: each must be on GitHub over HTTPS (after the ssh rewrite), public, fetched with no credentials.
	while read -r _ url; do
		case ${url} in
			https://github.com/* | git@github.com:*) ;;
			*) say "refused: submodule ${url} isn't on GitHub"; exit 3 ;;
		esac
	done < <(git -C "${tree}" config -f .gitmodules --get-regexp '^submodule\..*\.url$' 2> /dev/null)
	if ! git -C "${tree}" submodule update -q --init --recursive; then
		say "the submodule update failed; making the submodules again"
		git -C "${tree}" submodule deinit -q -f --all 2> /dev/null
		rm -rf "${tree}/.git/modules"
		retry git -C "${tree}" submodule update -q --init --recursive || { say "the submodules of ${sha} can't be fetched"; exit 2; }
	fi

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

# stage3/api's pinned npm packages, npm ci from the public registry once per lockfile, hardlinked into the tree.
lockfile=${tree}/stage3/api/package-lock.json
if [ -f "${lockfile}" ]; then
	key=$(sha256sum "${lockfile}" | cut -c1-64)
	cache=${root}/adamic-npm/${key} target=${tree}/stage3/api/node_modules
	if [ ! -d "${cache}/node_modules" ]; then
		staging=${cache}.staging-$$
		mkdir -p "${staging}" && cp "${tree}/stage3/api/package.json" "${lockfile}" "${staging}/"
		(cd "${staging}" && npm_config_update_notifier=false npm ci --ignore-scripts --no-audit --no-fund --install-strategy=hoisted --registry=https://registry.npmjs.org > npm.log 2>&1) && mv "${staging}" "${cache}" || { say "npm ci of stage3/api failed"; tail -20 "${staging}/npm.log"; exit 2; }
	fi
	if [ "$(cat "${target}/.fast-gate-lockfile-sha256" 2> /dev/null)" != "${key}" ]; then
		[ -e "${target}" ] && mv "${target}" "${root}/adamic-npm/replaced-$$-${SECONDS}"
		cp -al "${cache}/node_modules" "${target}" && echo "${key}" > "${target}/.fast-gate-lockfile-sha256"
	fi
fi

# The gate inputs, from Loom's public store by hash under gate-inputs/, where `loom gate-inputs publish` writes them and no
# lifecycle expires them: a manifest of chunks of one tar.gz and its total, every hash checked (gateinputs/gateinputs.go).
tools=${root}/adamic-tools inputs=${root}/adamic-tools/gate-inputs
if [ -n "${gateInputs}" ] && [ "$(cat "${tools}/gate-inputs.manifest" 2> /dev/null)" != "${gateInputs}" ]; then
	fetch() { curl -fsS --retry 3 -o "$2" "https://artifacts.loom.system.inc/gate-inputs/$1" && echo "$1  $2" | sha256sum -c --quiet; }
	staging=${tools}/staging-$$
	mkdir -p "${staging}" && fetch "${gateInputs}" "${staging}/manifest" || { say "gate inputs manifest ${gateInputs} unreadable"; exit 2; }
	while read -r hash; do
		[[ ${hash} =~ ^[0-9a-f]{64}$ ]] || { say "gate inputs manifest holds a line that isn't a hash"; exit 2; }
		fetch "${hash}" "${staging}/part" && cat "${staging}/part" >> "${staging}/gate-inputs.tar.gz" || { say "gate inputs chunk ${hash} failed"; exit 2; }
	done < <(grep -v '^total ' "${staging}/manifest")
	echo "$(sed -n 's/^total //p' "${staging}/manifest")  ${staging}/gate-inputs.tar.gz" | sha256sum -c --quiet || { say "gate inputs total hash differs"; exit 2; }
	[ -e "${inputs}" ] && mv "${inputs}" "${staging}/replaced"
	tar -C "${tools}" -xzf "${staging}/gate-inputs.tar.gz" && echo "${gateInputs}" > "${tools}/gate-inputs.manifest" || { say "gate inputs unpack failed"; exit 2; }
	rm -rf "${staging}"
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
	export ADAMIC_GITIGNORE_LARGEST=${inputs}/gitignore/.gitignore ADAMIC_CLANG_TSGO_ARCHIVE=${inputs}/checker/tsgo.a
	export PATH="${ADAMIC_TYPESCRIPT_SOURCE}/bin:${PATH}"
fi

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
