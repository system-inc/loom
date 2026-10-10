#!/usr/bin/env python3
"""The queue's bridge to today's gate (#jtdwm5n) and the lander's hands (#bx6ak2c), until the planner, judge and builder
take over. Each tick, from Kirk's Mac beside the gate lane:

0. Every submitted change still unchecked gets git's facts from this clone (the sha exists, its base is its ancestor
   and on main, the paths of base..sha), since no GitHub credential lives in Cloudflare. The queue decides on them.
1. Every unplanned future loom-pipeline lists (slice 1: one change, tree = its sha) goes through today's fast gate
   exactly as a cut does: a cloud/land-queue-<tree8> branch at the tree, which fast-gate-watch gates like any
   cloud/land-* tip. Its newest finished record (gate-logs/<tree12>/<stamp>/fast) is the verdict input: green is
   passed, void is void, and red follows the contract's judge stub, "any second failure is the change": a first red
   is posted void and served again, and only a red after that is failed with cause change. Main moves often,
   so the gate usually tests the change merged onto a newer main (a gate merge, second parent the change's sha): that
   merge becomes the change's future, posted with its first parent as gateMerge.base, and it is what lands. A record
   of any other tree is void for this future. A void is served once more (requeue.sh, the old path's one requeue),
   since fast-gate-watch never gates a sha twice on its own.
2. Every landing order goes through Kirk's push script, push-main.sh --fast-gate on the order's record. A landing is
   reported with the new main, the main it moved from, and the tree it landed, once git shows that tree on main. A
   hold (exit 3) or main's pause waits; any other refusal is reported, which parks the change.

The queue decides; this only carries. No credential that moves main lives in Cloudflare.

usage: queuebridge/queue_bridge.py    (launchd com.loom.queue-bridge runs it every minute)
"""
import base64, fcntl, hashlib, hmac, json, os, re, subprocess, sys, time, urllib.error, urllib.request

pipeline = os.environ.get("QUEUE_BRIDGE_URL", "https://loom-pipeline.kirk-ouimet.workers.dev")
state = os.environ.get("QUEUE_BRIDGE_STATE", os.path.expanduser("~/.loom/queue-bridge"))
repository = os.environ.get("QUEUE_BRIDGE_REPOSITORY", os.path.expanduser("~/Projects/system/adamic"))
pushMain = os.environ.get("QUEUE_BRIDGE_PUSH_MAIN", os.path.expanduser("~/.adamic-merge-tree/cloud/integration/push-main.sh"))
secretPath = os.environ.get("QUEUE_BRIDGE_SECRET", os.path.expanduser("~/.loom/token-secret"))
requeueScript = os.environ.get("QUEUE_BRIDGE_REQUEUE", os.path.expanduser("~/.loom/bin/requeue.sh"))
github = "system-inc/adamic"
rule = "todays-gate-v0"


def log(text):
    print("%s queue-bridge: %s" % (time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), text), flush=True)


def token():
    """A coordinator token, minted the way the wire verifies it (wire/source/Token.ts), good for ten minutes."""
    secret = open(secretPath).read().strip().encode()
    payload = base64.urlsafe_b64encode(json.dumps({"run": "queue-bridge", "scope": "coordinator", "expires": int(time.time()) + 600},
                                                  separators=(",", ":")).encode()).rstrip(b"=")
    return (payload + b"." + base64.urlsafe_b64encode(hmac.new(secret, payload, hashlib.sha256).digest()).rstrip(b"=")).decode()


class Pipeline:
    def __init__(self, url, bearer):
        self.url, self.bearer = url, bearer

    def call(self, method, path, body=None):
        data = None if body is None else json.dumps(body).encode()
        request = urllib.request.Request(self.url + path, data=data, method=method,
                                         headers={"Authorization": "Bearer " + self.bearer, "Content-Type": "application/json", "User-Agent": "loom-queue-bridge"})
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                return response.status, json.loads(response.read() or b"null")
        except urllib.error.HTTPError as error:
            text = error.read()
            try:
                return error.code, json.loads(text)
            except ValueError:
                return error.code, {"error": text.decode(errors="replace")[:300]}


def git(*arguments):
    try:
        return subprocess.run(["git", "-C", repository, *arguments], capture_output=True, text=True, timeout=300).stdout.strip()
    except subprocess.TimeoutExpired:
        log("git %s timed out" % " ".join(arguments[:2]))
        return ""


