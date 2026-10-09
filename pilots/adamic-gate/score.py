#!/usr/bin/env python3
"""score.py: each unit's predicted seconds beside its actual ones, scored (#2en3b4t, step 81's (e)).

	pilots/adamic-gate/score.py <plan's stderr> <job.json> <record.jsonl> [--setup 10] > score.txt   # a copy in ~/.loom/bin

The prediction is what the plan packed by: its "tests-N: ..., S s by the reference" line plus the unit's setup (the
plan's --unit-setup). The actual is the unit's last attempt, from its started event to its finished one on the
instance, so a unit's time in the pool's queue never counts. Printed: one row per unit (predicted, actual, the error
in seconds and as a ratio, the verdict, killed at its budget or not), then the run's mean absolute error, how many
packed units ran over their budget, and the worst under-predictions, which are what the next plan has to learn.
"""

import argparse
import datetime
import json
import re
import sys


def instant(value):
    return datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("plan")
    parser.add_argument("job")
    parser.add_argument("record")
    parser.add_argument("--setup", type=float, default=10.0)
    parser.add_argument("--budget", type=float, default=60.0)
    arguments = parser.parse_args()
    predicted = {}
    for line in open(arguments.plan):
        match = re.match(r"^(\S+): .*, (\d+(?:\.\d+)?) s by the reference$", line.strip())
        if match:
            predicted[match.group(1)] = float(match.group(2)) + arguments.setup
    timeouts = {unit["id"]: unit.get("timeoutSeconds") for unit in json.load(open(arguments.job))["units"]}
    started, finished = {}, {}
    for line in open(arguments.record):
        event = json.loads(line)
        if event.get("type") == "started":
            started[event["unit"]] = instant(event["time"])
        elif event.get("type") == "finished" and event["unit"] in started:
            finished[event["unit"]] = (instant(event["time"]) - started[event["unit"]]).total_seconds(), event.get("status"), bool(event.get("timedOut"))
    rows = []
    for unit, expected in predicted.items():
        if unit not in finished:
            rows.append((unit, expected, None, "missing", False, timeouts.get(unit)))
            continue
        actual, status, killed = finished[unit]
        rows.append((unit, expected, actual, status, killed, timeouts.get(unit)))
    print("unit\tpredicted\tactual\terror\tratio\tverdict\tkilled\ttimeout")
    for unit, expected, actual, status, killed, timeout in sorted(rows, key=lambda row: -((row[2] or 0) - row[1])):
        if actual is None:
            print("%s\t%.0f\t-\t-\t-\t%s\t-\t%s" % (unit, expected, status, timeout))
        else:
            print("%s\t%.0f\t%.1f\t%+.1f\t%.2f\t%s\t%s\t%s" % (unit, expected, actual, actual - expected, actual / expected if expected else 0, status, "killed" if killed else "", timeout))
    scored = [row for row in rows if row[2] is not None]
    packed = [row for row in scored if row[5] is not None and row[5] <= arguments.budget * 1.5 + 1]
    over = [row for row in packed if row[2] > arguments.budget]
    killed = [row for row in scored if row[4]]
    error = sum(abs(row[2] - row[1]) for row in scored) / len(scored) if scored else 0
    print("\n# %d units scored of %d planned; mean absolute error %.1f s; %d packed units, %d of them over %.0f s, %d killed at their budget"
          % (len(scored), len(rows), error, len(packed), len(over), arguments.budget, len(killed)), file=sys.stdout)
    if packed:
        ratios = sorted(row[2] / row[1] for row in packed if row[1])
        print("# packed units, actual over predicted: median %.2f, p90 %.2f, worst %.2f" % (ratios[len(ratios) // 2], ratios[int(len(ratios) * 0.9)], ratios[-1]))


if __name__ == "__main__":
    sys.exit(main())
