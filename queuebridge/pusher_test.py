#!/usr/bin/env python3
"""pusher.py against real git: a bare origin standing in for GitHub, the lander's bare clone, and a stand-in pipeline.

usage: python3 queuebridge/pusher_test.py
"""
import os, subprocess, sys, tempfile, unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import pusher

change = "chg_" + "c" * 26


def git(where, *arguments):
    return subprocess.run(["git", "-C", where, *arguments], check=True, capture_output=True, text=True,
                          env=dict(os.environ, GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@t", GIT_COMMITTER_NAME="t", GIT_COMMITTER_EMAIL="t@t")).stdout.strip()


class FakePipeline:
    def __init__(self, landings):
        self.landings, self.posts = landings, []

    def call(self, method, path, body=None):
        if method == "GET":
            return 200, {"landings": self.landings}
        self.posts.append((path, body))
        return 200, {"ok": True}


class Pusher(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = self.tmp.name
        self.origin = os.path.join(root, "origin.git")
        git(root, "init", "-q", "--bare", self.origin)
        self.work = os.path.join(root, "work")
        git(root, "init", "-q", self.work)
        git(self.work, "remote", "add", "origin", self.origin)
        self.main = self.commit("main")
        git(self.work, "push", "-q", "origin", "HEAD:refs/heads/main")
        self.lander = os.path.join(root, "lander.git")
        git(root, "init", "-q", "--bare", self.lander)
        git(self.lander, "remote", "add", "origin", self.origin)
        self.hands = pusher.Hands(self.lander)

    def commit(self, message, on=None):
        if on:
            git(self.work, "checkout", "-q", "--detach", on)
        with open(os.path.join(self.work, message), "w") as handle:
            handle.write(message)
        git(self.work, "add", "-A")
        git(self.work, "commit", "-qm", message)
        return git(self.work, "rev-parse", "HEAD")

    def publish(self, sha, branch):
        # The change's sha is on GitHub, as the queue's git facts required at submit.
        git(self.work, "push", "-q", "origin", "%s:refs/heads/%s" % (sha, branch))

    def mainNow(self):
        return git(self.origin, "rev-parse", "refs/heads/main")

    def test_main_moves_only_by_fast_forward_to_the_exact_tested_tree(self):
        tested = self.commit("tested", on=self.main)
        self.publish(tested, "a")
        sibling = self.commit("sibling", on=self.main)
        self.publish(sibling, "b")
        pipeline = FakePipeline([{"change": change, "future": tested, "base": self.main, "owner": "o", "run": "r"}])
        pusher.tick(pipeline, self.hands)
        self.assertEqual(self.mainNow(), tested)
        self.assertEqual(pipeline.posts, [("/landings/" + change, {"main": tested, "from": self.main, "landed": tested})])
        # A tree built on the old main isn't a fast-forward of main now: refused, reported, and main stays.
        pipeline = FakePipeline([{"change": change, "future": sibling, "base": self.main, "owner": "o", "run": "r"}])
        pusher.tick(pipeline, self.hands)
        self.assertEqual(self.mainNow(), tested)
        self.assertEqual(len(pipeline.posts), 1)
        path, body = pipeline.posts[0]
        self.assertEqual((path, body["main"]), ("/landings/" + change, tested))
        self.assertIn("not a fast-forward", body["refused"])

    def test_a_tree_github_doesnt_have_is_held_not_refused(self):
        pipeline = FakePipeline([{"change": change, "future": "f" * 40, "base": self.main, "owner": "o", "run": "r"}])
        pusher.tick(pipeline, self.hands)
        self.assertEqual((self.mainNow(), pipeline.posts), (self.main, []))


if __name__ == "__main__":
    unittest.main()
