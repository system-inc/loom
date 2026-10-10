#!/usr/bin/env python3
"""coldcheck_test.py [<coldcheck.py>]: a warm-runner unit on an unlisted machine, or before its machine's coldSince, is
warm; on a listed machine after it, cold; any other runner is cold by construction; a void is never checked. The mutant
that ignores coldSince must fail:

	python3 pilots/adamic-gate/coldcheck_test.py
	sed 's/ or started\\[:19\\] < since\\[machine\\]\\[:19\\]//' coldcheck.py > m.py && python3 coldcheck_test.py m.py   # fails
"""

import json
import os
import subprocess
import sys
import tempfile

here = os.path.dirname(os.path.abspath(__file__))
coldcheck = sys.argv[1] if len(sys.argv) > 1 else os.path.join(here, "coldcheck.py")
failures = 0
root = tempfile.mkdtemp()
pools = os.path.join(root, "pools.json")
json.dump({"pools": [{"name": "box-strict-8a70-cold", "cold": True, "machines": ["Cloud"], "coldSince": "2026-10-10T02:44:07Z"},
                     {"name": "codex-strict", "cold": False}]}, open(pools, "w"))


def run(units):
    path = os.path.join(root, "v.jsonl")
    with open(path, "w") as stream:
        for key, status, runner, machine, started in units:
            stream.write(json.dumps({"unitKey": key, "status": status, "attempts": [{"runner": runner, "machine": machine, "startedAt": started}]}) + "\n")
    result = subprocess.run([sys.executable, coldcheck, pools, path], capture_output=True, text=True)
    return result.returncode, result.stdout


def check(name, condition, output):
    global failures
    print(("PASS " if condition else "FAIL ") + name + ("" if condition else ": " + output))
    failures += 0 if condition else 1


warm = "git-50cd31d277a6"
code, output = run([("k1", "passed", warm, "Cloud", "2026-10-10T02:50:00Z")])
check("a warm-runner pass on a cold machine after its coldSince is cold, exit 0", code == 0, output)
code, output = run([("k1", "passed", warm, "Cloud", "2026-10-10T02:20:00Z")])
check("the same machine before its coldSince is warm, exit 1", code == 1 and "k1" in output, output)
code, output = run([("k1", "passed", warm, "9d4c7dee9915", "2026-10-10T02:50:00Z")])
check("a warm-runner pass on a Codex machine no cold pool lists is warm, exit 1", code == 1, output)
code, output = run([("k1", "passed", "git-9e8f05f00000", "9d4c7dee9915", "2026-10-10T02:50:00Z")])
check("a later runner is cold by construction, exit 0", code == 0, output)
code, output = run([("k1", "void", warm, "9d4c7dee9915", "2026-10-10T02:50:00Z")])
check("a void is never checked, exit 0", code == 0, output)
print("%d failed" % failures)
sys.exit(1 if failures else 0)
