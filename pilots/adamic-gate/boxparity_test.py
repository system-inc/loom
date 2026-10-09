#!/usr/bin/env python3
"""boxparity_test.py [<boxparity.py>]: boxparity.py on planted records. Identical sides agree; one test missing or
flipped on either side is named with its unit; a stopped box, a never-run test, a void verdict and two units that
disagree are each not comparable. The mutant that ignores tests absent from the new side must fail:

	python3 pilots/adamic-gate/boxparity_test.py
	sed 's/if box.get(key) != new.get(key):/if key in new and box.get(key) != new.get(key):/' boxparity.py > m.py && python3 boxparity_test.py m.py   # fails
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


def plant(root, tests, stopped=None, notRun=0):
    """A box record: fast.json and test.jsonl holding each (test, action), each test started before it ends."""
    directory = tempfile.mkdtemp(dir=root)
    outcomes = [{"package": package, "test": test, "status": "passed"} for test, _ in tests]
    outcomes += [{"package": package, "test": "TestNeverRan%d" % index, "status": "not run"} for index in range(notRun)]
    json.dump({"sha": "a" * 40, "base": "b" * 40, "stopped": stopped, "test_outcomes": outcomes}, open(os.path.join(directory, "fast.json"), "w"))
    with open(os.path.join(directory, "test.jsonl"), "w") as stream:
        for test, action in tests:
            stream.write(json.dumps({"Action": "run", "Package": package, "Test": test}) + "\n")
            stream.write(json.dumps({"Action": "output", "Package": package, "Test": test, "Output": "--- " + action}) + "\n")
            stream.write(json.dumps({"Action": action, "Package": package, "Test": test}) + "\n")
    return directory


def verdicts(root, units):
    """A new path's verdict records: units is [(unitKey, status, [(test, outcome)])]."""
    path = tempfile.mktemp(dir=root, suffix=".jsonl")
    with open(path, "w") as stream:
        for unitKey, status, tests in units:
            stream.write(json.dumps({"unitKey": unitKey, "change": "chg_test", "status": status,
                                     "tests": [{"package": package, "test": test, "outcome": outcome} for test, outcome in tests]}) + "\n")
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

code, output = run(box, verdicts(root, same + [("k5", "passed", [("TestB", "pass")])]))
check("two units disagreeing on one test is not comparable, exit 3", code == 3 and "units k2 and k5 disagree" in output, output)

code, output = run(os.path.join(root, "missing"), verdicts(root, same))
check("a missing box record is unreadable, exit 2", code == 2, output)

print("%d failed" % failures)
sys.exit(1 if failures else 0)
