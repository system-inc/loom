#!/bin/bash
# cutover_test.sh [<cutover.sh>]: every subcommand against stubs, one PASS or FAIL per check, then each planted mutant
# of cutover.sh, which must fail at least one check, and shellcheck when it is installed. Nothing reaches GitHub,
# Workshop or Cloud: gh, ssh, git and systemctl are stubs over a model of the repository (its deploy keys, rulesets,
# GitHub's rules on a branch, its refs) and of the two hosts (their homes, keys, Workshop's timer). The model answers as
# GitHub does: a personal credential is refused with GH013 by an active ruleset over the ref, a deploy key with write
# bypasses it, a read-only key is refused as read only, a deleted one as Permission denied (publickey). ssh resolves
# only Workshop and Cloud, by case, as Kirk's ssh config does, and Cloud's git rewrites git@github.com: to https. Runs on
# Linux and on macOS's bash 3.2; needs jq:
#
#	cutover/cutover_test.sh                 # every check, then every mutant
#	cutover/cutover_test.sh m.sh            # every check against m.sh (how each mutant runs); exits with the failures
# shellcheck disable=SC2016 # each check's condition is single-quoted for check to eval later
set -u
here=$(cd "$(dirname "$0")" && pwd)
script=${1:-${here}/cutover.sh}
script=$(cd "$(dirname "${script}")" && pwd)/$(basename "${script}")
T=$(mktemp -d)
failures=0 worlds=0
command -v jq > /dev/null || { echo "FAIL jq is needed"; exit 1; }

# Each world is a fresh directory, so nothing is ever removed; world() points the stubs at it.
export STUB_GITHUB STUB_HOSTS
export PATH=${T}/bin:${PATH}
mkdir -p "${T}/bin"
landerKey="ssh-ed25519 AAAAC3lander"
gateKey="ssh-ed25519 AAAAC3gate"
mainSha=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
scratchSha=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb

# gh api [-X <method>] <path> [-f|-F <name>=<value>]... [--jq <filter>]
cat > "${T}/bin/gh" << 'STUB'
#!/bin/bash
set -u
g=${STUB_GITHUB}
r=repos/system-inc/adamic
[ "$1" = api ] || exit 9
shift
method=GET path='' filter='' fields=''
while [ $# -gt 0 ]; do
	case $1 in
	-X) method=$2; shift 2 ;;
	--jq) filter=$2; shift 2 ;;
	-f | -F) fields="${fields}$2
"; shift 2 ;;
	*) path=$1; shift ;;
	esac
done
echo "gh ${method} ${path} $(echo "${fields}" | tr '\n' ' ')" >> "${g}/calls.log"
if [ -n "${STUB_GH_FAIL:-}" ] && echo "${method} ${path}" | grep -qE "${STUB_GH_FAIL}"; then
	echo "gh: Server Error (HTTP 502)" >&2
	exit 1
fi
field() { echo "${fields}" | sed -n "s/^$1=//p" | head -n 1; }
missing() { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
case "${method} ${path}" in
"GET ${r}/keys") out=$(cat "${g}/keys.json") ;;
"GET ${r}/keys/"*)
	out=$(jq -ce --argjson id "${path##*/}" '.[] | select(.id == $id)' "${g}/keys.json") || missing
	;;
"DELETE ${r}/keys/"*)
	jq -e --argjson id "${path##*/}" 'any(.id == $id)' "${g}/keys.json" > /dev/null || missing
	jq --argjson id "${path##*/}" 'map(select(.id != $id))' "${g}/keys.json" > "${g}/keys.new" && mv "${g}/keys.new" "${g}/keys.json"
	out=''
	;;
