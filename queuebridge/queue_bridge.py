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

Docs and test-only changes take the smallest gate that can fail for them (Loom, Oct 10 00:18Z), never the product
suite: a change that touches only Markdown is posted passed under the docs ruling and lands through push-main
--ruled-gate (the census and cheap static checks on the merged tree), and one that touches only test paths (push-main's
own testOnlyPattern) is posted passed under the test-only ruling and lands through push-main --test-only, whose lane
checks (gofmt, t.Parallel, vet) refuse anything that isn't.

Once Judge decides every future (slice 2), QUEUE_BRIDGE_DECIDES=0 turns step 1 and both lanes off, and the bridge only
carries git's facts.

The queue decides; this only carries. No credential that moves main lives in Cloudflare.

usage: queuebridge/queue_bridge.py    (launchd com.loom.queue-bridge runs it every minute)
"""
import base64, fcntl, gzip, hashlib, hmac, json, os, re, subprocess, sys, time, urllib.error, urllib.request

pipeline = os.environ.get("QUEUE_BRIDGE_URL", "https://loom-pipeline.kirk-ouimet.workers.dev")
state = os.environ.get("QUEUE_BRIDGE_STATE", os.path.expanduser("~/.loom/queue-bridge"))
repository = os.environ.get("QUEUE_BRIDGE_REPOSITORY", os.path.expanduser("~/Projects/system/adamic"))
pushMain = os.environ.get("QUEUE_BRIDGE_PUSH_MAIN", os.path.expanduser("~/.adamic-merge-tree/cloud/integration/push-main.sh"))
secretPath = os.environ.get("QUEUE_BRIDGE_SECRET", os.path.expanduser("~/.loom/token-secret"))
requeueScript = os.environ.get("QUEUE_BRIDGE_REQUEUE", os.path.expanduser("~/.loom/bin/requeue.sh"))
github = "system-inc/adamic"
rule = "todays-gate-v0"
docsRule = "ruled-gate-docs-v0"
# Whether this Mac still runs push-main on landing orders. Off once workshop's pusher holds main (QUEUE_BRIDGE_LANDS=1
# turns it back on).
landsHere = os.environ.get("QUEUE_BRIDGE_LANDS", "0") == "1"
docsRuling = "docs only: Markdown no product test reads (Loom, Oct 10 00:18Z)"
# Whether today's gate still decides futures here. Off (QUEUE_BRIDGE_DECIDES=0) once Judge decides every future and
# outside verdicts are refused (#xvvf6cn); git's facts keep posting either way.
decidesHere = os.environ.get("QUEUE_BRIDGE_DECIDES", "1") == "1"


# Main's own reds the judge has ruled, each by package and test pattern with its ruling. A red record whose every failing
# test is one of these is main's red, not the change's; push-main --infra-red checks each by name on the output again
# (a test-owned time limit, no assertion), so a ruled name can't hide a real failure of the same test.
mainReds = [
    ("stage1/cohere/gitignore", re.compile(r"^TestThePortAnswersAsGoCohereAndGitDo_\d+$"),
     "main's red: its run time at its own 90 s hard deadline, a deadline at its edge (Judge, Oct 10 00:33Z)"),
]


def excusedNames(failing):
    """The --infra-red names for failing ("<package> <Test>") when every one is a ruled main red, else None."""
    names = []
    for name in failing:
        package, test = name.split(" ", 1)
        ruling = [ruling for ruledPackage, pattern, ruling in mainReds if ruledPackage == package and pattern.match(test)]
        if not ruling:
            return None
        names.append("%s %s=%s" % (package, test, ruling[0]))
    return names or None


def failingTests(lines, fast):
    """The failing top-level tests in a record's test events, or None when a failure could hide outside them (Loom's
    guard, 00:34Z): a fail with no test (a build failure, a binary dying outside any test), a test that started and never
    ended, a stage other than tests that didn't pass, or no test record at all. None is never excusable."""
    if lines is None:
        return None
    stages = fast.get("stages_exit") or {}
    if any(code != 0 for stage, code in stages.items() if stage != "tests"):
        return None
    names, started, ended = set(), set(), set()
    for line in lines:
        try:
            event = json.loads(line)
        except ValueError:
            continue
        action, test, package = event.get("Action"), event.get("Test"), event.get("Package", "")
        if action == "fail" and not test:
            return None
        if test and action == "run":
            started.add((package, test))
        if test and action in ("pass", "fail", "skip"):
            ended.add((package, test))
        if action == "fail" and test:
            names.add(package.split("/adamic/")[-1] + " " + test.split("/")[0])
    if started - ended:
        return None
    return sorted(names)


