#!/usr/bin/env python3
"""boxparity_test.py [<boxparity.py>]: boxparity.py on planted records. Identical sides agree; one test missing or
flipped on either side is named with its unit; a stopped box, a never-run test, a void verdict and two units that
disagree are each not comparable. The mutant that ignores tests absent from the new side must fail:

	python3 pilots/adamic-gate/boxparity_test.py
	sed 's/if box.get(key) != new.get(key):/if key in new and box.get(key) != new.get(key):/' boxparity.py > m.py && python3 boxparity_test.py m.py   # fails
	sed 's/if box is None and new == "pass":/if new is None or (box is None and new == "pass"):/' boxparity.py > m.py && python3 boxparity_test.py m.py   # fails

Stages: each phase stage agrees with the box's stages_exit; a phase that fails where the box passed, and a box stage with
no phase unit, are each named; tests, products and census aren't phases; gofmt, which the box doesn't run, differs only
when it fails; --no-stages compares tests alone. The second mutant, which lets a box stage with no phase pass, must fail.
"""

import json
import os
import subprocess
import sys
import tempfile

here = os.path.dirname(os.path.abspath(__file__))
boxparity = sys.argv[1] if len(sys.argv) > 1 else os.path.join(here, "boxparity.py")
failures = 0
package = "github.com/system-inc/adamic/internal/oracle"


def check(name, condition, output):
    global failures
    print(("PASS " if condition else "FAIL ") + name + ("" if condition else ": " + output))
    failures += 0 if condition else 1


def plant(root, tests, stopped=None, notRun=0, stages=None):
    """A box record: fast.json (with stages_exit when given) and test.jsonl holding each (test, action), each test
    started before it ends."""
    directory = tempfile.mkdtemp(dir=root)
    outcomes = [{"package": package, "test": test, "status": "passed"} for test, _ in tests]
    outcomes += [{"package": package, "test": "TestNeverRan%d" % index, "status": "not run"} for index in range(notRun)]
    json.dump({"sha": "a" * 40, "base": "b" * 40, "stopped": stopped, "test_outcomes": outcomes, "stages_exit": stages or {}}, open(os.path.join(directory, "fast.json"), "w"))
    with open(os.path.join(directory, "test.jsonl"), "w") as stream:
        for test, action in tests:
            stream.write(json.dumps({"Action": "run", "Package": package, "Test": test}) + "\n")
            stream.write(json.dumps({"Action": "output", "Package": package, "Test": test, "Output": "--- " + action}) + "\n")
            stream.write(json.dumps({"Action": action, "Package": package, "Test": test}) + "\n")
    return directory


def verdicts(root, units, phases=()):
    """A new path's verdict records: units is [(unitKey, status, [(test, outcome)])], phases [(unitKey, status, line)]."""
    path = tempfile.mktemp(dir=root, suffix=".jsonl")
    with open(path, "w") as stream:
        for unitKey, status, tests in units:
            stream.write(json.dumps({"unitKey": unitKey, "change": "chg_test", "status": status,
                                     "tests": [{"package": package, "test": test, "outcome": outcome} for test, outcome in tests]}) + "\n")
        for unitKey, status, line in phases:
            stream.write(json.dumps({"unitKey": unitKey, "change": "chg_test", "status": status, "tests": [], "phase": line}) + "\n")
    return path


def run(*arguments):
    result = subprocess.run([sys.executable, boxparity] + list(arguments), capture_output=True, text=True)
    return result.returncode, result.stdout + result.stderr


root = tempfile.mkdtemp()
box = plant(root, [("TestA", "pass"), ("TestB", "fail"), ("TestC", "skip"), ("TestA/sub", "pass")])
same = [("k1", "passed", [("TestA", "pass"), ("TestA/sub", "pass")]), ("k2", "failed", [("TestB", "fail"), ("TestC", "skip")])]

code, output = run(box, verdicts(root, same))
check("identical sides agree, exit 0", code == 0 and "identical" in output and "4 tests the same" in output, output)

code, output = run(box, verdicts(root, [("k1", "passed", [("TestA", "pass"), ("TestA/sub", "pass")]), ("k2", "failed", [("TestB", "fail")])]))
check("a test the new side never ran is named, exit 1", code == 1 and "TestC: box skip, new absent" in output, output)

code, output = run(box, verdicts(root, [("k1", "passed", [("TestA", "pass"), ("TestA/sub", "pass")]), ("k2", "passed", [("TestB", "pass"), ("TestC", "skip")])]))
check("a flipped outcome is named with its unit, exit 1", code == 1 and "TestB: box fail, new pass (unit k2)" in output, output)

