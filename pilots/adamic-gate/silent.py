#!/usr/bin/env python3
"""silent.py: a candidate the fast server isn't moving pages Loom and its owner (@system_adamic, Oct 9 07:04: three
candidates went silent tonight three ways. C emission 3fefe5b2's void was never requeued; it read red on main's own
reds alone. after-chain e628f527 and hidden-boundaries 7fddff4b had job files and no run for an hour, at tier 0
behind side work).

	pilots/adamic-gate/silent.py [--dry]     # every minute from launchd (com.loom.silent); a copy in ~/.loom/bin
	pilots/adamic-gate/silent.py --seed      # remember every job silent now without paging: superseded recuts and
	                                         # abandoned gates (52 at its start, Oct 9 13:10Z) are nobody's to serve

A candidate is a fast job on a cloud/land- branch, or at tier 30 and up. Each pages, once per job and kind:
- unplaced: its job file is over 5 minutes old, with no .running and no .verdict.
- unrequeued: its verdict is void, or red only on main's own reds (fast.sh's ceiling rule reads them the same way),
  and has sat over 5 minutes without being served again.
A page goes to Loom and to the gate-lane's owner (@system_adamic_integration), naming the job, its tier and what it
waits on. ~/.loom/silent.json remembers what was paged; a job that moves (starts, or gets a new verdict) can page again.
"""

import json
import os
import re
import subprocess
import sys
import time

jobs = os.environ.get("LOOM_SILENT_JOBS") or os.path.expanduser("~/.loom/jobs/fast")
state = os.environ.get("LOOM_SILENT_STATE") or os.path.expanduser("~/.loom/silent.json")
ahra = "/Users/kirkouimet/Projects/ahra/node_modules/.bin/ahra"
recipients = ["system_adamic_developer_tools_loom", "system_adamic_integration"]
grace = 300


def mainReds():
    """Main's own named reds: the ruled list, and every red in main's pool records (main.log names them)."""
    names = set()
    ruled = os.path.expanduser("~/.loom/canary/main-reds.txt")
    for line in open(ruled) if os.path.exists(ruled) else []:
        if line.strip() and not line.startswith("#"):
            names.add(line.strip())
    log = os.path.expanduser("~/.loom/main.log")
    for line in open(log, errors="replace") if os.path.exists(log) else []:
        match = re.search(r" pre-gate of ([0-9a-f]{40}) ", line)
        reds = match and os.path.expanduser("~/.loom/pregate/%s.reds.txt" % match.group(1))
        for red in open(reds, errors="replace") if reds and os.path.exists(reds) else []:
            found = re.match(r"^FAIL (\S+) (\S+)", red)
            if found and not found.group(2).startswith("("):
                names.add(found.group(1) + " " + found.group(2))
    return names


def onlyMains(sha, mains):
    """True when the job's red list names reds and every one is main's own."""
    path = os.path.join(jobs, sha + ".work", "reds.txt")
    fails = [line.split() for line in open(path, errors="replace") if line.startswith("FAIL ")] if os.path.exists(path) else []
    return bool(fails) and all(len(fail) > 2 and fail[1] + " " + fail[2] in mains for fail in fails)


def main():
    dry, seed = "--dry" in sys.argv, "--seed" in sys.argv
    paged = json.load(open(state)) if os.path.exists(state) else {}
    now, mains, pages = time.time(), None, []
    for name in sorted(os.listdir(jobs)):
        if not re.fullmatch(r"[0-9a-f]{40}\.json", name):
            continue
        sha = name[:-5]
        try:
            job = json.load(open(os.path.join(jobs, name)))
        except (OSError, ValueError):
            continue
        tier = int(job.get("priority") or 0)
        if not (str(job.get("branch", "")).startswith("cloud/land-") or tier >= 30):
            continue
        running, verdict = os.path.join(jobs, sha + ".running"), os.path.join(jobs, sha + ".verdict")
        # A running job is moving, and a cancelled one was stopped on purpose (its void says so).
        if os.path.exists(running) or os.path.exists(os.path.join(jobs, sha + ".cancelled")):
            continue
        what = None
        if not os.path.exists(verdict):
            if now - os.path.getmtime(os.path.join(jobs, name)) > grace:
                what = ("unplaced", os.path.getmtime(os.path.join(jobs, name)),
                        "its job file is %d minutes old with no run" % ((now - os.path.getmtime(os.path.join(jobs, name))) // 60))
        elif now - os.path.getmtime(verdict) > grace:
            first = open(verdict, errors="replace").readline()
            if first.startswith("void:"):
                what = ("unrequeued", os.path.getmtime(verdict), "its void verdict has sat %d minutes, not served again" % ((now - os.path.getmtime(verdict)) // 60))
            elif first.startswith("red:"):
                mains = mainReds() if mains is None else mains
                if onlyMains(sha, mains):
                    what = ("unrequeued", os.path.getmtime(verdict), "it reads red only on main's own reds and has sat %d minutes, not served again" % ((now - os.path.getmtime(verdict)) // 60))
        if not what:
            continue
        key = "%s %s %d" % (sha, what[0], what[1])
        if key in paged:
            continue
        paged[key] = int(now)
        pages.append("Silent candidate: %s (%s), tier %d: %s. %s" % (job.get("branch", "?"), sha[:12], tier, what[2],
                     "Serve it (rename its .verdict) or say why it waits." if what[0] == "unrequeued" else "Place it (tier 30 and up starts past the cap) or say why it waits."))
    for page in pages:
        if seed:
            continue
        if dry:
            print("dry: " + page)
            continue
        for recipient in recipients:
            subprocess.run([ahra, "os", "send", recipient, page], cwd="/Users/kirkouimet/Projects/ahra", capture_output=True, text=True)
        print(time.strftime("%H:%M:%S", time.gmtime()) + " " + page)
    if not dry:
        json.dump(paged, open(state + ".partial", "w"))
        os.replace(state + ".partial", state)
    return 0


if __name__ == "__main__":
    sys.exit(main())