def docsOnly(paths):
    """Whether a change touches only Markdown, which the ruled gate lands with its census and no product suite."""
    return bool(paths) and all(path.endswith(".md") for path in paths)


# push-main.sh's testOnlyPattern, the same list by ruling (Kirk, Oct 8); push-main checks it again on the merged tree.
testOnlyPattern = re.compile(r"(_test\.go$|_test\.py$|-test\.py$|(^|/)test_[^/]*\.py$|/testdata/|^review/|(^|/)shards\.json$|^stage3/fixtures/|^stage3/meter/|^README\.md$)")
testOnlyRule = "test-only-lane-v0"


def testOnly(paths):
    """Whether a change touches only test paths, which the test-only lane lands with no gate in front of it."""
    return bool(paths) and all(testOnlyPattern.search(path) for path in paths)


# push-main's Python test names: a test a non-test file names is gate logic (l.787-795).
pythonTestPattern = re.compile(r"(_test\.py$|-test\.py$|(^|/)test_[^/]*\.py$)")


def historyOf(base, sha):
    """Every path a non-merge commit in base..sha touches: the queue refuses a change whose history reaches past its diff."""
    return sorted(set(path for path in git("log", "--no-merges", "--format=", "--name-only", "%s..%s" % (base, sha)).splitlines() if path))


def gateNamedOf(sha, diffPaths):
    """Each Python test in the diff that a file other than a test, a .md or a .txt names in sha's tree, with those files."""
    named = []
    for path in diffPaths:
        if not pythonTestPattern.search(path):
            continue
        found = [line.split(":", 1)[1] for line in git("grep", "-l", "-F", "-e", path.rsplit("/", 1)[-1], sha, "--", ".").splitlines() if ":" in line]
        users = sorted(user for user in found if user != path and not testOnlyPattern.search(user) and not user.endswith((".md", ".txt")))
        if users:
            named.append({"path": path, "users": users})
    return named


def patchId(older, newer):
    diff = subprocess.run(["git", "-C", repository, "diff", older, newer], capture_output=True).stdout
    if not diff:
        return None
    out = subprocess.run(["git", "-C", repository, "patch-id", "--stable"], input=diff, capture_output=True).stdout.split()
    return out[0].decode() if out else None


