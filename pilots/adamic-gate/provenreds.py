#!/usr/bin/env python3
"""provenreds.py <reds.txt> <test.jsonl>: whether a ceiling-stopped run holds a red that proves something about the change.

fast.sh keeps a red in hand when a job stops at its ceiling (a648c89), but a kill isn't a proven red: a unit killed over
its budget ("FAIL <unit> (killed ...)"), or a test whose own output says "signal: killed" (its build or binary killed on
an instance short of memory or disk). Oct 9 22:42Z: chain-followers 66988370 was posted red on product-11 killed at
600 s and two bridge/tsgo products whose builds were killed, while the run's own line read void with 219 units broken
on full disks.

Exit 0 and print the first proven red when one exists; exit 1 when every red is a kill (or there are none).
"""
import json
import os
import re
import sys

reds, tests = sys.argv[1:3]
killed = set()
for line in open(tests, errors="replace") if os.path.exists(tests) else []:
    try:
        event = json.loads(line)
    except ValueError:
        continue
    if event.get("Action") == "output" and "signal: killed" in (event.get("Output") or ""):
        killed.add((event.get("Package") or "", (event.get("Test") or "").split("/")[0]))
for line in open(reds, errors="replace") if os.path.exists(reds) else []:
    if not line.startswith("FAIL ") or re.match(r"FAIL \S+ \(killed", line):
        continue
    fields = line.split()
    if len(fields) >= 3 and (fields[1], fields[2]) in killed:
        continue
    print(line.rstrip("\n"))
    sys.exit(0)
sys.exit(1)
