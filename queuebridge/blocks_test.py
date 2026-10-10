#!/usr/bin/env python3
"""blocks.py against real git: a bare origin for GitHub, a bare builder clone, and a stand-in queue.

usage: python3 queuebridge/blocks_test.py
"""
import os, subprocess, sys, tempfile, unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import blocks


def git(where, *arguments):
    return subprocess.run(["git", "-C", where, *arguments], check=True, capture_output=True, text=True,
                          env=dict(os.environ, **blocks.identity)).stdout.strip()


class FakePipeline:
    def __init__(self, listed):
        self.listed, self.posts = listed, []

    def call(self, method, path, body=None):
        if method == "GET":
            return 200, {"blocks": self.listed}
        self.posts.append((path, body))
        return 200, {"ok": True}


class Build(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = self.tmp.name
        self.origin = os.path.join(root, "origin.git")
        git(root, "init", "-q", "--bare", self.origin)
        self.work = os.path.join(root, "work")
        git(root, "init", "-q", self.work)
        git(self.work, "remote", "add", "origin", self.origin)
        self.main = self.commit({"x.txt": "x\n", "y.txt": "y\n"}, "main")
        git(self.work, "push", "-q", "origin", "HEAD:refs/heads/main")
        self.builder = os.path.join(root, "builder.git")
        git(root, "init", "-q", "--bare", self.builder)
        git(self.builder, "remote", "add", "origin", self.origin)

    def commit(self, files, message, on=None):
        if on:
            git(self.work, "checkout", "-q", "--detach", on)
        for name, text in files.items():
            with open(os.path.join(self.work, name), "w") as handle:
                handle.write(text)
        git(self.work, "add", "-A")
        git(self.work, "commit", "-qm", message)
        sha = git(self.work, "rev-parse", "HEAD")
        git(self.work, "push", "-q", "origin", "%s:refs/heads/%s" % (sha, message))
        return sha

    def test_a_block_is_a_merge_chain_on_main_and_a_conflict_is_left_out_with_its_paths(self):
        a = self.commit({"x.txt": "x from a\n"}, "a", on=self.main)
        b = self.commit({"y.txt": "y from b\n"}, "b", on=self.main)
        c = self.commit({"x.txt": "x from c\n"}, "c", on=self.main)
        changes = [{"change": "chg_a", "sha": a}, {"change": "chg_b", "sha": b}, {"change": "chg_c", "sha": c}]
        pipeline = FakePipeline([{"block": 1, "changes": changes}])
        blocks.tick(pipeline, blocks.Chain(self.builder), lambda text: None)
        path, body = pipeline.posts[0]
        self.assertEqual(path, "/blocks/1/built")
        self.assertEqual(body["base"], self.main)
        self.assertEqual([prefix["change"] for prefix in body["prefixes"]], ["chg_a", "chg_b"])
        self.assertEqual(body["conflicts"], [{"change": "chg_c", "paths": ["x.txt"]}])
        first, second = (prefix["tree"] for prefix in body["prefixes"])
        # Each prefix is a real commit: the one before it (main for the first) and the change, with both changes' content.
        self.assertEqual(git(self.origin, "rev-list", "--parents", "-n", "1", first).split()[1:], [self.main, a])
        self.assertEqual(git(self.origin, "rev-list", "--parents", "-n", "1", second).split()[1:], [first, b])
        self.assertEqual(git(self.origin, "show", second + ":x.txt"), "x from a")
        self.assertEqual(git(self.origin, "show", second + ":y.txt"), "y from b")
        # The chain's tip is on GitHub under refs/loom/blocks/1, and landing a prefix is a fast-forward from main.
        self.assertEqual(git(self.origin, "rev-parse", "refs/loom/blocks/1"), second)
        subprocess.run(["git", "-C", self.origin, "merge-base", "--is-ancestor", self.main, first], check=True)


if __name__ == "__main__":
    unittest.main()
