#!/usr/bin/env python3
"""queue_bridge.py's tick against a stand-in pipeline and gate: what it queues, posts and reports, and what it must not.

usage: python3 queuebridge/queue_bridge_test.py
"""
import os, sys, unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import queue_bridge

# These tests drive the landing step too; on the Mac it is off once workshop's pusher holds main.
queue_bridge.landsHere = True

tree = "1" * 40
other = "2" * 40
old, new = "a" * 40, "b" * 40
change = "chg_" + "c" * 26


class FakePipeline:
    def __init__(self, futures=(), landings=(), unchecked=(), paths=("a.go",)):
        self.futures, self.landings, self.unchecked, self.paths, self.calls = list(futures), list(landings), list(unchecked), list(paths), []

    def call(self, method, path, body=None):
        self.calls.append((method, path, body))
        if path.startswith("/submissions") and method == "GET":
            return 200, {"changes": self.unchecked}
        if path.startswith("/futures"):
            return 200, {"futures": self.futures}
        if path.startswith("/changes/") and method == "GET":
            return 200, {"record": {"paths": self.paths}}
        if path == "/landings" and method == "GET":
            return 200, {"landings": self.landings}
        return 200, {"ok": True}

    def posts(self):
        return [(path, body) for method, path, body in self.calls if method == "POST"]


class FakeGate:
    def __init__(self, record=None, landing=(0, "", ""), holds=True):
        self.found, self.landing, self.holds = record, landing, holds
        self.queued, self.landed, self.requeued = [], [], []

    def record(self, tree):
        return self.found

    failures = []

    def failing(self, ref):
        return self.failures

    def parents(self, sha):
        return {other: [old, tree], "3" * 40: [old, "4" * 40]}.get(sha, [])

    def facts(self, sha, base):
        return {"shaExists": True, "baseIsAncestor": True, "baseOnMain": base == old, "diffPaths": ["a.go"]}

    def queue(self, tree):
        self.queued.append(tree)
        return True

    def requeue(self, sha):
        self.requeued.append(sha)
        return True

    def land(self, record, tree, label):
        self.landed.append((record, tree))
        return self.landing

    # push-main in check-only mode: (code, stdout, stderr), and every argument list it was given.
    checkResult = (0, "checked", "")

    def check(self, arguments, label):
        self.checks = getattr(self, "checks", []) + [arguments]
        return self.checkResult

    def landTestOnly(self, tree, label):
        self.landed.append(("test-only", tree))
        return self.landing

    def landRuled(self, ruling, tree, label):
        self.landed.append(("ruled", tree))
        return self.landing

    def main(self):
        return new

    def contains(self, main, tree):
        return self.holds

    def landedAt(self, tree):
        return (new, old) if self.holds else None


