#!/usr/bin/env python3
"""keptunits.py <job.json> <kept.json>: a re-plan places no unit whose every test an earlier attempt already proved
(#g4jyja4; Kirk, Oct 9 22:28Z: "take whatever passes, and if something fails for Loom's reasons, fix it and run just
that part").

verify.sh skips a kept test in every spec of its package, but a unit whose specs name only kept tests still ran: placed,
its tree readied and its tests compiled, to run nothing (c2 28107975's requeue held 11 such product units of 316). Here
a spec that names its tests exactly, ^(A|B)$, and names only tests kept for its package leaves the unit; a unit left
with no spec leaves the job, and the needs on it go with it. A spec that names its tests any other way (a remainder,
a family with its shards, a pattern) stays: it may run a test no one kept. The job is rewritten in place, and one line
says what left.
"""

import json
import re
import sys

exactNames = re.compile(r"\^\(([A-Za-z0-9_]+(?:\|[A-Za-z0-9_]+)*)\)\$")


def fullyKept(spec, kept):
    """Whether a <package>=<pattern>[ skip=...] spec runs only tests kept for its package."""
    package, _, pattern = spec.partition("=")
    match = exactNames.fullmatch(pattern.split(" skip=", 1)[0])
    return bool(match) and all(name in kept.get(package, ()) for name in match.group(1).split("|"))


def prune(job, kept):
    """Drops the fully kept specs and the units they leave empty; returns the ids of the units that left."""
    kept = {package: set(tests) for package, tests in kept.items()}
    left = []
    for unit in job["units"]:
        argv = unit.get("argv") or []
        if len(argv) <= 5 or argv[3] != "adamic-gate-unit":
            continue
        specs = [spec for spec in argv[5:] if not fullyKept(spec, kept)]
        if specs:
            unit["argv"] = argv[:5] + specs
        else:
            left.append(unit["id"])
    job["units"] = [unit for unit in job["units"] if unit["id"] not in left]
    for unit in job["units"]:
        if unit.get("needs"):
            unit["needs"] = [need for need in unit["needs"] if need not in left]
            if not unit["needs"]:
                del unit["needs"]
    return left


def main():
    job = json.load(open(sys.argv[1]))
    kept = json.load(open(sys.argv[2]))
    before = len(job["units"])
    left = prune(job, kept)
    json.dump(job, open(sys.argv[1], "w"), indent=2)
    print("%d of %d units left the plan, every test they name kept%s" % (len(left), before, (": " + ", ".join(left)) if left else ""))


if __name__ == "__main__":
    sys.exit(main())
