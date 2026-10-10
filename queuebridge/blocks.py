#!/usr/bin/env python3
"""The block builder (#7hn5em0), on workshop beside the pusher, since the queue's Durable Object can't run git. For each
block the queue opened and nobody built, it writes the block as a chain of merge commits on main's tip (main, +A, +B,
...), each prefix a real commit, pushes the chain's tip as refs/loom/blocks/<n> (every prefix is on its first-parent
line), and posts the chain to the queue: each prefix becomes the newest change's future, so halving a red block reuses
work and landing a green prefix is a fast-forward to a tested commit. A change that conflicts with the changes ahead of
it is left out and posted with the conflicting paths, which parks it and reaches its owner; the chain goes on without it.

Nothing here runs with blocks off: the queue opens no block until a rule.changed turns them on.
"""
import os, subprocess

identity = {"GIT_AUTHOR_NAME": "kirkouimet", "GIT_AUTHOR_EMAIL": "kirk@kirkouimet.com",
            "GIT_COMMITTER_NAME": "kirkouimet", "GIT_COMMITTER_EMAIL": "kirk@kirkouimet.com"}


class Chain:
    """Merge chains in a bare clone whose origin is GitHub (the lander's own, on workshop)."""

    def __init__(self, repository):
        self.repository = repository

    def git(self, *arguments):
        return subprocess.run(["git", "-C", self.repository, *arguments], capture_output=True, text=True, timeout=300,
                              env=dict(os.environ, **identity))

    def main(self):
        listed = self.git("ls-remote", "origin", "refs/heads/main").stdout.split()
        return listed[0] if listed else None

    def build(self, block, base, changes):
        """(prefixes [{tree, change}], conflicts [{change, paths}], the chain's tip), or None when git can't fetch."""
        if self.git("fetch", "-q", "--no-tags", "origin", base, *[change["sha"] for change in changes]).returncode != 0:
            return None
        prefixes, conflicts, prefix = [], [], base
        for change in changes:
            merged = self.git("merge-tree", "--write-tree", "--name-only", "--no-messages", prefix, change["sha"])
            lines = merged.stdout.splitlines()
            if merged.returncode == 1:
                conflicts.append({"change": change["change"], "paths": sorted(line for line in lines[1:] if line)})
                continue
            if merged.returncode != 0 or not lines:
                return None
            commit = self.git("commit-tree", lines[0], "-p", prefix, "-p", change["sha"],
                              "-m", "Loom block %d: %s (%s)" % (block, change["change"], change["sha"][:12])).stdout.strip()
            if not commit:
                return None
            prefixes.append({"tree": commit, "change": change["change"]})
            prefix = commit
        return prefixes, conflicts, prefix

    def publish(self, block, tip):
        return self.git("push", "-q", "origin", "%s:refs/loom/blocks/%d" % (tip, block)).returncode == 0


def tick(pipeline, chain, log):
    status, listed = pipeline.call("GET", "/blocks?state=unbuilt")
    if status != 200:
        log("blocks: %d %s" % (status, listed))
        return
    for block in listed["blocks"]:
        base = chain.main()
        built = None if base is None else chain.build(block["block"], base, block["changes"])
        if built is None:
            log("block %d: git couldn't build it; holding" % block["block"])
            continue
        prefixes, conflicts, tip = built
        if prefixes and not chain.publish(block["block"], tip):
            log("block %d: pushing refs/loom/blocks/%d failed; holding" % (block["block"], block["block"]))
            continue
        status, answer = pipeline.call("POST", "/blocks/%d/built" % block["block"], {"base": base, "prefixes": prefixes, "conflicts": conflicts})
        log("block %d built on main %s: %d prefixes, %d conflicts; %d %s" % (block["block"], base[:12], len(prefixes), len(conflicts), status, answer))
