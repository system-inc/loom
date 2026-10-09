#!/usr/bin/env python3
"""completion.py: a pool fast record states what its tests stage did, so push-main can land it (integration, Oct 9
14:55Z: "stages with no completion recorded: tests"; every pool Go-set record so far carried no steps, exits or counts,
and push-main lands only what a record says ran).

	completion.py <record directory> <verdict word> <build verdict>

The record directory holds fast.json, test.jsonl (or .gz) and record.jsonl (or .gz). It adds to fast.json:
planned_stages ["tests"]; pass, fail and skip, each top-level test counted once by its last verdict; wall_seconds and
steps_seconds {"tests": ...}, from the run's first event to its last; stages_exit {"tests": 0} for a green verdict and
1 for a red, never for a void, which completed nothing; build_ok and vet_ok only when the build-vet unit passed.
"""
import datetime
import gzip
import json
import os
import sys


def lines(path):
    if os.path.exists(path):
        return open(path, errors="replace")
    if os.path.exists(path + ".gz"):
        return gzip.open(path + ".gz", "rt", errors="replace")
    return []


def counts(directory):
    last = {}
    for line in lines(os.path.join(directory, "test.jsonl")):
        try:
            event = json.loads(line)
        except ValueError:
            continue
        test = event.get("Test") or ""
        if test and "/" not in test and event.get("Action") in ("pass", "fail", "skip"):
            last[(event.get("Package", ""), test)] = event["Action"]
    return {action: sum(1 for verdict in last.values() if verdict == action) for action in ("pass", "fail", "skip")}


def wall(directory):
    times = []
    for line in lines(os.path.join(directory, "record.jsonl")):
        try:
            event = json.loads(line)
        except ValueError:
            continue
        event = event.get("event", event)
        if event.get("time"):
            times.append(datetime.datetime.fromisoformat(event["time"].replace("Z", "+00:00")))
    return round((max(times) - min(times)).total_seconds(), 1) if times else None


def main():
    if len(sys.argv) != 4:
        print(__doc__.strip().split("\n\n")[1])
        return 2
    directory, verdict, build = sys.argv[1:]
    path = os.path.join(directory, "fast.json")
    record = json.load(open(path))
    record.update(counts(directory))
    record["planned_stages"] = ["tests"]
    seconds = wall(directory)
    if seconds is not None:
        record["wall_seconds"] = seconds
        record["steps_seconds"] = {"tests": seconds}
    if verdict in ("green", "red"):
        record["stages_exit"] = {"tests": 0 if verdict == "green" else 1}
    if build == "passed":
        record.update({"build_ok": True, "vet_ok": True})
    json.dump(record, open(path, "w"), indent=2)
    return 0


if __name__ == "__main__":
    sys.exit(main())
