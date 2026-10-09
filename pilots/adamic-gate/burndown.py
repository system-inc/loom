#!/usr/bin/env python3
"""burndown.py: step 79's burn-down (#5g5151k), every leaf over 60 s on Loom's own 4-CPU Codex times, by owner.

	pilots/adamic-gate/burndown.py <merged go test -json lines> [--over 60] [--run <name>] > burndown.md
	pilots/adamic-gate/burndown.py <merged go test -json lines> --line --run <sha12>   # one status line, 160 at most

A leaf is a test with no subtest of its own in the record. Its seconds run from its last run or cont event to its
pass or fail (a t.Parallel test's pause isn't its work), never a parent's Elapsed. Ownership is by package, refined by
what owners claimed for themselves on Oct 9 (runtime's C runtime tests in internal/native, library's oracle families,
typescript's note that stage1/typescript is cohere's). A row nobody claims says so.
"""

import argparse
import collections
import datetime
import json
import re
import sys

module = "github.com/system-inc/adamic/"

# (package prefix, test pattern or None, owner), first match wins.
owners = [
    ("internal/native", r"Decode|String|Normaliz|TypedArray|Parallel|TSan|Signal|Release|GraphRegion|Region|RecordMutant|CaseMapping", "runtime"),
    ("internal/oracle", r"Regexp|RegExp|Date|JSON|Json|Map|Set|Fs|FS|Process|Library", "library"),
    ("internal/unicodeproperties", None, "library"),
    ("internal/regexp", None, "library"),
    ("library", None, "library"),
    ("cmd/adamic-test262", None, "library"),
    ("stage1/", None, "cohere"),
    ("stage3", None, "typescript"),
    ("internal/oracle", None, "developer tools"),
    ("cloud", None, "developer tools"),
    ("internal/", None, "compiler"),
    ("cmd/adamic", None, "compiler"),
    ("bridge/", None, "compiler"),
]


def ownerOf(package, test):
    relative = package[len(module):] if package.startswith(module) else package
    for prefix, pattern, owner in owners:
        if relative.startswith(prefix) and (pattern is None or re.search(pattern, test)):
            return owner
    return "unclaimed (" + relative.split("/")[0] + ")"


def instant(value):
    # go test -json times carry nanoseconds and a zone; seconds are enough here.
    return datetime.datetime.fromisoformat(value[:19])


# statusLine is the burn-down as #5g5151k's status: the count and seconds over the budget, then each owner's, most
# seconds first, cut to fit 160 characters with the run last (the witness's grain curve reads the leading count). A run
# that didn't bring every planned unit's results counts only what came back, so its line says how many units that was
# ("31 over 60 s in 412 of 626 units"): only a complete run's count compares with another's (@system_adamic, Oct 9).
def statusLine(byOwner, rows, arguments):
    head = "%d leaves over %.0f s, %.0f s:" % (len(rows), arguments.over, sum(row[1] for row in rows))
    if arguments.units_planned and arguments.units_run < arguments.units_planned:
        head = "%d over %.0f s in %d of %d units, %.0f s:" % (len(rows), arguments.over, arguments.units_run, arguments.units_planned, sum(row[1] for row in rows))
    tail = " (%s)" % arguments.run if arguments.run else ""
    owners = sorted(byOwner, key=lambda name: -sum(row[1] for row in byOwner[name]))
    parts = ["%s %d/%.0f s" % (owner, len(byOwner[owner]), sum(row[1] for row in byOwner[owner])) for owner in owners]
    line = head
    for index, part in enumerate(parts):
        candidate = line + (" " if index == 0 else ", ") + part
        rest = len(parts) - index - 1
        if len(candidate + (", +%d more" % rest if rest else "") + tail) > 160:
            line += ", +%d more" % (len(parts) - index)
            break
        line = candidate
    if not parts:
        line = head[:-1].replace(", 0 s", "")
    return line + tail


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("record")
    parser.add_argument("--over", type=float, default=60.0)
    parser.add_argument("--run", default="")
    parser.add_argument("--line", action="store_true", help="one line for #5g5151k's status feed, at most 160 characters")
    parser.add_argument("--units-run", type=int, default=0, help="the units whose results came back")
    parser.add_argument("--units-planned", type=int, default=0, help="the units the run planned: fewer back marks the count partial")
    arguments = parser.parse_args()
    started, ended, outcome, names = {}, {}, {}, collections.defaultdict(set)
    for line in open(arguments.record, errors="replace"):
        try:
            event = json.loads(line)
        except ValueError:
            continue
        test, package = event.get("Test"), event.get("Package")
        if not test or not package or "Time" not in event:
            continue
        key = (package, test)
        names[package].add(test)
        if event.get("Action") in ("run", "cont"):
            started[key] = instant(event["Time"])
        elif event.get("Action") in ("pass", "fail"):
            ended[key] = instant(event["Time"])
            outcome[key] = event["Action"]
    rows = []
    for key, end in ended.items():
        package, test = key
        if any(other.startswith(test + "/") for other in names[package]):
            continue  # not a leaf: its children are
        if key not in started:
            continue
        seconds = (end - started[key]).total_seconds()
        if seconds > arguments.over:
            rows.append((ownerOf(package, test.split("/")[0]), seconds, package[len(module):], test, outcome[key]))
    byOwner = collections.defaultdict(list)
    for row in rows:
        byOwner[row[0]].append(row)
    if arguments.line:
        print(statusLine(byOwner, rows, arguments))
        return 0
    print("# Step 79 burn-down%s: leaves over %.0f s on a 4-CPU Codex instance\n" % (" (" + arguments.run + ")" if arguments.run else "", arguments.over))
    print("%d leaves, %.0f s, by Loom's own leaf times (last run or cont to its end).\n" % (len(rows), sum(row[1] for row in rows)))
    print("| owner | leaves over %.0f s | seconds | longest |" % arguments.over)
    print("|---|---|---|---|")
    for owner in sorted(byOwner, key=lambda name: -max(row[1] for row in byOwner[name])):
        longest = max(byOwner[owner], key=lambda row: row[1])
        print("| %s | %d | %.0f | %s %s %.0f s |" % (owner, len(byOwner[owner]), sum(row[1] for row in byOwner[owner]), longest[2], longest[3], longest[1]))
    for owner in sorted(byOwner):
        print("\n## %s\n" % owner)
        for _, seconds, package, test, action in sorted(byOwner[owner], key=lambda row: -row[1]):
            print("- %s %s: %.0f s%s" % (package, test, seconds, "" if action == "pass" else " (failed)"))


if __name__ == "__main__":
    sys.exit(main())
