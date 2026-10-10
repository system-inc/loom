#!/usr/bin/env python3
"""coldcheck.py <pools.json> <verdicts.jsonl>: no parity verdict may rest on a warm run (Release, Oct 10 02:48Z). A
decided unit is warm when its last attempt ran on a warm runner (runner 8a70ebce, commit 50cd31d: one Go cache per
Codex instance, outliving its units) on a machine no cold pool lists, or before that machine's coldSince. Every later
runner gives each unit its own empty caches, so its runs are cold by construction. A machine's coldSince is the latest
over the cold pools that list it, the conservative reading. Prints each warm unit; exit 0 when none, 1 when any, 2 on
an unreadable file.

	pilots/adamic-gate/coldcheck.py pools.json verdicts.jsonl && pilots/adamic-gate/boxparity.py <box record> verdicts.jsonl
"""

import json
import sys

# The runners whose units share a warm cache, as their started events name them.
warmRunners = {"git-50cd31d277a6"}


def coldSince(pools):
    """Each machine's coldSince, the latest over the cold pools that list it."""
    since = {}
    for pool in pools:
        if pool.get("cold") and pool.get("coldSince"):
            for machine in pool.get("machines") or []:
                since[machine] = max(since.get(machine, ""), pool["coldSince"])
    return since


def warmUnits(pools, verdicts):
    since = coldSince(pools)
    warm = []
    for verdict in verdicts:
        if verdict.get("status") not in ("passed", "failed"):
            continue
        attempt = (verdict.get("attempts") or [{}])[-1]
        if attempt.get("runner") not in warmRunners:
            continue
        machine, started = attempt.get("machine"), attempt.get("startedAt") or ""
        if machine not in since or started[:19] < since[machine][:19]:
            warm.append("%s %s on %s at %s" % (verdict.get("unitKey", "")[:12], verdict.get("status"), machine, started))
    return warm


def main():
    if len(sys.argv) != 3:
        print(__doc__.strip().splitlines()[0])
        return 2
    try:
        pools = json.load(open(sys.argv[1]))["pools"]
        verdicts = [json.loads(line) for line in open(sys.argv[2]) if line.strip()]
    except (OSError, ValueError, KeyError) as error:
        print("coldcheck: unreadable: %s" % error)
        return 2
    warm = warmUnits(pools, verdicts)
    for row in warm:
        print("warm: " + row)
    return 1 if warm else 0


if __name__ == "__main__":
    sys.exit(main())
