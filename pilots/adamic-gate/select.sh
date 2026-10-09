# select.sh: the body of a fast gate's selection unit (fast.sh, for developer tools' jobs whose packages are
# "select"). After the opening readies the candidate, it checks out the tools sha beside the tree and runs the
# gate's own selection, which runs nothing and writes select.json: the packages, the env, the tests to skip or keep.
# The selection writes into a fixed directory, /tmp/loom-select/<sha>, because its env names a file there
# (ADAMIC_GATE_CHANGED) and every test unit recreates that directory at the same path from select.tgz.
# Arguments: <base> <base name> <tools sha>. Exit 1 with no select.json is the change's red, never Loom's.
base=$1 baseName=$2 toolsSha=$3
toolsTree=/tmp/adamic-tools-checkout
retry git -C "${tree}" fetch -q origin "${toolsSha}" || { echo "loom-select: fetching tools ${toolsSha} failed"; exit 2; }
if [ -d "${toolsTree}" ]; then
  git -C "${toolsTree}" switch -q --detach "${toolsSha}" || { echo "loom-select: tools checkout failed"; exit 2; }
else
  git -C "${tree}" worktree add -q --detach "${toolsTree}" "${toolsSha}" || { echo "loom-select: tools worktree failed"; exit 2; }
fi
selection=/tmp/loom-select/${sha}
[ -e "${selection}" ] && mv "${selection}" "${selection}.replaced-$$"
mkdir -p "${selection}"
python3 "${toolsTree}/cloud/fast-gate/run.py" --select --tree "${tree}" --sha "${sha}" --base "${base}" --base-name "${baseName}" --tools "${toolsTree}" --out "${selection}" > "${out}/select.stdout" 2>&1
code=$?
tar -C "${selection}" -czf "${out}/select.tgz" .
echo "loom-select: run.py --select exited ${code}; $( [ -f "${selection}/select.json" ] && echo "select.json written" || echo "no select.json")"
tail -5 "${out}/select.stdout"
[ -f "${selection}/select.json" ] && exit 0
[ "${code}" = 1 ] && exit 1
exit 2