class Gate:
    """Today's gate, as the bridge sees it: git's facts, records on origin, the branch that queues a tree, push-main."""

    def facts(self, sha, base):
        """What git says about a submitted change, read from origin through this clone."""
        git("fetch", "-q", "--no-tags", "origin", "main", sha)
        exists = subprocess.run(["git", "-C", repository, "cat-file", "-e", sha + "^{commit}"], capture_output=True).returncode == 0
        if not exists:
            return {"shaExists": False, "baseIsAncestor": False, "baseOnMain": False, "diffPaths": []}
        ancestor = lambda older, newer: subprocess.run(["git", "-C", repository, "merge-base", "--is-ancestor", older, newer], capture_output=True).returncode == 0
        return {"shaExists": True, "baseIsAncestor": ancestor(base, sha), "baseOnMain": ancestor(base, "origin/main"),
                "diffPaths": sorted(path for path in git("diff", "--no-renames", "--name-only", base, sha).splitlines() if path)}

    def record(self, tree):
        """The newest finished fast record for tree: {ref, status, gated}, or None while none has finished."""
        refs = [line.split("\t")[1].removeprefix("refs/heads/") for line in
                git("ls-remote", "origin", "refs/heads/gate-logs/%s/*" % tree[:12]).splitlines() if "\t" in line]
        refs = sorted((ref for ref in refs if ref.endswith("/fast")), key=lambda ref: ref.split("/")[2], reverse=True)
        if refs:
            git("fetch", "-q", "--no-tags", "origin", *["+refs/heads/%s:refs/remotes/origin/%s" % (ref, ref) for ref in refs])
        for ref in refs:
            status = git("show", "origin/%s:status.txt" % ref).split("\n")[0].split(":")[0].strip()
            if status not in ("green", "red", "void"):
                continue
            try:
                fast = json.loads(git("show", "origin/%s:fast.json" % ref) or "{}")
            except ValueError:
                fast = {}
            return {"ref": ref, "status": status, "gated": fast.get("gated") or fast.get("sha") or ""}
        return None

    def queue(self, tree):
        """Puts tree in front of fast-gate-watch as a cloud/land-* tip. True when the branch is there."""
        branch = "cloud/land-queue-%s" % tree[:8]
        ran = subprocess.run(["gh", "api", "-X", "POST", "repos/%s/git/refs" % github, "-f", "ref=refs/heads/" + branch, "-f", "sha=" + tree],
                             capture_output=True, text=True, timeout=60)
        return ran.returncode == 0 or "Reference already exists" in ran.stdout + ran.stderr

    def requeue(self, sha):
        """Serves sha's fast job again (requeue.sh). True when it started."""
        ran = subprocess.run(["bash", requeueScript, sha], capture_output=True, text=True, timeout=120)
        return ran.returncode == 0

    def land(self, record, tree, label):
        """push-main.sh --fast-gate on the record: (exit code, stdout, stderr)."""
        ran = subprocess.run(["bash", pushMain, "--fast-gate", record, tree, label], capture_output=True, text=True,
                             cwd=os.path.dirname(os.path.dirname(os.path.dirname(pushMain))))
        return ran.returncode, ran.stdout, ran.stderr

    def parents(self, sha):
        """sha's parents, from git (a gate merge lives under refs/gate-merges, so fetch it by sha): [] when origin lacks it."""
        listed = git("rev-list", "--parents", "-n", "1", sha).split()
        if not listed:
            git("fetch", "-q", "--no-tags", "origin", sha)
            listed = git("rev-list", "--parents", "-n", "1", sha).split()
        return listed[1:]

    def main(self):
        git("fetch", "-q", "origin", "main")
        return git("rev-parse", "origin/main")

    def contains(self, main, tree):
        return subprocess.run(["git", "-C", repository, "merge-base", "--is-ancestor", tree, main]).returncode == 0


def verdictOf(record, tree, gate, served=False):
    """The body today's record gives the change at tree: its whole verdict, and gateMerge when it gated a merge of tree.
    served says the tree was already served again once, so a red now is the change's."""
    gated, merge = record["gated"], None
    if gated != tree:
        parents = gate.parents(gated)
        if len(parents) != 2 or parents[1] != tree:
            return {"verdict": {"future": tree, "run": record["ref"], "status": "void", "cause": "infra", "rule": rule}}
        merge = {"base": parents[0]}
    status, cause = {"green": ("passed", None), "red": ("failed", "change") if served else ("void", "flake"), "void": ("void", "infra")}[record["status"]]
    body = {"verdict": {"future": gated, "run": record["ref"], "status": status, "cause": cause, "rule": rule}}
    if merge is not None:
        body["gateMerge"] = merge
    return body


