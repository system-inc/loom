#!/usr/bin/env python3
"""times_test.py: the times table's prediction, each test's p90 of its last five on Loom, on tables made here.

	python3 pilots/adamic-gate/times_test.py
	LOOM_TIMES=<path to another times.py> python3 pilots/adamic-gate/times_test.py   # a mutant must fail
"""

import contextlib
import importlib.util
import io
import json
import os
import shutil
import sys
import tempfile
import unittest

path = os.environ.get("LOOM_TIMES") or os.path.join(os.path.dirname(os.path.abspath(__file__)), "times.py")
specification = importlib.util.spec_from_file_location("times", path)
times = importlib.util.module_from_spec(specification)
specification.loader.exec_module(times)
# No gate checkout here: every observation's tree is empty.
times.treeHash = lambda sha, package, cache: ""

package = "github.com/system-inc/adamic/a"
sha = "8161285ad449cc3a8cce6746120de2133c4a738a"


class Predicted(unittest.TestCase):
    def setUp(self):
        self.work = tempfile.mkdtemp()
        self.table = os.path.join(self.work, "times.json")

    def tearDown(self):
        shutil.rmtree(self.work)

    def run_times(self, *arguments):
        printed = io.StringIO()
        sys.argv = ["times.py", *arguments, "--table", self.table]
        with contextlib.redirect_stdout(printed):
            times.main()
        return printed.getvalue()

    def tsv(self, rows):
        """The tsv plan --loom-times reads, from a table of rows: test name to its seconds, oldest first."""
        with open(self.table, "w") as out:
            json.dump({package + " " + test: [{"seconds": seconds, "run": "", "sha": sha, "tree": ""} for seconds in observed] for test, observed in rows.items()}, out)
        lines = self.run_times("tsv").splitlines()
        self.assertEqual(lines[0], "loom_seconds\twhole_gate_seconds\tunit\tpackage\ttest")
        return {line.split("\t")[4]: line.split("\t")[0] for line in lines[1:]}

    def test_five_times_predict_their_nearest_rank_p90(self):
        # Nearest rank: the ceil(0.9 * 5) = 5th smallest of five, here 50 s (their mean is 30, their median 30).
        self.assertEqual(self.tsv({"TestFive": [30, 10, 50, 20, 40]}), {"TestFive": "50.00"})

    def test_three_times_predict_the_p90_of_what_exists(self):
        # The ceil(0.9 * 3) = 3rd smallest of three.
        self.assertEqual(self.tsv({"TestThree": [7, 9, 5]}), {"TestThree": "9.00"})

    def test_one_time_predicts_itself(self):
        # A table from before a test had a second run holds one time, and that time is its prediction.
        self.assertEqual(self.tsv({"TestOne": [12.5]}), {"TestOne": "12.50"})

    def test_a_table_keeps_the_last_five_runs(self):
        # Seven runs, 1 s to 7 s: the first two leave the table and the prediction is the p90 of 3 s to 7 s.
        for seconds in range(1, 8):
            record = os.path.join(self.work, "run.jsonl")
            with open(record, "w") as out:
                out.write(json.dumps({"Action": "run", "Package": package, "Test": "TestSeven", "Time": "2026-10-09T10:00:00Z"}) + "\n")
                out.write(json.dumps({"Action": "pass", "Package": package, "Test": "TestSeven", "Time": "2026-10-09T10:00:%02dZ" % seconds}) + "\n")
            self.run_times("update", record, "--sha", sha, "--run", "run-%d" % seconds)
        rows = json.load(open(self.table))[package + " TestSeven"]
        self.assertEqual([row["seconds"] for row in rows], [3, 4, 5, 6, 7])
        self.assertEqual([row["run"] for row in rows], ["run-3", "run-4", "run-5", "run-6", "run-7"])
        self.assertEqual(self.run_times("tsv").splitlines()[1], "7.00\t0\ttimes\t%s\tTestSeven" % package)


if __name__ == "__main__":
    unittest.main()