def revertOf(base, sha, depth=30):
    """The commit among main's newest first-parent commits whose inverse is exactly base..sha, or None."""
    change = patchId(base, sha)
    if change is None:
        return None
    for commit in git("rev-list", "--first-parent", "-n", str(depth), "origin/main").splitlines():
        parent = git("rev-parse", "--verify", "-q", commit + "^1")
        if parent and patchId(commit, parent) == change:
            return commit
    return None


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
        diffPaths = sorted(path for path in git("diff", "--no-renames", "--name-only", base, sha).splitlines() if path)
        return {"shaExists": True, "baseIsAncestor": ancestor(base, sha), "baseOnMain": ancestor(base, "origin/main"),
                "diffPaths": diffPaths, "historyPaths": historyOf(base, sha), "gateNamed": gateNamedOf(sha, diffPaths),
                "revertOf": revertOf(base, sha)}

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

    def failing(self, ref):
        """The failing top-level tests of a record, "<package under the module> <Test>", from its test.jsonl.gz and
        fast.json; None when the record can't be read that way (see failingTests)."""
        raw = subprocess.run(["git", "-C", repository, "show", "origin/%s:test.jsonl.gz" % ref], capture_output=True).stdout
        try:
            lines = gzip.decompress(raw).decode(errors="replace").splitlines() if raw else None
            fast = json.loads(git("show", "origin/%s:fast.json" % ref) or "{}")
        except (OSError, ValueError):
            return None
        return failingTests(lines, fast)

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
        """push-main.sh --fast-gate on the record, naming any ruled main red it carries: (exit code, stdout, stderr)."""
        infra = []
        for name in excusedNames(self.failing(record) or []) or []:
            infra += ["--infra-red", name]
        ran = subprocess.run(["bash", pushMain, "--fast-gate", record, *infra, tree, label], capture_output=True, text=True,
                             cwd=os.path.dirname(os.path.dirname(os.path.dirname(pushMain))))
        return ran.returncode, ran.stdout, ran.stderr

    def parents(self, sha):
        """sha's parents, from git (a gate merge lives under refs/gate-merges, so fetch it by sha): [] when origin lacks it."""
        listed = git("rev-list", "--parents", "-n", "1", sha).split()
        if not listed:
            git("fetch", "-q", "--no-tags", "origin", sha)
            listed = git("rev-list", "--parents", "-n", "1", sha).split()
        return listed[1:]

    def check(self, arguments, label):
        """push-main.sh in check-only mode: every check it runs, the landing commit built, nothing pushed."""
        ran = subprocess.run(["bash", pushMain, *arguments, label], capture_output=True, text=True,
                             cwd=os.path.dirname(os.path.dirname(os.path.dirname(pushMain))), env=dict(os.environ, PUSH_MAIN_CHECK_ONLY="1"))
        return ran.returncode, ran.stdout, ran.stderr

    def landRuled(self, ruling, tree, label):
        """push-main.sh --ruled-gate: the census and static checks on the merged tree, no product suite."""
        ran = subprocess.run(["bash", pushMain, "--ruled-gate", ruling, tree, "0", "0", "0", "0", label], capture_output=True, text=True,
                             cwd=os.path.dirname(os.path.dirname(os.path.dirname(pushMain))))
        return ran.returncode, ran.stdout, ran.stderr

    def landTestOnly(self, tree, label):
        """push-main.sh --test-only: the test-only lane's checks, no gate."""
        ran = subprocess.run(["bash", pushMain, "--test-only", tree, label], capture_output=True, text=True,
                             cwd=os.path.dirname(os.path.dirname(os.path.dirname(pushMain))))
        return ran.returncode, ran.stdout, ran.stderr

    def main(self):
        git("fetch", "-q", "origin", "main")
        return git("rev-parse", "origin/main")

    def contains(self, main, tree):
        return subprocess.run(["git", "-C", repository, "merge-base", "--is-ancestor", tree, main]).returncode == 0

    def landedAt(self, tree):
        """The first-parent commit on origin/main that brought tree in: (that commit, the main it moved from), or None.
        Git is the record of a landing; push-main's own words (8-character shas) are only a hint."""
        main = self.main()
        if not self.contains(main, tree):
            return None
        for line in git("log", "--first-parent", "--format=%H %P", "-n", "200", main).splitlines():
            fields = line.split()
            if len(fields) >= 2 and not self.contains(fields[1], tree):
                return fields[0], fields[1]
        return None


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
    if record["status"] == "red" and excusedNames(gate.failing(record["ref"]) or []) is not None:
        # Every failure is a red the judge ruled main's: excused, as the judge's Green excuses it.
        status, cause = "failed", "mainRed"
    body = {"verdict": {"future": gated, "run": record["ref"], "status": status, "cause": cause, "rule": rule}}
    if merge is not None:
        body["gateMerge"] = merge
    return body


def checked(gate, arguments, change):
    """push-main's whole judgment on a candidate, nothing pushed: (exit code, its reason). 3 is a hold."""
    code, out, err = gate.check(arguments, "queue %s" % change)
    reason = ([line for line in err.splitlines() if line.startswith("refused")] or err.splitlines()[-1:] or out.splitlines()[-1:] or ["exit %d" % code])[0]
    log("push-main checked %s (%s): exit %d %s" % (change, " ".join(arguments[:2]), code, reason[:200] if code else ""))
    return code, reason


