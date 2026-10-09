#!/bin/bash
# detach.sh: start a run's driver in a session of its own, so the window that started it can end without taking the
# driver with it (@system_adamic, Oct 9: p3's coordinator died with its window and the second parity proof was void).
# The command's stdin is /dev/null and its output appends to the log; the driver's pid is printed.
#
#	pilots/adamic-gate/detach.sh <log> <command> [arguments...]    # a copy in ~/.loom/bin
set -euo pipefail
[ $# -ge 2 ] || { echo "usage: detach.sh <log> <command> [arguments...]" >&2; exit 2; }
log=$1
shift
# A child of a shell without job control is never its process group's leader, so setsid always succeeds here.
python3 -c '
import os, sys
os.setsid()
output = os.open(sys.argv[1], os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o644)
os.dup2(output, 1)
os.dup2(output, 2)
os.dup2(os.open("/dev/null", os.O_RDONLY), 0)
os.execvp(sys.argv[2], sys.argv[2:])
' "${log}" "$@" &
echo $!