def tick(pipeline, gate, memory):
    """One pass: git's facts, verdicts for unplanned futures, then every landing order. memory holds what was done or said."""
    status, unchecked = pipeline.call("GET", "/submissions?state=unchecked")
    if status != 200:
        log("submissions: %d %s" % (status, unchecked))
        return
    for submitted in unchecked["changes"]:
        facts = gate.facts(submitted["sha"], submitted["base"])
        status, answer = pipeline.call("POST", "/submissions/%s/facts" % submitted["change"], facts)
        log("facts for %s: %d %s" % (submitted["change"], status, answer))
    status, listed = pipeline.call("GET", "/futures?state=unplanned")
    if status != 200:
        log("futures: %d %s" % (status, listed))
        return
    for future in listed["futures"]:
        tree, change = future["tree"], future["changes"][0]
        # A parity run is Release's proof of the new path against a box record: today's gate never decides it.
        if future.get("parity"):
            continue
        record = gate.record(tree)
        if record is None:
            if tree not in memory["queued"] and gate.queue(tree):
                memory["queued"].append(tree)
                log("queued %s (%s) for today's fast gate" % (tree[:12], change))
            continue
        key = "%s %s" % (change, record["ref"])
        if key in memory["posted"]:
            continue
        body = dict(verdictOf(record, tree, gate, served=tree in memory["requeued"]), change=change)
        status, answer = pipeline.call("POST", "/verdicts", body)
        log("verdict %s %s on %s: %d %s" % (change, body["verdict"]["status"], record["ref"], status, answer))
        if status in (200, 409):
            memory["posted"].append(key)
        if body["verdict"]["status"] == "void" and status == 200 and tree not in memory["requeued"]:
            memory["requeued"].append(tree)
            log("requeued %s after its void: %s" % (tree[:12], "started" if gate.requeue(tree) else "requeue.sh refused"))
    status, orders = pipeline.call("GET", "/landings")
    if status != 200:
        log("landings: %d %s" % (status, orders))
        return
    for order in orders["landings"]:
        change, tree = order["change"], order["future"]
        code, out, err = gate.land(order["run"], tree, "queue %s (%s)" % (change, order["owner"]))
        pushed = re.search(r"Pushed main ([0-9a-f]{40})\.\.([0-9a-f]{40})", out)
        if code == 0 and pushed and gate.contains(pushed.group(2), tree):
            status, answer = pipeline.call("POST", "/landings/" + change, {"main": pushed.group(2), "from": pushed.group(1), "landed": tree})
            log("landed %s: main %s..%s, %d %s" % (change, pushed.group(1)[:12], pushed.group(2)[:12], status, answer))
            continue
        if code == 0:
            log("push-main exited 0 for %s without a main that holds %s; holding: %s" % (change, tree[:12], out.strip()[-300:]))
            continue
        reason = ([line for line in err.splitlines() if line.startswith("refused")] or err.splitlines()[-1:] or ["exit %d" % code])[0]
        if code == 3 or "landings are paused" in reason:
            if "%s %s" % (change, reason) not in memory["held"]:
                memory["held"].append("%s %s" % (change, reason))
                log("holding %s: %s" % (change, reason[:300]))
            continue
        status, answer = pipeline.call("POST", "/landings/" + change, {"refused": reason[:600], "main": gate.main()})
        log("refused %s: %s; %d %s" % (change, reason[:300], status, answer))


def main():
    os.makedirs(state, exist_ok=True)
    lock = open(os.path.join(state, "lock"), "w")
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        return
    path = os.path.join(state, "memory.json")
    memory = json.load(open(path)) if os.path.exists(path) else {}
    for name in ("queued", "posted", "held", "requeued"):
        memory.setdefault(name, [])
    try:
        tick(Pipeline(pipeline, token()), Gate(), memory)
    finally:
        open(path + ".new", "w").write(json.dumps(memory, indent=1))
        os.replace(path + ".new", path)


if __name__ == "__main__":
    sys.exit(main())
