#!/usr/bin/env python3
"""queue_bridge.py's tick against a stand-in pipeline and gate: what it queues, posts and reports, and what it must not.

usage: python3 queuebridge/queue_bridge_test.py
"""
import os, sys, unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import queue_bridge

tree = "1" * 40
other = "2" * 40
old, new = "a" * 40, "b" * 40
change = "chg_" + "c" * 26


class FakePipeline:
    def __init__(self, futures=(), landings=(), unchecked=()):
        self.futures, self.landings, self.unchecked, self.calls = list(futures), list(landings), list(unchecked), []

    def call(self, method, path, body=None):
        self.calls.append((method, path, body))
        if path.startswith("/submissions") and method == "GET":
            return 200, {"changes": self.unchecked}
        if path.startswith("/futures"):
            return 200, {"futures": self.futures}
        if path == "/landings" and method == "GET":
            return 200, {"landings": self.landings}
        return 200, {"ok": True}

    def posts(self):
        return [(path, body) for method, path, body in self.calls if method == "POST"]


class FakeGate:
    def __init__(self, record=None, landing=(0, "", ""), holds=True):
        self.found, self.landing, self.holds = record, landing, holds
        self.queued, self.landed = [], []

    def record(self, tree):
        return self.found

    def facts(self, sha, base):
        return {"shaExists": True, "baseIsAncestor": True, "baseOnMain": base == old, "diffPaths": ["a.go"]}

    def queue(self, tree):
        self.queued.append(tree)
        return True

    def land(self, record, tree, label):
        self.landed.append((record, tree))
        return self.landing

    def main(self):
        return new

    def contains(self, main, tree):
        return self.holds


def memory():
    return {"queued": [], "posted": [], "held": []}


future = {"future": tree, "tree": tree, "base": old, "changes": [change]}
order = {"change": change, "future": tree, "base": old, "owner": "system_adamic_compiler", "run": "gate-logs/111111111111/s/fast"}


class Tick(unittest.TestCase):
    def test_every_unchecked_change_gets_gits_facts(self):
        pipeline = FakePipeline(unchecked=[{"change": change, "sha": tree, "base": old, "paths": ["a.go"]}])
        queue_bridge.tick(pipeline, FakeGate(), memory())
        self.assertEqual(pipeline.posts(), [("/submissions/%s/facts" % change, {"shaExists": True, "baseIsAncestor": True, "baseOnMain": True, "diffPaths": ["a.go"]})])

    def test_a_future_with_no_record_is_queued_once_and_decided_by_nothing(self):
        pipeline, gate, held = FakePipeline([future]), FakeGate(), memory()
        queue_bridge.tick(pipeline, gate, held)
        queue_bridge.tick(pipeline, gate, held)
        self.assertEqual(gate.queued, [tree])
        self.assertEqual(pipeline.posts(), [])

    def test_a_record_of_the_tree_is_its_verdict_posted_once(self):
        for status, verdict, cause in (("green", "passed", None), ("red", "failed", "change"), ("void", "void", "infra")):
            pipeline, held = FakePipeline([future]), memory()
            gate = FakeGate({"ref": "gate-logs/r/fast", "status": status, "gated": tree})
            queue_bridge.tick(pipeline, gate, held)
            queue_bridge.tick(pipeline, gate, held)
            self.assertEqual(pipeline.posts(), [("/verdicts", {"change": change, "verdict": {
                "future": tree, "run": "gate-logs/r/fast", "status": verdict, "cause": cause, "rule": "todays-gate-v0"}})])

    def test_a_green_record_of_another_tree_never_passes_this_one(self):
        pipeline = FakePipeline([future])
        queue_bridge.tick(pipeline, FakeGate({"ref": "gate-logs/r/fast", "status": "green", "gated": other}), memory())
        self.assertEqual([body["verdict"]["status"] for path, body in pipeline.posts()], ["void"])

    def test_a_landing_is_reported_with_the_tree_only_when_main_holds_it(self):
        pushed = (0, "Pushed main %s..%s\n" % (old, new), "")
        pipeline, gate = FakePipeline(landings=[order]), FakeGate(landing=pushed)
        queue_bridge.tick(pipeline, gate, memory())
        self.assertEqual(gate.landed, [(order["run"], tree)])
        self.assertEqual(pipeline.posts(), [("/landings/" + change, {"main": new, "from": old, "landed": tree})])
        pipeline, gate = FakePipeline(landings=[order]), FakeGate(landing=pushed, holds=False)
        queue_bridge.tick(pipeline, gate, memory())
        self.assertEqual(pipeline.posts(), [])

    def test_a_hold_or_a_pause_waits_and_a_refusal_is_reported(self):
        for landing in ((3, "", "held: rerun pending"), (1, "", "refused: landings are paused, main's newest finished whole gate is red")):
            pipeline = FakePipeline(landings=[order])
            queue_bridge.tick(pipeline, FakeGate(landing=landing), memory())
            self.assertEqual(pipeline.posts(), [], landing)
        pipeline = FakePipeline(landings=[order])
        queue_bridge.tick(pipeline, FakeGate(landing=(1, "", "refused: the record gated another sha")), memory())
        self.assertEqual(pipeline.posts(), [("/landings/" + change, {"refused": "refused: the record gated another sha", "main": new})])


if __name__ == "__main__":
    unittest.main()
