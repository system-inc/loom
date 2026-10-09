#!/usr/bin/env python3
"""keptunits_test.py: a unit naming only kept tests leaves the plan, with the needs on it; anything else stays.

	python3 pilots/adamic-gate/keptunits_test.py

Mutant: fullyKept reading a remainder (". skip=...") as kept must fail test_a_remainder_stays.
"""

import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import keptunits  # noqa: E402

lint = "github.com/system-inc/adamic/stage1/cohere/lint"
typeaware = "github.com/system-inc/adamic/stage1/cohere/typeaware"


def unit(identifier, *specs, needs=None):
    result = {"id": identifier, "argv": ["bash", "-c", "body", "adamic-gate-unit", "a" * 40, *specs], "timeoutSeconds": 600}
    if needs:
        result["needs"] = needs
    return result


class KeptUnits(unittest.TestCase):
    def setUp(self):
        self.kept = {lint: ["TestProduct_A", "TestProduct_B"], typeaware: ["TestProduct_C"]}

    def test_a_product_unit_whose_products_are_all_kept_leaves_with_the_needs_on_it(self):
        job = {"units": [unit("product-00", lint + "=^(TestProduct_A|TestProduct_B)$"), unit("product-01", typeaware + "=^(TestProduct_C|TestProduct_D)$"),
                         unit("tests-00", lint + "=^(TestX)$", needs=["product-00", "product-01"]), unit("tests-01", lint + "=^(TestY)$", needs=["product-00"])]}
        self.assertEqual(keptunits.prune(job, self.kept), ["product-00"])
        self.assertEqual([u["id"] for u in job["units"]], ["product-01", "tests-00", "tests-01"])
        self.assertEqual(job["units"][1]["needs"], ["product-01"])
        self.assertNotIn("needs", job["units"][2])

    def test_a_kept_spec_leaves_a_unit_that_holds_others(self):
        job = {"units": [unit("product-00", lint + "=^(TestProduct_A)$", typeaware + "=^(TestProduct_D)$")]}
        self.assertEqual(keptunits.prune(job, self.kept), [])
        self.assertEqual(job["units"][0]["argv"][5:], [typeaware + "=^(TestProduct_D)$"])

    def test_a_spec_with_a_skip_is_judged_by_what_it_runs(self):
        job = {"units": [unit("product-00", lint + "=^(TestProduct_A|TestProduct_B)$ skip=^(TestProduct_A|TestProduct_B)$")]}
        self.assertEqual(keptunits.prune(job, self.kept), ["product-00"])

    def test_a_remainder_stays(self):
        job = {"units": [unit("tests-00", lint + "=. skip=^(TestProduct_A|TestProduct_B)$")]}
        self.assertEqual(keptunits.prune(job, self.kept), [])

    def test_a_family_or_pattern_stays(self):
        job = {"units": [unit("tests-00", lint + "=^(TestProduct_A)((Unit|Points|_)[0-9]+|_Setup|_Union)?$"), unit("tests-01", lint + "=^TestProduct_A$")]}
        self.assertEqual(keptunits.prune(job, self.kept), [])

    def test_a_test_kept_in_another_package_keeps_nothing_here(self):
        job = {"units": [unit("product-00", typeaware + "=^(TestProduct_A)$")]}
        self.assertEqual(keptunits.prune(job, self.kept), [])

    def test_build_vet_and_phases_are_never_touched(self):
        job = {"units": [{"id": "build-vet", "argv": ["bash", "-c", "b", "adamic-build-vet", "a" * 40]}, {"id": "phase-vet", "argv": ["bash", "-c", "b", "adamic-gate-phase", "a" * 40, "t" * 40, "vet"]}]}
        self.assertEqual(keptunits.prune(job, self.kept), [])
        self.assertEqual(len(job["units"]), 2)


if __name__ == "__main__":
    unittest.main()