def memory():
    return {"queued": [], "posted": [], "held": [], "requeued": [], "ruled": []}


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
        for status, verdict, cause in (("green", "passed", None), ("red", "void", "flake"), ("void", "void", "infra")):
            pipeline, held = FakePipeline([future]), memory()
            gate = FakeGate({"ref": "gate-logs/r/fast", "status": status, "gated": tree})
            queue_bridge.tick(pipeline, gate, held)
            queue_bridge.tick(pipeline, gate, held)
            self.assertEqual(pipeline.posts(), [("/verdicts", {"change": change, "verdict": {
                "future": tree, "run": "gate-logs/r/fast", "status": verdict, "cause": cause, "rule": "todays-gate-v0"}})])

    def test_a_parity_run_is_never_sent_through_todays_gate(self):
        pipeline, gate = FakePipeline([dict(future, parity=True)]), FakeGate({"ref": "gate-logs/r/fast", "status": "green", "gated": tree})
        queue_bridge.tick(pipeline, gate, memory())
        self.assertEqual((gate.queued, pipeline.posts()), ([], []))

    def test_a_red_that_is_only_mains_ruled_red_is_excused_and_any_other_failure_is_not(self):
        pipeline, gate = FakePipeline([future]), FakeGate({"ref": "gate-logs/r/fast", "status": "red", "gated": tree})
        gate.failures = ["stage1/cohere/gitignore TestThePortAnswersAsGoCohereAndGitDo_022"]
        queue_bridge.tick(pipeline, gate, memory())
        self.assertEqual([(body["verdict"]["status"], body["verdict"]["cause"]) for path, body in pipeline.posts()], [("failed", "mainRed")])
        pipeline, gate = FakePipeline([future]), FakeGate({"ref": "gate-logs/r/fast", "status": "red", "gated": tree})
        gate.failures = ["stage1/cohere/gitignore TestThePortAnswersAsGoCohereAndGitDo_022", "stage1/cohere/gitignore TestPatterns"]
        queue_bridge.tick(pipeline, gate, memory())
        self.assertEqual([(body["verdict"]["status"], body["verdict"]["cause"]) for path, body in pipeline.posts()], [("void", "flake")])
        self.assertEqual(queue_bridge.excusedNames(["stage1/cohere/gitignore TestThePortAnswersAsGoCohereAndGitDo_026"]),
                         ["stage1/cohere/gitignore TestThePortAnswersAsGoCohereAndGitDo_026=" + queue_bridge.mainReds[0][2]])
        self.assertIsNone(queue_bridge.excusedNames(["stage1/cohere/estree TestThePortAnswersAsGoCohereAndGitDo_026"]))

    def test_a_failure_outside_any_test_makes_a_record_unexcusable(self):
        import json
        def event(action, test=None, package="github.com/system-inc/adamic/stage1/cohere/gitignore"):
            return json.dumps({"Action": action, "Package": package, **({"Test": test} if test else {})})
        shard = "TestThePortAnswersAsGoCohereAndGitDo_022"
        clean = [event("run", shard), event("fail", shard)]
        self.assertEqual(queue_bridge.failingTests(clean, {"stages_exit": {"build": 0, "tests": 1}}), ["stage1/cohere/gitignore " + shard])
        # A package-level fail (a build break, a binary dying outside any test) beside the ruled red.
        self.assertIsNone(queue_bridge.failingTests(clean + [event("fail", package="github.com/system-inc/adamic/internal/lower")], {}))
        # A test that started and never ended.
        self.assertIsNone(queue_bridge.failingTests(clean + [event("run", "TestOther", "github.com/system-inc/adamic/internal/lower")], {}))
        # A stage other than tests that didn't pass.
        self.assertIsNone(queue_bridge.failingTests(clean, {"stages_exit": {"build": 2, "tests": 1}}))
        # No test record at all.
        self.assertIsNone(queue_bridge.failingTests(None, {}))
        self.assertIsNone(queue_bridge.excusedNames(queue_bridge.failingTests(clean + [event("fail", package="github.com/system-inc/adamic/internal/lower")], {}) or []))

    def test_a_first_red_is_served_again_and_only_a_second_red_is_the_changes(self):
        pipeline, held = FakePipeline([future]), memory()
        gate = FakeGate({"ref": "gate-logs/r1/fast", "status": "red", "gated": tree})
        queue_bridge.tick(pipeline, gate, held)
        gate.found = {"ref": "gate-logs/r2/fast", "status": "red", "gated": tree}
        queue_bridge.tick(pipeline, gate, held)
        self.assertEqual(gate.requeued, [tree])
        self.assertEqual([(body["verdict"]["status"], body["verdict"]["cause"]) for path, body in pipeline.posts()], [("void", "flake"), ("failed", "change")])

    def test_a_markdown_only_change_takes_the_ruled_gate_and_lands_through_it(self):
        pipeline, gate, held = FakePipeline([future], paths=["docs/front-door.md"]), FakeGate(), memory()
        queue_bridge.tick(pipeline, gate, held)
        queue_bridge.tick(pipeline, gate, held)
        self.assertEqual(gate.queued, [])
        self.assertEqual(pipeline.posts(), [("/verdicts", {"change": change, "verdict": {
            "future": tree, "run": "ruled-gate:docs", "status": "passed", "cause": None, "rule": "ruled-gate-docs-v0"}})])
        pushed = (0, "Pushed main %s..%s\n" % (old, new), "")
        pipeline, gate = FakePipeline(landings=[dict(order, run="ruled-gate:docs")]), FakeGate(landing=pushed)
        queue_bridge.tick(pipeline, gate, memory())
        self.assertEqual(gate.landed, [("ruled", tree)])
        # Code in the change keeps it on today's fast gate.
        pipeline, gate = FakePipeline([future], paths=["docs/front-door.md", "cmd/x/main.go"]), FakeGate()
        queue_bridge.tick(pipeline, gate, memory())
        self.assertEqual(gate.queued, [tree])

    def test_a_test_only_change_takes_the_test_only_lane(self):
        pipeline, gate = FakePipeline([future], paths=["internal/oracle/a_test.go", "internal/oracle/testdata/x.a"]), FakeGate()
        queue_bridge.tick(pipeline, gate, memory())
        self.assertEqual((gate.queued, [body["verdict"]["run"] for path, body in pipeline.posts()]), ([], ["test-only"]))
        pipeline, gate = FakePipeline(landings=[dict(order, run="test-only")]), FakeGate(landing=(0, "Pushed main aaaaaaaa..bbbbbbbb\n", ""))
        queue_bridge.tick(pipeline, gate, memory())
        self.assertEqual(gate.landed, [("test-only", tree)])
        pipeline, gate = FakePipeline([future], paths=["internal/oracle/a_test.go", "internal/oracle/a.go"]), FakeGate()
        queue_bridge.tick(pipeline, gate, memory())
        self.assertEqual(gate.queued, [tree])
        # push-main's lane checks refuse a test that isn't gofmt'd: the change's red, its reason in the run.
        pipeline, gate = FakePipeline([future], paths=["internal/oracle/a_test.go"]), FakeGate()
        gate.checkResult = (1, "", "refused: internal/oracle/a_test.go isn't gofmt-formatted")
        queue_bridge.tick(pipeline, gate, memory())
        self.assertEqual(gate.checks, [["--test-only", tree]])
        verdict = pipeline.posts()[0][1]["verdict"]
        self.assertEqual((verdict["status"], verdict["cause"]), ("failed", "change"))
        self.assertIn("gofmt", verdict["run"])

    def test_nothing_reads_green_until_push_main_s_own_checks_pass(self):
        # A green record that push-main refuses (zerorun: a unit ran none of its tests) is a void, never a pass.
        pipeline, gate = FakePipeline([future]), FakeGate({"ref": "gate-logs/r/fast", "status": "green", "gated": tree})
        gate.checkResult = (1, "", "refused: gate-logs/r/fast has units that ran none of the tests they named")
        queue_bridge.tick(pipeline, gate, memory())
        verdict = pipeline.posts()[0][1]["verdict"]
        self.assertEqual((verdict["status"], verdict["cause"]), ("void", "infra"))
        self.assertIn("ran none of the tests", verdict["rule"])
        self.assertEqual(gate.checks, [["--fast-gate", "gate-logs/r/fast", tree]])
        # main's ruled red goes to push-main by name, which rechecks its output.
        pipeline, gate = FakePipeline([future]), FakeGate({"ref": "gate-logs/r/fast", "status": "red", "gated": tree})
        gate.failures = ["stage1/cohere/gitignore TestThePortAnswersAsGoCohereAndGitDo_022"]
        queue_bridge.tick(pipeline, gate, memory())
        self.assertEqual(gate.checks[0][:3], ["--fast-gate", "gate-logs/r/fast", "--infra-red"])
        self.assertEqual(pipeline.posts()[0][1]["verdict"]["cause"], "mainRed")
        # A docs change takes the ruled gate's census; a hold posts nothing.
        pipeline, gate = FakePipeline([future], paths=["docs/a.md"]), FakeGate()
        gate.checkResult = (3, "", "held")
        queue_bridge.tick(pipeline, gate, memory())
        self.assertEqual((gate.checks[0][0], pipeline.posts()), ("--ruled-gate", []))
        # A check that didn't run (git's lock race in the shared checkout) posts nothing and runs again; only
        # push-main's refusal is the change's red.
        pipeline, gate, held = FakePipeline([future], paths=["docs/a.md"]), FakeGate(), memory()
        gate.checkResult = (1, "", "error: cannot lock ref 'refs/remotes/origin/x': is at 1 but expected 2")
        queue_bridge.tick(pipeline, gate, held)
        self.assertEqual((pipeline.posts(), held["ruled"]), ([], []))
        gate.checkResult = (1, "", "refused: not test-only against main 0bf6186d: cmd/x/main.go")
        queue_bridge.tick(pipeline, gate, held)
        verdict = pipeline.posts()[0][1]["verdict"]
        self.assertEqual((verdict["status"], verdict["cause"]), ("failed", "change"))

    def test_the_mac_lands_nothing_once_the_pusher_holds_main(self):
        queue_bridge.landsHere = False
        try:
            pipeline, gate = FakePipeline(landings=[order]), FakeGate(landing=(0, "", ""))
            queue_bridge.tick(pipeline, gate, memory())
            self.assertEqual((gate.landed, pipeline.posts()), ([], []))
        finally:
            queue_bridge.landsHere = True

    def test_once_judge_decides_every_future_the_bridge_only_carries_gits_facts(self):
        queue_bridge.decidesHere = False
        try:
            submitted = [{"change": change, "sha": tree, "base": old, "paths": ["a.go"]}]
            # Every lane today's gate decides: the fast gate (a record, or none yet to queue), docs and test-only.
            for paths, record in ((["a.go"], {"ref": "gate-logs/r/fast", "status": "green", "gated": tree}), (["a.go"], None),
                                  (["docs/a.md"], None), (["internal/x/a_test.go"], None)):
                pipeline, gate = FakePipeline([future], unchecked=submitted, paths=paths), FakeGate(record)
                queue_bridge.tick(pipeline, gate, memory())
                self.assertEqual([path for path, body in pipeline.posts()], ["/submissions/%s/facts" % change])
                self.assertEqual((gate.queued, getattr(gate, "checks", []), gate.requeued), ([], [], []))
        finally:
            queue_bridge.decidesHere = True

    def test_a_void_is_served_once_more_and_only_once(self):
        pipeline, held = FakePipeline([future]), memory()
        gate = FakeGate({"ref": "gate-logs/r1/fast", "status": "void", "gated": tree})
        queue_bridge.tick(pipeline, gate, held)
        gate.found = {"ref": "gate-logs/r2/fast", "status": "void", "gated": tree}
        queue_bridge.tick(pipeline, gate, held)
        self.assertEqual(gate.requeued, [tree])
        self.assertEqual(len(pipeline.posts()), 2)

    def test_a_gate_merge_of_the_tree_is_its_future_and_any_other_tree_is_void(self):
        pipeline = FakePipeline([future])
        queue_bridge.tick(pipeline, FakeGate({"ref": "gate-logs/r/fast", "status": "green", "gated": other}), memory())
        self.assertEqual(pipeline.posts(), [("/verdicts", {"change": change, "gateMerge": {"base": old}, "verdict": {
            "future": other, "run": "gate-logs/r/fast", "status": "passed", "cause": None, "rule": "todays-gate-v0"}})])
        pipeline = FakePipeline([future])
        queue_bridge.tick(pipeline, FakeGate({"ref": "gate-logs/r/fast", "status": "green", "gated": "3" * 40}), memory())
        self.assertEqual([(body["verdict"]["future"], body["verdict"]["status"]) for path, body in pipeline.posts()], [(tree, "void")])

    def test_a_landing_is_reported_with_the_tree_only_when_main_holds_it(self):
        pushed = (0, "Pushed main %s..%s\n" % (old, new), "")
        pipeline, gate = FakePipeline(landings=[order]), FakeGate(landing=pushed)
        queue_bridge.tick(pipeline, gate, memory())
        self.assertEqual(gate.landed, [(order["run"], tree)])
        self.assertEqual(pipeline.posts(), [("/landings/" + change, {"main": new, "from": old, "landed": tree})])
        pipeline, gate = FakePipeline(landings=[order]), FakeGate(landing=pushed, holds=False)
        queue_bridge.tick(pipeline, gate, memory())
        self.assertEqual(pipeline.posts(), [])
        # push-main prints 8-character shas, and a retry after a landing answers "already holds": git decides.
        pipeline, gate = FakePipeline(landings=[order]), FakeGate(landing=(0, "Pushed main aaaaaaaa..bbbbbbbb, 2 commits\n", ""))
        queue_bridge.tick(pipeline, gate, memory())
        self.assertEqual(pipeline.posts(), [("/landings/" + change, {"main": new, "from": old, "landed": tree})])
        pipeline, gate = FakePipeline(landings=[order]), FakeGate(landing=(1, "", "refused: main %s already holds %s" % (new[:8], tree[:8])))
        queue_bridge.tick(pipeline, gate, memory())
        self.assertEqual(pipeline.posts(), [("/landings/" + change, {"main": new, "from": old, "landed": tree})])

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
