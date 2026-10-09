#!/usr/bin/env python3
"""solve.py: from a budgeted plan, the wall on a pool of a given size and the instances a target wall needs (#2en3b4t,
step 81's (h)).

	pilots/adamic-gate/solve.py <plan's stderr> [--instances 100] [--target 180] [--setup 10]   # a copy in ~/.loom/bin

Each unit's seconds are the plan's prediction (its "by the reference" plus setup). The pool hands out longest first
and an idle instance takes the next unit, so the wall on n instances is the list schedule's makespan, longest first.
It never falls below the longest unit alone: when that unit is over the target, no number of instances reaches it, and
the solver names the units that set the floor instead of asking for instances that wouldn't help.
"""

import argparse
import heapq
import re
import sys


def makespan(seconds, instances):
    finishing = [0.0] * instances
    for one in sorted(seconds, reverse=True):
        heapq.heappush(finishing, heapq.heappop(finishing) + one)
    return max(finishing)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("plan")
    parser.add_argument("--instances", type=int, default=100)
    parser.add_argument("--target", type=float, default=180.0)
    parser.add_argument("--setup", type=float, default=10.0)
    arguments = parser.parse_args()
    units = {}
    for line in open(arguments.plan):
        match = re.match(r"^(\S+): .*, (\d+(?:\.\d+)?) s by the reference$", line.strip())
        if match:
            units[match.group(1)] = float(match.group(2)) + arguments.setup
    if not units:
        sys.exit("solve: no unit lines in %s" % arguments.plan)
    seconds = list(units.values())
    total, longest = sum(seconds), max(seconds)
    wall = makespan(seconds, arguments.instances)
    print("%d units, %.0f s of work, longest %.0f s; on %d instances the wall is %.0f s (%.1f rounds of the mean unit)"
          % (len(units), total, longest, arguments.instances, wall, wall / (total / len(units))))
    if longest > arguments.target:
        floor = sorted(((one, unit) for unit, one in units.items() if one > arguments.target), reverse=True)
        print("the %.0f s target is under the floor: %d units alone take longer, so no instance count reaches it; "
              "they are the burn-down: %s" % (arguments.target, len(floor), ", ".join("%s %.0f s" % (unit, one) for one, unit in floor[:8])))
        print("at the floor, %d instances already reach %.0f s" % (needed(seconds, longest * 1.0001), makespan(seconds, needed(seconds, longest * 1.0001))))
        return 0
    count = needed(seconds, arguments.target)
    print("%d instances reach the %.0f s target (wall %.0f s)" % (count, arguments.target, makespan(seconds, count)))
    return 0


# needed is the fewest instances whose makespan is within the target, by bisection: the makespan only falls as
# instances are added, and len(seconds) instances give the longest unit alone.
def needed(seconds, target):
    low, high = 1, len(seconds)
    while low < high:
        middle = (low + high) // 2
        if makespan(seconds, middle) <= target:
            high = middle
        else:
            low = middle + 1
    return low


if __name__ == "__main__":
    sys.exit(main())