"POST ${r}/keys")
	id=$(cat "${g}/nextkey")
	echo $((id + 1)) > "${g}/nextkey"
	jq --argjson id "${id}" --arg title "$(field title)" --arg key "$(field key)" --argjson readOnly "$(field read_only)" \
		'. + [{id: $id, title: $title, key: $key, read_only: $readOnly}]' "${g}/keys.json" > "${g}/keys.new" && mv "${g}/keys.new" "${g}/keys.json"
	out=$(jq -c --argjson id "${id}" '.[] | select(.id == $id)' "${g}/keys.json")
	;;
"GET ${r}/rulesets/"*)
	[ -f "${g}/rulesets/${path##*/}.json" ] || missing
	out=$(cat "${g}/rulesets/${path##*/}.json")
	;;
"PUT ${r}/rulesets/"*)
	[ -f "${g}/rulesets/${path##*/}.json" ] || missing
	jq --arg enforcement "$(field enforcement)" '.enforcement = $enforcement' "${g}/rulesets/${path##*/}.json" > "${g}/ruleset.new" && mv "${g}/ruleset.new" "${g}/rulesets/${path##*/}.json"
	out=$(cat "${g}/rulesets/${path##*/}.json")
	;;
"GET ${r}/rules/branches/"*)
	if [ -n "${STUB_RULES_EMPTY:-}" ]; then
		out='[]'
	else
		out=$(jq -s --arg ref "refs/heads/${path#"${r}"/rules/branches/}" \
			'[.[] | select(.enforcement == "active" and (.conditions.ref_name.include | index($ref))) | .id as $id | .rules[] | {type, ruleset_id: $id}]' "${g}"/rulesets/*.json)
	fi
	;;
"GET ${r}/git/ref/heads/"*)
	ref=${path#"${r}"/git/ref/heads/}
	[ -f "${g}/refs/${ref}" ] || missing
	out="{\"object\":{\"sha\":\"$(cat "${g}/refs/${ref}")\"}}"
	;;
*) echo "gh stub: no ${method} ${path}" >&2; exit 9 ;;
esac
if [ -n "${filter}" ]; then echo "${out}" | jq -r "${filter}"; else echo "${out}"; fi
STUB

# ssh <host> <command>...: the command runs here, in that host's home, with its name for the git stub.
cat > "${T}/bin/ssh" << 'STUB'
#!/bin/bash
host=$1
shift
echo "ssh ${host} $*" >> "${STUB_GITHUB}/calls.log"
case ${host} in
Workshop | Cloud) ;;
*) echo "ssh: Could not resolve hostname ${host}: nodename nor servname provided, or not known" >&2; exit 255 ;;
esac
case " ${STUB_DOWN:-} " in *" ${host} "*) echo "ssh: connect to host ${host} port 22: Operation timed out" >&2; exit 255 ;; esac
# A host that refuses the probe's own ssh (an agent without the key), as GitHub's refusal would be worded.
case "${STUB_PROBE_DENIED:-} $1" in "${host} bash") echo "${host}: Permission denied (publickey)." >&2; exit 255 ;; esac
cd "${STUB_HOSTS}/${host}" || exit 255
HOME=${STUB_HOSTS}/${host} STUB_IDENTITY_HOST=${host} exec bash -c "$*"
STUB

# systemctl --user show|enable|disable|start|stop [--now] [-p <property>]... <unit>: Workshop's units, one file each
# holding "<ActiveState> <UnitFileState>".
cat > "${T}/bin/systemctl" << 'STUB'
#!/bin/bash
echo "systemctl $*" >> "${STUB_GITHUB}/calls.log"
[ "$1" = --user ] || exit 9
[ -z "${STUB_SYSTEMCTL_FAIL:-}" ] || { echo "Failed to connect to bus: No medium found" >&2; exit 1; }
verb=$2 now=no
for argument in "$@"; do
	unit=${argument}
	[ "${argument}" != --now ] || now=yes
done
file=${HOME}/systemd/${unit}
if [ "${verb}" = show ]; then
	if [ -f "${file}" ]; then
		read -r active enabled < "${file}"
		printf 'LoadState=loaded\nActiveState=%s\nUnitFileState=%s\n' "${active}" "${enabled}"
	else
		printf 'LoadState=not-found\nActiveState=inactive\nUnitFileState=\n'
	fi
	exit 0
fi
[ -f "${file}" ] || { echo "Failed to ${verb} unit: Unit file ${unit} does not exist." >&2; exit 1; }
read -r active enabled < "${file}"
case ${verb} in
enable) enabled=enabled; [ "${now}" = no ] || active=active ;;
disable) enabled=disabled; [ "${now}" = no ] || active=inactive ;;
start) active=active ;;
stop) active=inactive ;;
*) exit 9 ;;
esac
echo "${active} ${enabled}" > "${file}"
STUB

# git, as the probe uses it, against the model: who pushes is the host's key (the lander's through github-lander on
# Workshop, the gate key on Cloud) or, on this Mac, Kirk's own credential.
cat > "${T}/bin/git" << 'STUB'
#!/bin/bash
g=${STUB_GITHUB}
directory=.
if [ "$1" = -C ]; then directory=$2; shift 2; fi
verb=$1
shift
github() { # <url>: who the push or fetch authenticates as, or a refusal before GitHub reads a ref
	[ -z "${STUB_NETWORK_DOWN:-}" ] || { echo "ssh: Could not resolve hostname github.com: nodename nor servname provided" >&2; return 128; }
	case ${STUB_IDENTITY_HOST:-local} in
	local)
		[ -z "${STUB_KIRK_NO_AUTH:-}" ] || { echo "git@github.com: Permission denied (publickey)." >&2; return 128; }
		who=person
		return 0
		;;
	Workshop) case $1 in *github-lander*) keyFile=${HOME}/.ssh/loom_lander.pub ;; *) echo "git@github.com: Permission denied (publickey)." >&2; return 128 ;; esac ;;
	Cloud) case $1 in git@github.com:*) echo "fatal: could not read Username for 'https://github.com': No such device or address" >&2; return 128 ;; esac; keyFile=${HOME}/.ssh/adamic_deploy.pub ;;
	esac
	key=$(awk '{print $1" "$2}' "${keyFile}")
	readOnly=$(jq -r --arg key "${key}" '.[] | select(.key == $key) | .read_only' "${g}/keys.json")
	[ -n "${readOnly}" ] || { printf 'git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.\n' >&2; return 128; }
	if [ "${readOnly}" = true ]; then who=readOnlyKey; else who=writeKey; fi
}
case ${verb} in
init) mkdir -p "$3" ;; # -q --bare <directory>
clone) # -q --bare --shared <source> <directory>
	[ -d "$4" ] || { echo "fatal: repository '$4' does not exist" >&2; exit 128; }
	mkdir -p "$5" && cp "$4/tips" "$4/main" "$5/"
	;;
fetch) # -q --depth 1 --no-tags <url> <sha>
	github "$5" || exit 128
	echo "$6" >> "${directory}/tips"
	;;
cat-file) grep -qx "${2%%^*}" "${directory}/tips" 2> /dev/null ;; # -e <sha>^{tree}
rev-parse) cat "${directory}/main" ;;
commit-tree)
	count=$(cat "${g}/commits" 2> /dev/null || echo 0)
	echo $((count + 1)) > "${g}/commits"
	printf 'c0ffee%034x\n' $((count + 1))
	;;
push)
	url=$1 sha=${2%%:*} ref=${2#*:refs/heads/}
	github "${url}" || exit 128
	if [ "${who}" = person ] && [ -n "${STUB_KIRK_NO_WRITE:-}" ]; then
		printf 'ERROR: Permission to system-inc/adamic.git denied to kirkouimet.\nfatal: Could not read from remote repository.\n' >&2
		exit 128
	fi
	if [ "${who}" = readOnlyKey ]; then
		printf 'ERROR: The key you are authenticating with has been marked as read only.\nfatal: Could not read from remote repository.\n' >&2
		exit 128
	fi
	if [ "${who}" = person ] && [ -z "${STUB_RULESET_IGNORED:-}" ] &&
		jq -se --arg ref "refs/heads/${ref}" 'any(.[]; .enforcement == "active" and (.conditions.ref_name.include | index($ref)))' "${g}"/rulesets/*.json > /dev/null; then
		printf 'remote: error: GH013: Repository rule violations found for refs/heads/%s.\nTo %s\n ! [remote rejected] %s -> %s (push declined due to repository rule violations)\n' "${ref}" "${url}" "${sha}" "${ref}" >&2
		exit 1
	fi
	[ -z "${STUB_PUSH_NOOP:-}" ] || { echo "Everything up-to-date" >&2; exit 0; }
	echo "${sha}" > "${g}/refs/${ref}"
	printf 'To %s\n   %s..%s  %s -> %s\n' "${url}" "${3:-tip}" "${sha:0:7}" "${sha}" "${ref}" >&2
	;;
*) echo "git stub: no ${verb}" >&2; exit 9 ;;
esac
STUB
chmod 755 "${T}/bin/"*

ruleset() { # <id> <enforcement> <ref>
	jq -n --argjson id "$1" --arg enforcement "$2" --arg ref "refs/heads/$3" \
		'{id: $id, name: "scratch", target: "branch", enforcement: $enforcement, conditions: {ref_name: {include: [$ref], exclude: []}},
		rules: [{type: "update"}, {type: "deletion"}, {type: "non_fast_forward"}], bypass_actors: [{actor_id: null, actor_type: "DeployKey", bypass_mode: "always"}]}' \
		> "${STUB_GITHUB}/rulesets/$1.json"
}
timer() { echo "$1 $2" > "${STUB_HOSTS}/Workshop/systemd/loom-pusher.timer"; }
key() { # <id> <read_only> <key>: one more deploy key
	jq --argjson id "$1" --argjson readOnly "$2" --arg key "$3" '. + [{id: $id, title: "key \($id)", key: $key, read_only: $readOnly}]' \
		"${STUB_GITHUB}/keys.json" > "${T}/keys.new" && mv "${T}/keys.new" "${STUB_GITHUB}/keys.json"
}
dropKey() { jq --argjson id "$1" 'map(select(.id != $id))' "${STUB_GITHUB}/keys.json" > "${T}/keys.new" && mv "${T}/keys.new" "${STUB_GITHUB}/keys.json"; }

# Today's house (Oct 10): main's ruleset disabled, the lander's key with write, the gate key read-only, the pusher's
# timer stopped and disabled, and a scratch ruleset over loom-rehearsal. `world on` is main's switch wholly on.
world() {
	worlds=$((worlds + 1))
	STUB_GITHUB=${T}/${worlds}/github STUB_HOSTS=${T}/${worlds}/hosts
	mkdir -p "${STUB_GITHUB}/rulesets" "${STUB_GITHUB}/refs" "${STUB_HOSTS}/Workshop/.ssh" "${STUB_HOSTS}/Workshop/systemd" \
		"${STUB_HOSTS}/Workshop/loom-lander/adamic.git" "${STUB_HOSTS}/Cloud/.ssh"
	: > "${STUB_GITHUB}/calls.log"
	echo 170000000 > "${STUB_GITHUB}/nextkey"
	echo "${landerKey} ahra@Workshop" > "${STUB_HOSTS}/Workshop/.ssh/loom_lander.pub"
	echo "${gateKey} ahra@Cloud" > "${STUB_HOSTS}/Cloud/.ssh/adamic_deploy.pub"
	echo "${mainSha}" > "${STUB_HOSTS}/Workshop/loom-lander/adamic.git/tips"
	echo "${mainSha}" > "${STUB_HOSTS}/Workshop/loom-lander/adamic.git/main"
	jq -n --arg lander "${landerKey}" --arg gate "${gateKey}" \
		'[{id: 165969193, title: "threadripper gate box", key: $gate, read_only: true}, {id: 165969878, title: "loom lander", key: $lander, read_only: false}]' \
		> "${STUB_GITHUB}/keys.json"
	ruleset 24823318 disabled main
	ruleset 900 disabled loom-rehearsal
	echo "${mainSha}" > "${STUB_GITHUB}/refs/main"
	echo "${scratchSha}" > "${STUB_GITHUB}/refs/loom-rehearsal"
	timer inactive disabled
	if [ "${1:-}" = on ]; then
		ruleset 24823318 active main
		timer active enabled
	fi
}

run() { "${script}" "$@" > "${T}/run.log" 2>&1; echo $? > "${T}/code"; }
code() { [ "$(cat "${T}/code")" = "$1" ]; }
said() { grep -qE -- "$1" "${T}/run.log"; }
called() { grep -qE -- "$1" "${STUB_GITHUB}/calls.log"; }
enforcement() { [ "$(jq -r .enforcement "${STUB_GITHUB}/rulesets/$1.json")" = "$2" ]; }
timerIs() { [ "$(cat "${STUB_HOSTS}/Workshop/systemd/loom-pusher.timer")" = "$1 $2" ]; }
keyIs() { [ "$(jq -r --argjson id "$1" '.[] | select(.id == $id) | .read_only' "${STUB_GITHUB}/keys.json")" = "$2" ]; }
refIs() { [ "$(cat "${STUB_GITHUB}/refs/$1")" = "$2" ]; }
check() {
	if eval "$2"; then
		echo "PASS $1"
	else
		echo "FAIL $1"
		sed 's/^/    /' "${T}/run.log"
		failures=$((failures + 1))
	fi
}
scratch=(--ref loom-rehearsal --ruleset 900)

# check
world; run check
check "check: today's house reads off" 'code 0 && said "main is off"'
check "check: ssh says Workshop and Cloud, never workshop or threadripper" '! called "^ssh (workshop|threadripper|cloud) " && called "^ssh Workshop "'
world on; run check
check "check: main wholly on reads on" 'code 0 && said "main is on"'
world on; timer active disabled; run check
check "check: a timer started but not enabled reads mixed, since a reboot would stop landing" 'code 1 && said "main is mixed"'
world on; timer inactive enabled; run check
check "check: a timer enabled but stopped reads mixed" 'code 1 && said "main is mixed"'
world; timer active enabled; run check
check "check: the ruleset disabled while the pusher runs reads mixed" 'code 1 && said "main is mixed"'
world on; STUB_RULES_EMPTY=1 run check
check "check: an active ruleset GitHub doesn't apply to main reads mixed" 'code 1 && said "main is mixed"'
world on; run check --gate-key 165739542
check "check: the gone gate key 165739542 reads mixed" 'code 1 && said "gate key 165739542 missing"'
world on; key 42 false "ssh-ed25519 AAAAC3other"; run check
check "check: another write key reads mixed" 'code 1 && said "write key 42 isn.t the lander.s"'
world; STUB_DOWN=Workshop run check
check "check: Workshop unreachable reads as neither on nor off" 'code 3 && said "neither on nor off" && ! said "main is (on|off)"'
world on; STUB_GH_FAIL="GET .*/keys$" run check
check "check: an unreadable key list reads as neither" 'code 3 && ! said "main is (on|off)"'
world; STUB_SYSTEMCTL_FAIL=1 run check
check "check: a systemd that can't be asked reads as neither" 'code 3 && ! said "main is (on|off)"'
world; run check --timer loom-push.timer
check "check: the timer is a setting, and one with no unit file reads as stopped" 'code 0 && said "loom-push.timer not-found" && called "show .*loom-push.timer"'

# on
world; run on
check "on: from today's house, main is on" 'code 0 && said "main is on" && enforcement 24823318 active'
check "on: the pusher's timer is enabled, not only started, so a reboot keeps landing" 'timerIs active enabled && called "enable --now loom-pusher.timer"'
check "on: no key is rewritten when the keys are already in on's shape" '! called "^gh (DELETE|POST) .*/keys"'
check "on: no Mac launch agent is asked" '! called launchctl && ! grep -q launchctl "${script}"'
world; key 42 false "ssh-ed25519 AAAAC3other"; run on
check "on: another write key goes read-only, the same key and title" 'code 0 && [ "$(jq -r ".[] | select(.key == \"ssh-ed25519 AAAAC3other\") | \"\(.read_only) \(.title)\"" "${STUB_GITHUB}/keys.json")" = "true key 42" ]'
world; dropKey 165969878; run on
check "on: a missing lander key is added with write" 'code 0 && [ "$(jq -r ".[] | select(.key == \"${landerKey}\") | .read_only" "${STUB_GITHUB}/keys.json")" = false ]'
world; STUB_SYSTEMCTL_FAIL=1 run on
check "on: a timer that can't be enabled fails on" '! code 0 && said "neither|stopped|can.t"'
world; STUB_DOWN=Workshop run on
check "on: Workshop unreachable changes nothing" 'code 3 && enforcement 24823318 disabled && ! called "^gh (PUT|POST|DELETE)"'

# off
world on; run off
check "off: from on, main is off" 'code 0 && said "main is off" && enforcement 24823318 disabled'
check "off: the pusher's timer is stopped and disabled" 'timerIs inactive disabled && called "disable --now loom-pusher.timer"'
check "off: the lander's key stays unless asked" 'keyIs 165969878 false && ! called "^gh DELETE"'
check "off: no Mac launch agent is asked" '! called launchctl'
world on; run off --delete-lander-key
check "off --delete-lander-key: the lander's key is gone" 'code 0 && [ -z "$(jq -r ".[] | select(.id == 165969878)" "${STUB_GITHUB}/keys.json")" ] && keyIs 165969193 true'
world on; STUB_DOWN=Workshop run off
check "off: Workshop unreachable still disables the ruleset, and fails" '! code 0 && enforcement 24823318 disabled'
world on; STUB_GH_FAIL="PUT .*/rulesets/" run off
check "off: a ruleset that won't disable still stops the timer, and fails" '! code 0 && timerIs inactive disabled'
world on; rm "${STUB_HOSTS}/Workshop/systemd/loom-pusher.timer"; run off
check "off: no timer's unit file is nothing to stop" 'code 0 && said "main is off"'

# rehearsal
world; run on "${scratch[@]}"
check "rehearsal on: the scratch ruleset is active and loom-rehearsal reads on" 'code 0 && said "loom-rehearsal is on" && enforcement 900 active && enforcement 24823318 disabled'
check "rehearsal on: no key written, no timer touched, main's ruleset untouched" '! called "^gh (DELETE|POST)" && ! called "^gh PUT .*/24823318" && ! called "systemctl --user (enable|disable|start|stop)"'
world; ruleset 900 active loom-rehearsal; run off "${scratch[@]}"
check "rehearsal off: the scratch ruleset is disabled, and only it" 'code 0 && said "loom-rehearsal is off" && enforcement 900 disabled && ! called "^gh (DELETE|POST)" && ! called "^gh PUT .*/24823318" && ! called "systemctl --user (enable|disable|start|stop)"'
world; key 42 false "ssh-ed25519 AAAAC3other"; run on "${scratch[@]}"
check "rehearsal on: another write key reads mixed, and isn't rewritten" 'code 1 && keyIs 42 false && ! called "^gh (DELETE|POST)"'
world; ruleset 900 disabled main; run on "${scratch[@]}"
check "rehearsal: a scratch ruleset over main is refused before any write" '! code 0 && said "targets refs/heads/main, not refs/heads/loom-rehearsal" && ! called "^gh PUT"'
world; run on --ref loom-rehearsal
check "rehearsal: --ref without --ruleset is refused" 'code 2 && ! called "^gh PUT"'
world; run on --ref loom-rehearsal --ruleset 24823318
check "rehearsal: main's ruleset on a scratch ref is refused" 'code 2 && ! called "^gh PUT"'
world; run off "${scratch[@]}" --delete-lander-key
check "rehearsal: --delete-lander-key is refused" 'code 2 && ! called "^gh"'

# prove
world on; run prove
check "prove on: Kirk's credential refused by GH013, Cloud's gate key as read only" 'code 0 && said "refused, as it must be: Kirk.s credential.*GH013" && said "refused, as it must be: Cloud.s gate key.*read only" && refIs main "${mainSha}"'
world on; STUB_DOWN=Cloud run prove
check "prove on: Cloud unreachable is no proof" 'code 1 && said "no proof: Cloud"'
world on; STUB_RULESET_IGNORED=1 run prove
check "prove on: a push that lands is a failed proof" 'code 1 && said "PROOF FAILED: Kirk"'
world on; STUB_KIRK_NO_AUTH=1 run prove
check "prove on: Kirk's credential missing is no proof" 'code 1 && said "no proof: Kirk"'
world on; STUB_KIRK_NO_WRITE=1 run prove
check "prove on: Kirk refused for want of write isn't the ruleset's refusal" 'code 1 && said "not with /GH013/"'
world on; STUB_NETWORK_DOWN=1 run prove
check "prove on: a push that never reached GitHub is no proof" 'code 1 && said "no proof"'
world; dropKey 165969878; run prove
check "prove off, lander key deleted: GitHub refuses the lander" 'code 0 && said "refused, as it must be: the lander.s key.*publickey"'
world; dropKey 165969878; STUB_PROBE_DENIED=Workshop run prove
check "prove off: ssh refused by Workshop itself isn't GitHub's refusal" 'code 1 && said "no proof: the lander"'
world; run prove
check "prove off, lander key kept: nothing must be refused, and it says why" 'code 0 && said "quiet because loom-pusher.timer is inactive and disabled" && ! called "bash -s"'
world on; timer active disabled; run prove
check "prove: mixed runs no probe" 'code 1 && said "mixed, so no probe runs" && ! called "bash -s"'
world on; ruleset 900 active loom-rehearsal; run prove "${scratch[@]}"
check "prove, rehearsal on: refusals on loom-rehearsal, main untouched" 'code 0 && refIs loom-rehearsal "${scratchSha}" && refIs main "${mainSha}"'

# accept
world; ruleset 900 active loom-rehearsal; run accept "${scratch[@]}"
check "accept, rehearsal on: the lander lands on loom-rehearsal, and it reads back" 'code 0 && said "landed, as it must: the lander" && ! refIs loom-rehearsal "${scratchSha}" && refIs main "${mainSha}"'
world; run accept "${scratch[@]}"
check "accept, rehearsal off: Kirk's credential lands" 'code 0 && said "landed, as it must: Kirk"'
world; ruleset 900 active loom-rehearsal; STUB_DOWN=Workshop run accept "${scratch[@]}"
check "accept: Workshop unreachable fails, never passes" 'code 1 && refIs loom-rehearsal "${scratchSha}"'
world; ruleset 900 active loom-rehearsal; dropKey 165969878; key 7 true "${landerKey}"; key 8 false "ssh-ed25519 AAAAC3x"; run accept "${scratch[@]}"
check "accept: mixed runs no probe" 'code 1 && said "mixed, so no probe runs"'
world; ruleset 900 active loom-rehearsal; STUB_NETWORK_DOWN=1 run accept "${scratch[@]}"
check "accept: a push that never reached GitHub fails" 'code 1 && said "ACCEPT FAILED"'
world; STUB_KIRK_NO_WRITE=1 run accept "${scratch[@]}"
check "accept: a refused push fails" 'code 1 && said "ACCEPT FAILED: Kirk.*refusedOtherwise"'
world; STUB_PUSH_NOOP=1 run accept "${scratch[@]}"
check "accept: a push that leaves the ref where it was fails" 'code 1 && said "not the probe"'
world on; run accept
check "accept never runs on main" 'code 2 && said "never runs on main" && ! called "bash -s"'

if [ $# -ge 1 ]; then
	exit "${failures}"
fi
if [ "${failures}" != 0 ]; then
	echo "SKIP mutants: the checks fail on cutover.sh itself"
	echo "${failures} failed"
	exit 1
fi

# Each mutant undoes one guarantee and must fail at least one check above.
mutant() { # <name> <sed expression>...
	local name=$1
	shift
	sed "$@" "${script}" > "${T}/mutant.sh"
	chmod 755 "${T}/mutant.sh"
	if cmp -s "${script}" "${T}/mutant.sh"; then
		echo "FAIL mutant ${name}: the sed changed nothing"
		failures=$((failures + 1))
	elif "$0" "${T}/mutant.sh" > "${T}/mutant.log" 2>&1; then
		echo "FAIL mutant ${name}: every check passed"
		failures=$((failures + 1))
	else
		echo "PASS mutant ${name}: $(grep -c '^FAIL' "${T}/mutant.log") checks fail"
	fi
}
mutant "the lander's host in lowercase" -e 's/^landerHost=Workshop$/landerHost=workshop/'
mutant "the gate's host by its old name" -e 's/^gateHost=Cloud$/gateHost=threadripper/'
mutant "the gone gate key" -e 's/^gateKey=165969193$/gateKey=165739542/'
mutant "accept as the negated proof it was" -e 's/^		mustLand "\(.*\)" "\(.*\)" "\(.*\)"$/		! mustRefuse "\1" "\2" "\3" GH013/'
mutant "accept without reading the ref back" -e 's/if \[ "\${tip}" != "\${probeSha}" \]; then/if false; then/'
mutant "on only starts the timer" -e 's/systemctl --user enable --now/systemctl --user start/'
mutant "off only stops the timer" -e 's/systemctl --user disable --now/systemctl --user stop/'
mutant "off deletes the lander's key unasked" -e 's/^			if \[ "\${deleteLanderKey}" = yes \]; then$/			if true; then/'
mutant "a rehearsal writes keys and the timer" -e 's/^		if \[ "\${rehearsal}" = no \]; then$/		if true; then/'
mutant "a ruleset over another ref is flipped" -e 's/if \[ "\${target}" != "refs\/heads\/\${reference}" \]; then/if false; then/'
mutant "check reads an unreadable switch" -e "s/readState || { say \"can't read the switch, so it reads as neither on nor off\"; exit 3; }/readState || true/"
mutant "off ignores the timer" -e 's/{ \[ "\${rehearsal}" = yes \] || timerOff; }/true/'
mutant "on ignores the timer" -e 's/{ \[ "\${rehearsal}" = yes \] || timerOn; }/true/'
mutant "on without GitHub's view" -e 's/ \&\& \[ "\${applied}" -gt 0 \]//'
mutant "a refusal read from before the push" -e 's/pushed=\${probeOutput#\*push begins}/pushed=${probeOutput}/' \
	-e 's/case \${probeOutput} in \*"push begins"\*"push ended "\*) ;; \*) return ;; esac/:/'
mutant "any refusal of Kirk counts" -e "s/'GH013' || failed=1/'.' || failed=1/"

if command -v shellcheck > /dev/null; then
	if shellcheck "${script}" "$0" > "${T}/run.log" 2>&1; then echo "PASS shellcheck"; else check "shellcheck" false; fi
else
	echo "SKIP shellcheck: not installed"
fi

echo "${failures} failed"
[ "${failures}" = 0 ]
