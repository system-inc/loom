#!/usr/bin/env python3
"""The lander's hands (#83m6zw8): the only thing that moves adamic main, from workshop, with the lander's deploy key
(Host github-lander, ~/.ssh/loom_lander), never from Kirk's Mac. Each tick it pulls loom-pipeline's landing orders and,
in line order, fast-forwards main to each order's exact tested tree:

    git push origin <tree>:refs/heads/main     (no force: GitHub refuses anything that isn't a fast-forward)

A push that lands is reported as {main, from, landed} (main is then the tree itself); a push GitHub refuses as not a
fast-forward is reported as {refused, main}, which parks the change; any other failure (the network, GitHub) is left for
the next tick. The queue decided the tree; this only moves main to it.

usage: queuebridge/pusher.py    (systemd user timer loom-pusher on workshop, every 30 s; queue_bridge.py and blocks.py beside
it, and each pass builds any block the queue opened, through blocks.py, before it lands)
"""
import fcntl, json, os, subprocess, sys, time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import blocks, queue_bridge

lander = os.environ.get("LANDER_REPOSITORY", os.path.expanduser("~/loom-lander/adamic.git"))
state = os.environ.get("LANDER_STATE", os.path.expanduser("~/loom-lander/state"))


def log(text):
    print("%s pusher: %s" % (time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), text), flush=True)


class Hands:
    """main on GitHub through the lander's own bare clone, whose origin is git@github-lander:system-inc/adamic.git."""

    def __init__(self, repository):
        self.repository = repository

    def git(self, *arguments):
        return subprocess.run(["git", "-C", self.repository, *arguments], capture_output=True, text=True, timeout=300)

    def main(self):
        listed = self.git("ls-remote", "origin", "refs/heads/main").stdout.split()
        return listed[0] if listed else None

    def push(self, tree):
        """(landed, refused reason or None, other failure or None)."""
        fetched = self.git("fetch", "-q", "--no-tags", "origin", tree)
        if fetched.returncode != 0:
            return False, None, "fetching %s: %s" % (tree[:12], fetched.stderr.strip()[-300:])
        pushed = self.git("push", "origin", "%s:refs/heads/main" % tree)
        if pushed.returncode == 0:
            return True, None, None
        if "non-fast-forward" in pushed.stderr or "fetch first" in pushed.stderr or "rejected" in pushed.stderr:
            return False, "not a fast-forward of main: %s" % pushed.stderr.strip()[-300:], None
        return False, None, "pushing %s: %s" % (tree[:12], pushed.stderr.strip()[-300:])


def tick(pipeline, hands):
    status, orders = pipeline.call("GET", "/landings")
    if status != 200:
        log("landings: %d %s" % (status, orders))
        return
    for order in orders["landings"]:
        change, tree = order["change"], order["future"]
        old = hands.main()
        if old is None:
            log("can't read main; holding %s" % change)
            return
        landed, refused, failure = hands.push(tree)
        if landed:
            status, answer = pipeline.call("POST", "/landings/" + change, {"main": tree, "from": old, "landed": tree})
            log("landed %s: main %s..%s, %d %s" % (change, old[:12], tree[:12], status, answer))
        elif refused is not None:
            status, answer = pipeline.call("POST", "/landings/" + change, {"refused": refused, "main": old})
            log("refused %s on main %s: %s; %d %s" % (change, old[:12], refused[:200], status, answer))
        else:
            log("holding %s: %s" % (change, failure))


def main():
    os.makedirs(state, exist_ok=True)
    lock = open(os.path.join(state, "lock"), "w")
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        return
    pipeline = queue_bridge.Pipeline(queue_bridge.pipeline, queue_bridge.token())
    # Blocks first (a no-op while their switch is off), then the landing orders.
    blocks.tick(pipeline, blocks.Chain(lander), log)
    tick(pipeline, Hands(lander))


if __name__ == "__main__":
    sys.exit(main())
