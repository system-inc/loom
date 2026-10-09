#!/usr/bin/env python3
"""clock.py: submit to verdict, the clock a change lives through (@system_adamic, Oct 9 18:20Z, from the honesty
angel: "176 s" was a run's wall once it started, and the queue, the voids and the dead jobs it left out burned hours).

	pilots/adamic-gate/clock.py [--hours 24] [--now <epoch>]     # LOOM_CLOCK_JOBS and LOOM_CLOCK_GATE for tests

Every fast job the gate saw in the window counts. Its clock starts at submit, the earliest of its job file's and its
work directory's creation (a re-tier rewrites the job file, never the work directory). It stops at the verdict that
stands: a green or red <sha>.verdict, at that file's time; or, for a sha main already holds with no standing verdict,
the commit time of the first main commit that holds it. A void, a cancel, a requeue or no verdict at all leaves the
clock running to now: a change with no verdict counts as still waiting, never as missing. A job its owner withdrew
(withdraw.sh's <sha>.withdrawn: superseded, folded into a train, no longer wanted) is answered at that moment, and is
counted apart, so a withdrawal never passes for a verdict.

It prints the headline: submit to verdict p50 and p90 over every job in the window (running ones at their age now), how
many have no verdict, and the oldest of those. A second line splits the ones with no verdict: a job whose sha is no
longer its branch's tip was superseded by a newer cut, and its change waits on in that cut, so it is named apart there,
never dropped from the headline.
"""

import argparse
import os
import re
import subprocess
import sys
import time

jobs = os.environ.get("LOOM_CLOCK_JOBS") or os.path.expanduser("~/.loom/jobs/fast")
gate = os.environ.get("LOOM_CLOCK_GATE") or os.path.expanduser("~/Projects/system/adamic-gate")


def born(path):
    stat = os.stat(path)
    return getattr(stat, "st_birthtime", stat.st_mtime)


def landedAt(sha):
    """When main first held sha, or None: the commit time of the first commit on main's first-parent line that has it."""
    result = subprocess.run(["git", "-C", gate, "rev-list", "--first-parent", "--reverse", "--format=%ct",
                             "--ancestry-path", sha + "..origin/main"], capture_output=True, text=True)
    if result.returncode != 0:
        return None
    times = [int(line) for line in result.stdout.split() if line.isdigit()]
    return times[0] if times else None


def percentile(values, fraction):
    ordered = sorted(values)
    return ordered[min(len(ordered) - 1, int(fraction * len(ordered)))]


def minutes(seconds):
    return "%d min" % round(seconds / 60) if seconds >= 60 else "%d s" % seconds


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--hours", type=float, default=24)
    parser.add_argument("--now", type=float, default=None)
    arguments = parser.parse_args()
    now = arguments.now or time.time()
    heads = {}
    listing = subprocess.run(["git", "-C", gate, "ls-remote", "origin", "refs/heads/*"], capture_output=True, text=True).stdout
    for line in listing.splitlines():
        tip, _, ref = line.partition("\t")
        heads[ref[len("refs/heads/"):]] = tip
    clocks, open_, superseded, withdrawn = [], [], 0, 0
    for name in sorted(os.listdir(jobs)):
        if not re.fullmatch(r"[0-9a-f]{40}\.json", name):
            continue
        sha = name[:-5]
        starts = [born(os.path.join(jobs, name))]
        if os.path.isdir(os.path.join(jobs, sha + ".work")):
            starts.append(born(os.path.join(jobs, sha + ".work")))
        submit = min(starts)
        if now - submit > arguments.hours * 3600:
            continue
        stopped = None
        verdict = os.path.join(jobs, sha + ".verdict")
        if os.path.exists(os.path.join(jobs, sha + ".withdrawn")):
            withdrawn += 1
            clocks.append(max(0, os.path.getmtime(os.path.join(jobs, sha + ".withdrawn")) - submit))
            continue
        if os.path.exists(verdict) and open(verdict, errors="replace").read(6).startswith(("green:", "red:")):
            stopped = os.path.getmtime(verdict)
        else:
            stopped = landedAt(sha)
        if stopped is None:
            branch = re.search(r'"branch": "([^"]*)"', open(os.path.join(jobs, name), errors="replace").read())
            if branch and heads and heads.get(branch.group(1)) != sha:
                superseded += 1
            open_.append((now - submit, sha))
            clocks.append(now - submit)
        else:
            clocks.append(max(0, stopped - submit))
    if not clocks:
        print("submit to verdict: no fast jobs in the last %g hours" % arguments.hours)
        return 0
    oldest = max(open_) if open_ else None
    print("submit to verdict over the last %g hours: p50 %s, p90 %s, %d jobs, %d with no verdict%s%s" % (
        arguments.hours, minutes(percentile(clocks, 0.5)), minutes(percentile(clocks, 0.9)), len(clocks), len(open_),
        " (oldest %s, %s)" % (minutes(oldest[0]), oldest[1][:12]) if oldest else "",
        ", %d withdrawn by their owners" % withdrawn if withdrawn else ""))
    if open_:
        print("of the %d with no verdict: %d are no longer their branch's tip (superseded by a newer cut, the change waits on in it),"
              " %d are current tips still waiting" % (len(open_), superseded, len(open_) - superseded))
    return 0


if __name__ == "__main__":
    sys.exit(main())
