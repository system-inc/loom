#!/usr/bin/env python3
"""placed.py: a fast job's units-placed signal for developer tools' watcher (#3tj643t, #7bjfzte): while the job runs,
~/.loom/jobs/fast/<tip sha>.placed holds one line, "<placed> <total>": units an instance has taken (a started event on
the wire), and units the job planned, its selection counted as one. Rewritten (write, then rename) whenever the count
changes; absent until something is placed. Until the tests are planned the total is one more than what's placed, so the
watcher never reads a job as fully placed before its plan exists.

	pilots/adamic-gate/placed.py <jobs directory> <tip sha>      # fast.sh starts it beside each job; a copy in ~/.loom/bin
"""

import base64
import hashlib
import hmac
import json
import os
import subprocess
import sys
import time

wire = "https://loom-wire.kirk-ouimet.workers.dev"


def token(run):
    secret = open(os.path.expanduser("~/.loom/token-secret")).read().strip().encode()
    payload = base64.urlsafe_b64encode(json.dumps({"run": run, "scope": "coordinator", "expires": int(time.time()) + 600}, separators=(",", ":")).encode()).rstrip(b"=")
    return (payload + b"." + base64.urlsafe_b64encode(hmac.new(secret, payload, hashlib.sha256).digest()).rstrip(b"=")).decode()


def runOf(path):
    """The run id a coordinator's log names on its first line ("run <id>: ...")."""
    try:
        first = open(path).readline().split()
    except OSError:
        return None
    return first[1].rstrip(":") if len(first) > 1 and first[0] == "run" else None


def main():
    jobs, sha = sys.argv[1], sys.argv[2]
    work = os.path.join(jobs, sha + ".work")
    running = os.path.join(jobs, sha + ".running")
    selecting = os.path.exists(os.path.join(work, "select-mode")) and open(os.path.join(work, "select-mode")).read().strip() == "yes"
    cursors, started, written = {}, {}, None
    while os.path.exists(running):
        for log in ("select-run.log", "run.log"):
            run = runOf(os.path.join(work, log))
            if run is None:
                continue
            # A run log rewritten for a later attempt names a new run: its count starts over.
            started.setdefault(run, set())
            try:
                lines = subprocess.run(["curl", "-fsS", "--max-time", "4", "%s/runs/%s/events?after=%d" % (wire, run, cursors.get(run, 0)),
                                        "-H", "Authorization: Bearer " + token(run)], capture_output=True, text=True).stdout.splitlines()
            except OSError:
                lines = []
            for line in lines:
                try:
                    record = json.loads(line)
                except ValueError:
                    continue
                cursors[run] = max(cursors.get(run, 0), record.get("position", 0))
                if record.get("event", {}).get("type") == "started":
                    started[run].add(record["event"].get("unit"))
        selectRun, testRun = runOf(os.path.join(work, "select-run.log")), runOf(os.path.join(work, "run.log"))
        try:
            names = {unit["id"] for unit in json.load(open(os.path.join(work, "job.json")))["units"]} if testRun else None
        except (OSError, ValueError, KeyError):
            names = None
        # Only units this attempt's plan names count, the selection as one: a long-lived count once read 309 of 24 on
        # the trio (Oct 9 10:11Z), so placed is held to what the plan can hold.
        # A selection made on a gate box (select-box.sh, #7cmv2g3) has no run: its log's "box <name>:" line is it placed.
        try:
            onBox = open(os.path.join(work, "select-run.log")).readline().startswith("box ")
        except OSError:
            onBox = False
        placed = (1 if (selectRun and started.get(selectRun)) or onBox else 0) + (len(started.get(testRun, set()) & names) if names else 0)
        total = (1 if selecting else 0) + len(names) if names is not None else placed + 1
        placed = min(placed, total)
        line = "%d %d\n" % (placed, total)
        if placed and line != written:
            partial = os.path.join(jobs, sha + ".placed.partial")
            open(partial, "w").write(line)
            os.replace(partial, os.path.join(jobs, sha + ".placed"))
            written = line
        time.sleep(5)


if __name__ == "__main__":
    main()
