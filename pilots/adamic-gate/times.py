#!/usr/bin/env python3
"""times.py: Loom's living times table (#2en3b4t), every leaf's seconds on a 4-CPU Codex instance, kept run by run.

	pilots/adamic-gate/times.py update <merged go test -json lines> --sha <sha> --run <run> [--table ~/.loom/times.json]
	pilots/adamic-gate/times.py tsv [--table ...] > ~/.loom/loom-times.tsv      # what plan --loom-times reads

Each test, leaf or parent, keeps its last five observations: seconds from its last run or cont
event to its end, the run, the sha, and its package's git tree hash at that sha (for the headroom a changed package
will get). tsv predicts each as the p90 of its five; a parent is timed by its own wall, since its parallel children
overlap.
"""

import argparse
import datetime
import json
import os
import subprocess
import sys

module = "github.com/system-inc/adamic/"
gate = os.path.expanduser("~/Projects/system/adamic-gate")
keep = 5


def instant(value):
    return datetime.datetime.fromisoformat(value[:19])


def treeHash(sha, package, cache):
    directory = package[len(module):] if package.startswith(module) else ""
    if (sha, directory) not in cache:
        listing = subprocess.run(["git", "-C", gate, "rev-parse", "%s:%s" % (sha, directory)], capture_output=True, text=True)
        cache[(sha, directory)] = listing.stdout.strip() if listing.returncode == 0 else ""
    return cache[(sha, directory)]


def leaves(path):
    started, ended, names = {}, {}, {}
    for line in open(path, errors="replace"):
        try:
            event = json.loads(line)
        except ValueError:
            continue
        test, package = event.get("Test"), event.get("Package")
        if not test or not package or "Time" not in event:
            continue
        names.setdefault(package, set()).add(test)
        if event.get("Action") in ("run", "cont"):
            started[(package, test)] = instant(event["Time"])
        elif event.get("Action") in ("pass", "fail") and (package, test) in started:
            ended[(package, test)] = (started[(package, test)], instant(event["Time"]))
    # Parents too, by their own wall: a parent's parallel children overlap, so their sum would overstate it.
    for (package, test), (start, end) in ended.items():
        yield package, test, (end - start).total_seconds()


def load(path):
    try:
        return json.load(open(path))
    except (OSError, ValueError):
        return {}


def p90(values):
    ordered = sorted(values)
    return ordered[min(len(ordered) - 1, int(0.9 * len(ordered)))]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("command", choices=["update", "tsv"])
    parser.add_argument("record", nargs="?")
    parser.add_argument("--sha", default="")
    parser.add_argument("--run", default="")
    parser.add_argument("--table", default=os.path.expanduser("~/.loom/times.json"))
    arguments = parser.parse_args()
    table = load(arguments.table)
    if arguments.command == "update":
        if not arguments.record or len(arguments.sha) != 40:
            sys.exit("update needs the merged record and --sha")
        hashes, count = {}, 0
        for package, test, seconds in leaves(arguments.record):
            row = table.setdefault(package + " " + test, [])
            row.append({"seconds": round(seconds, 2), "run": arguments.run, "sha": arguments.sha, "tree": treeHash(arguments.sha, package, hashes)})
            del row[:-keep]
            count += 1
        staging = arguments.table + ".partial"
        json.dump(table, open(staging, "w"), indent=0, sort_keys=True)
        os.replace(staging, arguments.table)
        print("times: %d leaves from %s, %d in the table" % (count, arguments.run or arguments.sha[:12], len(table)))
        return
    # tsv: each test's prediction, leaves and parents alike, in compare --times' shape.
    print("loom_seconds\twhole_gate_seconds\tunit\tpackage\ttest")
    for key, rows in sorted(table.items()):
        package, test = key.split(" ", 1)
        print("%.2f\t0\ttimes\t%s\t%s" % (p90([row["seconds"] for row in rows]), package, test))


if __name__ == "__main__":
    main()