def decide(pipeline, gate, memory):
    """Today's gate's verdict for every unplanned future: the docs and test-only lanes by push-main's checks, the rest by
    a fast record. Off (QUEUE_BRIDGE_DECIDES=0) once Judge decides every future."""
    status, listed = pipeline.call("GET", "/futures?state=unplanned")
    if status != 200:
        log("futures: %d %s" % (status, listed))
        return
    for future in listed["futures"]:
        tree, change = future["tree"], future["changes"][0]
        # A parity run is Release's proof of the new path against a box record: today's gate never decides it.
        if future.get("parity"):
            continue
        status, read = pipeline.call("GET", "/changes/" + change)
        lane = None
        if status == 200 and docsOnly(read["record"]["paths"]):
            lane = ("ruled-gate:docs", docsRule)
        elif status == 200 and testOnly(read["record"]["paths"]):
            lane = ("test-only", testOnlyRule)
        if lane is not None:
            # Keyed by the tree too: a resubmitted change is a new tree, decided again.
            if "%s %s" % (change, tree) in memory["ruled"]:
                continue
            verdict = {"future": tree, "run": lane[0], "status": "passed", "cause": None, "rule": lane[1]}
            # The pusher only fast-forwards, so push-main's own checks for the lane (the ruled gate's census on the
            # landing tree, or the test-only lane's checks) run here, before anything reads green (Loom, 00:54Z).
            arguments = ["--ruled-gate", docsRuling, tree, "0", "0", "0", "0"] if lane[0] == "ruled-gate:docs" else ["--test-only", tree]
            code, why = checked(gate, arguments, change)
            if code == 3:
                continue
            # Only push-main's own refusal is the change's. Anything else (a git lock race in the shared checkout, a
            # fetch that failed) is the check not running, so it runs again next tick, never a red.
            if code != 0 and not why.startswith("refused"):
                log("push-main couldn't check %s (exit %d), trying again: %s" % (change, code, why[:300]))
                continue
            if code != 0:
                verdict.update(status="failed", cause="change", run="%s refused by push-main's checks: %s" % (lane[0], why[:300]))
            status, answer = pipeline.call("POST", "/verdicts", {"change": change, "verdict": verdict})
            log("verdict %s %s under %s: %d %s" % (change, verdict["status"], lane[1], status, answer))
            if status in (200, 409):
                memory["ruled"].append("%s %s" % (change, tree))
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
        verdict = body["verdict"]
        # Nothing reads green, or main's red, on today's record alone: push-main's checks on it (zerorun on the
        # record's job, --infra-red's recheck of each ruled red on its output, the pause rule) run first. A refusal is
        # a void with push-main's reason in its rule, never a pass.
        if verdict["status"] == "passed" or verdict["cause"] == "mainRed":
            infra = []
            for name in excusedNames(gate.failing(record["ref"]) or []) or []:
                infra += ["--infra-red", name]
            code, why = checked(gate, ["--fast-gate", record["ref"], *infra, verdict["future"]], change)
            if code == 3:
                continue
            if code != 0:
                verdict.update(status="void", cause="infra", rule="%s; push-main refused: %s" % (rule, why[:300]))
        status, answer = pipeline.call("POST", "/verdicts", body)
        log("verdict %s %s on %s: %d %s" % (change, body["verdict"]["status"], record["ref"], status, answer))
        if status in (200, 409):
            memory["posted"].append(key)
        if body["verdict"]["status"] == "void" and status == 200 and tree not in memory["requeued"]:
            memory["requeued"].append(tree)
            log("requeued %s after its void: %s" % (tree[:12], "started" if gate.requeue(tree) else "requeue.sh refused"))


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
    # Once outside verdicts are refused, Judge decides every future and this only carries git's facts (#hkmzefm).
    if decidesHere:
        decide(pipeline, gate, memory)
    # Landing is the pusher's on workshop, with the lander key (#83m6zw8); this Mac lands only while it's asked to.
    if not landsHere:
        return
    status, orders = pipeline.call("GET", "/landings")
    if status != 200:
        log("landings: %d %s" % (status, orders))
        return
    for order in orders["landings"]:
        change, tree = order["change"], order["future"]
        if order["run"] == "ruled-gate:docs":
            code, out, err = gate.landRuled(docsRuling, tree, "queue %s (%s)" % (change, order["owner"]))
        elif order["run"] == "test-only":
            code, out, err = gate.landTestOnly(tree, "queue %s (%s)" % (change, order["owner"]))
        else:
            code, out, err = gate.land(order["run"], tree, "queue %s (%s)" % (change, order["owner"]))
        reason = ([line for line in err.splitlines() if line.startswith("refused")] or err.splitlines()[-1:] or ["exit %d" % code])[0]
        # Whatever push-main said, git says whether the tree is on main now (a retry after a landing it printed in a
        # form this missed answers "already holds").
        landed = gate.landedAt(tree) if code == 0 or "already holds" in reason else None
        if landed is not None:
            status, answer = pipeline.call("POST", "/landings/" + change, {"main": landed[0], "from": landed[1], "landed": tree})
            log("landed %s: main %s..%s, %d %s" % (change, landed[1][:12], landed[0][:12], status, answer))
            continue
        if code == 0:
            log("push-main exited 0 for %s without a main that holds %s; holding: %s" % (change, tree[:12], out.strip()[-300:]))
            continue
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
    for name in ("queued", "posted", "held", "requeued", "ruled"):
        memory.setdefault(name, [])
    try:
        tick(Pipeline(pipeline, token()), Gate(), memory)
    finally:
        open(path + ".new", "w").write(json.dumps(memory, indent=1))
        os.replace(path + ".new", path)


if __name__ == "__main__":
    sys.exit(main())
