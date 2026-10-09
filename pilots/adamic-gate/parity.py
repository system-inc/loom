#!/usr/bin/env python3
"""parity.py <strict test.jsonl> <race test.jsonl>: the two runs' go test outcomes, test for test (#098rcha, step d).

verify.sh's strict race runs one job twice, its go test units as test jobs on codex-strict and as before on another
pool, and asks whether they agree. Each test's outcome is its last word (pass, fail or skip) in its package; a test one
run holds and the other doesn't is a difference too. The first line is the summary; each difference follows, sorted.
Exit 0 when every test agrees, 1 when any differs, 2 when a file can't be read.
"""

import json
import sys


def outcomes(path):
    result = {}
    for line in open(path, errors="replace"):
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if event.get("Test") and event.get("Action") in ("pass", "fail", "skip"):
            result[(event.get("Package") or "", event["Test"])] = event["Action"]
    return result


def main():
    try:
        strict, race = outcomes(sys.argv[1]), outcomes(sys.argv[2])
    except OSError as error:
        print("parity: unreadable: %s" % error)
        return 2
    differences = []
    for key in sorted(set(strict) | set(race)):
        if strict.get(key) != race.get(key):
            differences.append("%s %s: strict %s, race %s" % (key[0], key[1], strict.get(key, "absent"), race.get(key, "absent")))
    same = len(set(strict) | set(race)) - len(differences)
    print("parity: %s, %d tests the same, %d differ (strict %d, race %d)" % ("identical" if not differences else "differs", same, len(differences), len(strict), len(race)))
    for line in differences:
        print(line)
    return 1 if differences else 0


if __name__ == "__main__":
    sys.exit(main())