code, output = run(box, verdicts(root, same + [("k3", "passed", [("TestExtra", "pass")])]))
check("a test only the new side ran is named, exit 1", code == 1 and "TestExtra: box absent, new pass (unit k3)" in output, output)

code, output = run(box, verdicts(root, [("k1", "passed", [("TestA", "pass")]), ("k2", "failed", [("TestB", "fail"), ("TestC", "skip")])]), "--top-level")
check("--top-level ignores subtests on both sides", code == 0 and "3 tests the same" in output, output)

code, output = run(plant(root, [("TestA", "pass")], stopped={"reason": "over the box ceiling of 5400 s"}), verdicts(root, same))
check("a stopped box record is not comparable, exit 3", code == 3 and "box record stopped" in output, output)

code, output = run(plant(root, [("TestA", "pass")], notRun=2), verdicts(root, same))
check("a box record with never-run tests is not comparable, exit 3", code == 3 and "2 planned tests that never ran" in output, output)

code, output = run(box, verdicts(root, same + [("k4", "void", [])]))
check("a void verdict is not comparable, exit 3", code == 3 and "k4" in output and "void" in output, output)

path = verdicts(root, same)
with open(path, "a") as stream:
    stream.write(json.dumps({"unitKey": "k6", "change": "chg_test", "status": "void", "cause": "infra", "infra": "overBudgetRun", "tests": []}) + "\n")
code, output = run(box, path)
check("an over-budget void is not comparable and names its cause, exit 3", code == 3 and "k6" in output and "overBudgetRun" in output, output)

code, output = run(box, verdicts(root, same + [("k5", "passed", [("TestB", "pass")])]))
check("two units disagreeing on one test is not comparable, exit 3", code == 3 and "units k2 and k5 disagree" in output, output)

zipped = plant(root, [("TestA", "pass"), ("TestB", "fail"), ("TestC", "skip"), ("TestA/sub", "pass")])
subprocess.run(["gzip", os.path.join(zipped, "test.jsonl")], check=True)
code, output = run(zipped, verdicts(root, same))
check("an older record's test.jsonl.gz is read the same, exit 0", code == 0 and "4 tests the same" in output, output)

boxStages = {"vet": 0, "build": 0, "smoke": 0, "tests": 1, "products": 0, "census": 0}
staged = plant(root, [("TestA", "pass"), ("TestB", "fail"), ("TestC", "skip"), ("TestA/sub", "pass")], stages=boxStages)
phases = [("p1", "passed", "vet"), ("p2", "passed", "build"), ("p3", "passed", "smoke 1"), ("p4", "passed", "smoke 2"), ("p5", "passed", "gofmt")]
code, output = run(staged, verdicts(root, same, phases))
check("every phase stage agrees with the box's stages_exit, exit 0", code == 0 and "4 stages the same, 0 differ" in output, output)

code, output = run(staged, verdicts(root, same, phases[:2] + [("p3", "passed", "smoke 1"), ("p4", "failed", "smoke 2"), phases[4]]))
check("a stage with one failed unit where the box passed is named with its unit, exit 1", code == 1 and "stage smoke: box pass, new fail (units p4)" in output, output)

code, output = run(staged, verdicts(root, same, phases[:2] + [phases[4]]))
check("a box stage with no phase unit is named, exit 1", code == 1 and "stage smoke: box pass, new no phase unit" in output, output)

code, output = run(staged, verdicts(root, same, phases[:4] + [("p5", "failed", "gofmt")]))
check("gofmt, which the box doesn't run, differs when it fails, exit 1", code == 1 and "stage gofmt: box absent, new fail" in output, output)

darwin = plant(root, [("TestA", "pass"), ("TestB", "fail"), ("TestC", "skip"), ("TestA/sub", "pass")], stages=dict(boxStages, **{"darwin-compile": 0}))
code, output = run(darwin, verdicts(root, same, phases))
check("a stage ruled out by name (darwin-compile) is printed as ruled, never a difference, exit 0",
      code == 0 and "1 ruled out" in output and "stage darwin-compile: box exit 0, not on the new path, ruled" in output, output)

code, output = run(staged, verdicts(root, same, phases[:2] + [phases[4]]), "--no-stages")
check("--no-stages compares tests alone, exit 0", code == 0 and "stages not compared" in output, output)

code, output = run(os.path.join(root, "missing"), verdicts(root, same))
check("a missing box record is unreadable, exit 2", code == 2, output)

print("%d failed" % failures)
sys.exit(1 if failures else 0)
