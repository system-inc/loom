#!/usr/bin/env python3
"""inputs_test.py: each part of loom-inputs-v1 moves exactly the units it should, on a repository made here.

	python3 pilots/adamic-gate/inputs_test.py
"""

import json
import os
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import inputs  # noqa: E402

module = inputs.module


def git(repository, *arguments):
    return subprocess.run(["git", "-C", repository, *arguments], check=True, capture_output=True, text=True).stdout.strip()


def testUnit(identifier, *specs):
    return {"id": identifier, "argv": ["bash", "-c", "gateInputs=" + "a" * 64 + "\nrun", "adamic-gate-unit", "SHA", *specs],
            "timeoutSeconds": 90, "resources": {"cpus": 4}}


def phaseUnit(identifier, phase):
    return {"id": identifier, "argv": ["bash", "-c", "gateInputs=" + "a" * 64 + "\nphase", "adamic-gate-phase", "SHA", "t" * 40, phase],
            "timeoutSeconds": 3600, "resources": {"cpus": 4}}


class Inputs(unittest.TestCase):
    def setUp(self):
        self.repository = tempfile.mkdtemp()
        git(self.repository, "init", "-q")
        git(self.repository, "config", "user.email", "loom@test")
        git(self.repository, "config", "user.name", "loom")
        self.write({"go.mod": "module " + module.rstrip("/") + "\n", "a/a.go": "package a\n", "a/a_test.go": "package a\n",
                    "a/testdata/x.txt": "x\n", "b/b.go": "package b\n", "b/b_test.go": "package b\n", "README": "r\n"})
        self.base = self.commit()
        self.job = {"name": "j", "units": [
            testUnit("tests-a", module + "a=^TestA$"),
            testUnit("tests-b", module + "b=."),
            testUnit("tests-rest", module + "b=^TestB$", "@unplanned=" + module + "a," + module + "b"),
            phaseUnit("phase-vet", "vet"),
            phaseUnit("phase-census", "census"),
        ]}

    def write(self, files):
        for path, text in files.items():
            full = os.path.join(self.repository, path)
            os.makedirs(os.path.dirname(full), exist_ok=True)
            open(full, "w").write(text)

    def commit(self):
        git(self.repository, "add", "-A")
        git(self.repository, "commit", "-q", "-m", "c", "--allow-empty")
        return git(self.repository, "rev-parse", "HEAD")

    def rerun(self, files, verdicts=None, base=None):
        """The units a rerun runs after files change, and its plan's lines."""
        self.write(files)
        sha = self.commit()
        out = tempfile.mkdtemp()
        verdicts = verdicts or {unit["id"]: "passed" for unit in self.job["units"]}
        inputs.rerunPlan(base or self.base, sha, verdicts, [self.job], out, self.repository)
        plan = dict(line.split(" ", 1) for line in open(os.path.join(out, "plan.txt")).read().splitlines())
        return sorted(unit for unit, word in plan.items() if word.startswith("rerun")), plan, out

    def test_a_test_file_moves_its_own_packages_units_and_the_phases(self):
        moved, plan, _ = self.rerun({"a/a_test.go": "package a\n// split\n"})
        self.assertEqual(moved, ["phase-vet", "tests-a"])
        self.assertEqual(plan["tests-b"], "kept")

    def test_testdata_moves_its_own_package(self):
        moved, _, _ = self.rerun({"a/testdata/x.txt": "y\n"})
        self.assertEqual(moved, ["phase-vet", "tests-a"])

    def test_a_non_test_change_moves_every_unit(self):
        moved, _, _ = self.rerun({"b/b.go": "package b\n// changed\n"})
        self.assertEqual(moved, ["phase-vet", "tests-a", "tests-b", "tests-rest"])
        moved, _, _ = self.rerun({"README": "changed\n"}, base=git(self.repository, "rev-parse", "HEAD"))
        self.assertEqual(moved, ["phase-vet", "tests-a", "tests-b", "tests-rest"])

    def test_a_new_package_moves_the_remainder_unit_only(self):
        moved, _, _ = self.rerun({"c/c_test.go": "package c\n"})
        self.assertEqual(moved, ["phase-vet", "tests-rest"])

    def test_a_declared_read_moves_the_reader(self):
        self.write({inputs.testReadsPath: json.dumps({"b": ["a/testdata"]})})
        base = self.commit()
        moved, _, _ = self.rerun({"a/testdata/x.txt": "z\n"}, base=base)
        self.assertEqual(moved, ["phase-vet", "tests-a", "tests-b", "tests-rest"])

    def test_review_evidence_and_landing_records_move_no_test_unit(self):
        moved, _, _ = self.rerun({"review/x/notes.md": "n\n", "documentation/velocity/landings.csv": "row\n",
                                  "stage3/progress.json": "{}\n", "stage3/meter/runs/1/log": "l\n"})
        self.assertEqual(moved, ["phase-vet"])

    def test_the_known_review_read_moves_its_reader(self):
        self.job["units"].append(testUnit("tests-lower", module + "internal/lower=."))
        moved, _, _ = self.rerun({"review/optional-indexing/map-shape.a": "shape\n"})
        self.assertEqual(moved, ["phase-vet", "tests-lower"])

    def test_the_gate_command_reads_the_whole_tree(self):
        self.job["units"].append(testUnit("tests-gate", module + "cmd/adamic-gate=."))
        moved, _, _ = self.rerun({"b/b_test.go": "package b\n// t.Parallel\n"})
        self.assertEqual(moved, ["phase-vet", "tests-b", "tests-gate", "tests-rest"])

    def test_the_trees_own_declaration_replaces_the_stand_in(self):
        self.write({inputs.testReadsPath: json.dumps({"a": ["README"]})})
        base = self.commit()
        self.job["units"].append(testUnit("tests-lower", module + "internal/lower=."))
        moved, _, _ = self.rerun({"review/optional-indexing/map-shape.a": "s\n", "README": "r2\n"}, base=base)
        # README is shared, so everything moves; the review read is no longer declared, so only phase-vet and the
        # README's moves remain, and tests-lower moves through shared alone.
        self.assertIn("tests-lower", moved)
        moved, _, _ = self.rerun({"review/optional-indexing/map-shape.a": "s2\n"}, base=git(self.repository, "rev-parse", "HEAD"))
        self.assertEqual(moved, ["phase-vet"])

    def test_an_undeclared_read_is_not_seen(self):
        # The definition's known edge, named so a change to it is a decision: b reading a's testdata without a
        # declaration keeps its verdict. test-reads.json is where such reads are declared.
        moved, _, _ = self.rerun({"a/testdata/x.txt": "w\n"})
        self.assertNotIn("tests-b", moved)

    def test_a_unit_that_was_not_green_runs_again_unchanged(self):
        verdicts = {"tests-a": "passed", "tests-b": "killed", "tests-rest": "passed", "phase-vet": "passed"}
        moved, plan, _ = self.rerun({"a/a_test.go": "package a\n// fix\n"}, verdicts=verdicts)
        self.assertEqual(moved, ["phase-vet", "tests-a", "tests-b"])
        self.assertEqual(plan["tests-b"], "rerun: was killed")
        self.assertEqual(plan["tests-a"], "rerun: inputs moved")

    def test_the_census_is_left_to_the_rerun_and_every_unit_is_hashed_at_the_new_sha(self):
        moved, plan, out = self.rerun({"a/a_test.go": "package a\n// x\n"})
        self.assertNotIn("phase-census", plan)
        here = json.load(open(os.path.join(out, "inputs.json")))
        base = json.load(open(os.path.join(out, "inputs-base.json")))
        self.assertEqual(here["definition"], "loom-inputs-v1")
        self.assertEqual([unit["id"] for unit in here["units"]], ["tests-a", "tests-b", "tests-rest", "phase-vet"])
        same = {unit["id"] for unit in here["units"]} - set(moved)
        for unit in same:
            self.assertEqual(next(u for u in here["units"] if u["id"] == unit)["input_sha256"], next(u for u in base["units"] if u["id"] == unit)["input_sha256"])
        rerunJob = json.load(open(os.path.join(out, "rerun-0.json")))
        self.assertEqual([unit["id"] for unit in rerunJob["units"]], ["tests-a", "phase-vet"])
        self.assertEqual({unit["argv"][4] for unit in rerunJob["units"]}, {here["sha"]})

    def test_a_different_plan_is_a_different_unit(self):
        tree = inputs.Tree(self.repository, self.base)
        unit = inputs.retarget(self.job, self.base)["units"][0]
        other = json.loads(json.dumps(unit))
        other["argv"][5] = module + "a=^TestOther$"
        self.assertNotEqual(inputs.unitInputs(unit, tree)["input_sha256"], inputs.unitInputs(other, tree)["input_sha256"])
        other = json.loads(json.dumps(unit))
        other["timeoutSeconds"] = 91
        self.assertNotEqual(inputs.unitInputs(unit, tree)["input_sha256"], inputs.unitInputs(other, tree)["input_sha256"])

    def test_a_sha_off_the_base_line_is_refused(self):
        git(self.repository, "checkout", "-q", "--orphan", "elsewhere")
        self.write({"b/b.go": "package b\n// elsewhere\n"})
        elsewhere = self.commit()
        with self.assertRaises(ValueError):
            inputs.rerunPlan(git(self.repository, "rev-parse", "HEAD~0"), self.base, {}, [self.job], tempfile.mkdtemp(), self.repository)
        with self.assertRaises(ValueError):
            inputs.rerunPlan(self.base, elsewhere, {}, [self.job], tempfile.mkdtemp(), self.repository)


if __name__ == "__main__":
    unittest.main()
