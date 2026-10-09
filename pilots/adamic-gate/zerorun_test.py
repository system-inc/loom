#!/usr/bin/env python3
"""zerorun_test.py: which units zerorun scores, on work directories made here.

	python3 pilots/adamic-gate/zerorun_test.py
	LOOM_ZERORUN=<path to another zerorun.py> python3 pilots/adamic-gate/zerorun_test.py   # a mutant must fail
"""

import importlib.util
import json
import os
import shutil
import tempfile
import unittest

path = os.environ.get("LOOM_ZERORUN") or os.path.join(os.path.dirname(os.path.abspath(__file__)), "zerorun.py")
specification = importlib.util.spec_from_file_location("zerorun", path)
zerorun = importlib.util.module_from_spec(specification)
specification.loader.exec_module(zerorun)

package = "example.com/lint"


def unit(identifier, pattern):
    return {"id": identifier, "argv": ["bash", "-c", "run", "adamic-gate-unit", "SHA", "%s=%s" % (package, pattern)]}


def attempt(identifier, code, status, results):
    """One attempt's events, as the coordinator records them."""
    events = [{"unit": identifier, "type": "started", "machine": "m1"}, {"unit": identifier, "type": "exit", "code": code}]
    if results:
        events.append({"unit": identifier, "type": "uploaded", "path": "loom-out/test.jsonl.gz", "sha256": "0" * 64})
    events.append({"unit": identifier, "type": "finished", "status": status})
    return events


class Reported(unittest.TestCase):
    def setUp(self):
        self.work = tempfile.mkdtemp()
        self.tree = os.path.join(self.work, "tree.txt")
        with open(self.tree, "w") as out:
            out.write("%s TestAgree_0000\n%s TestAgree_0001\n" % (package, package))
        with open(os.path.join(self.work, "job.json"), "w") as out:
            json.dump({"units": [unit("tests-00", "^(TestAgree)((Unit|Points|_)[0-9]+|_Setup|_Union)?$")]}, out)

    def tearDown(self):
        shutil.rmtree(self.work)

    def write(self, name, events):
        with open(os.path.join(self.work, name), "w") as out:
            out.writelines(json.dumps(event) + "\n" for event in events)

    def ran(self, *tests):
        with open(os.path.join(self.work, "test.jsonl"), "w") as out:
            for test in tests:
                out.write(json.dumps({"Package": package, "Test": test, "Action": "run"}) + "\n")
                out.write(json.dumps({"Package": package, "Test": test, "Action": "pass"}) + "\n")

    def lines(self):
        return zerorun.sweep(self.work, (), self.tree)

    def test_a_family_that_ran_nowhere_is_red(self):
        # The check can still fail: a unit that ran and uploaded results, with none of its family in the job's tests.
        self.write("record.jsonl", attempt("tests-00", 0, "passed", True))
        self.ran()
        self.assertEqual(len(self.lines()), 1)
        self.assertIn("requested tests ran: 0", self.lines()[0])

    def test_a_family_that_ran_is_not(self):
        self.write("record.jsonl", attempt("tests-00", 0, "passed", True))
        self.ran("TestAgree_0000", "TestAgree_0001")
        self.assertEqual(self.lines(), [])

    def test_an_exit_2_attempt_is_a_break_not_a_zero_run(self):
        # A black hole failed the unit in 0.1 s, exit 2, before any test: the red list names it BROKEN.
        self.write("record.jsonl", attempt("tests-00", 2, "failed", False))
        self.ran()
        self.assertEqual(self.lines(), [])

    def test_an_attempt_with_no_results_is_a_break_not_a_zero_run(self):
        # A refusal on a full /tmp exits 0 and uploads nothing (gocacheprog 77dcb095, Oct 9).
        self.write("record.jsonl", attempt("tests-00", 0, "passed", False))
        self.ran()
        self.assertEqual(self.lines(), [])

    def test_the_latest_attempt_decides(self):
        # Broken in the run, then placed again and ran with results but none of its family: the retry is what's scored.
        self.write("record.jsonl", attempt("tests-00", 2, "failed", False))
        self.write("again-1-record.jsonl", attempt("tests-00", 0, "passed", True))
        self.ran()
        self.assertEqual(len(self.lines()), 1)
        self.ran("TestAgree_0000")
        self.assertEqual(self.lines(), [])


if __name__ == "__main__":
    unittest.main()
