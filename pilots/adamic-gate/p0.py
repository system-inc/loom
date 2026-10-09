#!/usr/bin/env python3
"""p0.py: a leaf killed at 90 s on Loom is a red and P0 (Kirk, Oct 9 03:17Z), filed by Loom itself (#2en3b4t, step 81's
(g), #kxnza1j).

	pilots/adamic-gate/p0.py <reds file> <go test lines> --run <name> --sha <sha> [--dry]   # a copy in ~/.loom/bin

From the reds file's "KILLED <unit> <package> <test>: over budget, P0" lines (the unit's kill trap names each leaf
still running), each killed leaf goes to its owner: an open grain task under #5g5151k that names its top-level test
gets a comment; else Loom files one, Now, owned by the package's owner (burndown.py's map). The tasks Loom filed are
kept in ~/.loom/p0.json; from the run's go test lines, one whose leaf passes under 60 s on two runs in a row is closed.

A complete run (no unit broken, so every leaf's time is in) also files every leaf over 60 s the same way, with its
seconds, killed or not, so the grain task list and the burn-down's leaves line count the same leaves (@system_adamic,
Oct 9: the list read 61 of 63 done while cd9930fa had 37 leaves over 60 s). A partial run files only its kills.
"""

import argparse
import datetime
import json
import os
import re
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import burndown  # noqa: E402

ahra = "/Users/kirkouimet/Projects/ahra/node_modules/.bin/ahra"
state = os.path.expanduser("~/.loom/p0.json")
noted = os.path.expanduser("~/.loom/p0-noted.json")  # an owner's open task Loom noted a run on, by "<task> <run>"
members = {"cohere": "system_cohere_adamic", "runtime": "system_adamic_runtime", "library": "system_adamic_library",
           "compiler": "system_adamic_compiler", "typescript": "system_adamic_typescript", "developer tools": "system_adamic_developer_tools"}


def tasks(*arguments, dry=False):
    if dry:
        print("dry: ahra tasks " + " ".join(arguments))
        return ""
    return subprocess.run([ahra, "tasks", *arguments], cwd="/Users/kirkouimet/Projects/ahra", capture_output=True, text=True).stdout


def leafSeconds(path):
    """Each leaf's seconds and outcome in the run, by "<package> <test>"."""
    started, done = {}, {}
    for line in open(path, errors="replace"):
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if not event.get("Test") or "Time" not in event:
            continue
        key = event["Package"] + " " + event["Test"]
        moment = datetime.datetime.fromisoformat(event["Time"][:19])
        if event.get("Action") in ("run", "cont"):
            started[key] = moment
        elif event.get("Action") in ("pass", "fail") and key in started:
            done[key] = ((moment - started[key]).total_seconds(), event["Action"])
    return done


# A family is a top-level test with its numbered shards, setup and planted siblings (X_000, X_Setup, XPlantedFailure,
# XUnion), the way owners split one test: one cause on one family is one task listing its shards, never one task per
# shard (@system_adamic, Oct 9: 52 of lint TestCompilerAndStage1Agree's shards filed as 52 tasks for one cause).
familyStem = re.compile(r"(_[0-9]+|_Setup|PlantedFailure|Union)$")


def familyOf(package, test):
    return package, familyStem.sub("", test.split("/")[0])


def git(*arguments):
    return subprocess.run(["git", "-C", gate, *arguments], capture_output=True, text=True)


gate = os.path.expanduser("~/Projects/system/adamic-gate")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("reds")
    parser.add_argument("tests")
    parser.add_argument("--run", required=True)
    parser.add_argument("--sha", required=True, help="the commit the run tested: only main's, and only where its package hasn't moved since, are filed")
    parser.add_argument("--dry", action="store_true")
    parser.add_argument("--over", type=float, default=60.0, help="in a complete run, every leaf over this many seconds is filed")
    arguments = parser.parse_args()
    filed = json.load(open(state)) if os.path.exists(state) else {}
    killed, complete = [], False
    for line in open(arguments.reds):
        match = re.match(r"^KILLED (\S+) (\S+) (\S+): over budget, P0$", line.strip())
        if match:
            killed.append((match.group(1), match.group(2), match.group(3), None))
        if re.match(r"^run \S+: (green|red), .* 0 units broken", line):
            complete = True
    seconds = leafSeconds(arguments.tests) if os.path.exists(arguments.tests) else {}
    # Each leaf over the budget in a complete run, unless a kill names it already: (unit, package, test, seconds).
    slow = []
    if complete:
        named = {package + " " + test for _, package, test, _ in killed}
        # A leaf the way burndown.py reads one: no test of any action in the stream (a subtest that only ran, paused
        # or skipped included) is its child, so a parent waiting on its subtests isn't counted beside them.
        parents = set()
        for line in open(arguments.tests, errors="replace"):
            try:
                event = json.loads(line)
            except ValueError:
                continue
            # Every prefix that ends before a slash is a parent: subtest names may hold slashes of their own (the oracle's
            # TestNativeAgreesWithNode/internal/oracle/testdata/...), as burndown.py's startswith reads them.
            parts = (event.get("Test") or "").split("/")
            for end in range(1, len(parts)):
                parents.add(event["Package"] + " " + "/".join(parts[:end]))
        leaves = {key for key in seconds if key not in parents}
        for key in sorted(leaves):
            if seconds[key][0] > arguments.over and key not in named:
                package, test = key.split(" ", 1)
                slow.append(("", package, test, seconds[key][0]))
    # Only main's runs file: a candidate's kill is the candidate's, and its owner hears it in the red list. And a leaf
    # whose package moved on main since the run's sha is stale (@system_adamic, Oct 9: a run that predated lint's re-cut
    # #155 and json's setup fix #192 filed their old shards): it waits for a run that has the change.
    git("fetch", "-q", "origin", "main")
    if git("merge-base", "--is-ancestor", arguments.sha, "origin/main").returncode != 0:
        print("p0: %s isn't on main; nothing filed" % arguments.sha[:12])
        killed, slow = [], []
    moved = {}

    def stale(package):
        if package not in moved:
            directory = package[len(burndown.module):] if package.startswith(burndown.module) else package
            moved[package] = git("log", "-1", "--format=%h %s", arguments.sha + "..origin/main", "--", directory).stdout.strip()
        return moved[package]

    families = {}  # (package, stem, cause) -> [(unit, test, seconds)]
    skipped = {}
    for unit, package, test, over in killed + slow:
        if stale(package):
            skipped.setdefault(package, stale(package))
            continue
        cause = "slow" if over else "killed"
        families.setdefault(familyOf(package, test) + (cause,), []).append((unit, test, over))
    for package, change in sorted(skipped.items()):
        print("p0: stale, not filed: %s moved on main since %s (%s)" % (package, arguments.sha[:12], change))
    tree = tasks("tree", "5g5151k", "--depth", "4")  # a read, so it runs in a dry run too
    notes = set(json.load(open(noted))) if os.path.exists(noted) else set()
    for (package, stem, cause), members in sorted(families.items()):
        key = "%s %s %s" % (package, stem, cause)
        relative = package[len(burndown.module):] if package.startswith(burndown.module) else package
        listing = "\n".join("- %s%s" % (test, " (%.0f s)" % over if over else " (unit %s)" % unit) for unit, test, over in members[:60])
        if len(members) > 60:
            listing += "\n- and %d more" % (len(members) - 60)
        what = ("%d leaves over %.0f s, the longest %.0f s" % (len(members), arguments.over, max(over for _, _, over in members)) if cause == "slow"
                else "%d leaves running at a 90 s kill" % len(members))
        note = "Loom's run %s (%s): %s %s*: %s.\n%s" % (arguments.run, arguments.sha[:12], relative, stem, what, listing)
        if key in filed:
            if filed[key].get("last") == arguments.run:
                continue  # this run is on the task already
            tasks("comment", filed[key]["task"], "--role", "Agent", "--text", note, dry=arguments.dry)
            filed[key]["under"], filed[key]["last"] = 0, arguments.run
            continue
        # An open grain task that names the family (and its package) is where it goes.
        existing = [found for found in re.findall(r"[◑○] \[\w\] (#\w+)\s+(.*)", tree) if stem in found[1] and relative.split("/")[-1] in found[1]]
        if existing:
            mark = existing[0][0].lstrip("#") + " " + arguments.run
            if mark not in notes:
                tasks("comment", existing[0][0].lstrip("#"), "--role", "Agent", "--text", note, dry=arguments.dry)
                notes.add(mark)
            continue
        owner = members_of(package, stem)
        if cause == "slow":
            title = "Grain: %s* (%s), %s on Loom; under 60 s" % (stem, relative, what)
        else:
            title = "P0: %s* (%s), %s on Loom; fix or split under 60 s" % (stem, relative, what)
        body = (note + "\n\nOne family, one cause, one task: fix or split it until every leaf runs under 60 s on one 4-CPU Codex "
                "instance. A kill that lands before the tests start (C emission, a sanitized build) is the build phase's (#8gw478y), "
                "not the tests'. Loom closes this task itself after two runs in a row with every leaf of the family under 60 s.")
        # New tasks go under LOOM_P0_PARENT only. The block rule (@system_adamic, Oct 9 06:26Z) closes #5g5151k and #fvmyvy8
        # to new blockers, so with no parent named a family is reported and left unfiled; notes and closings go on.
        parent = os.environ.get("LOOM_P0_PARENT", "")
        if not parent:
            print("p0: not filed, no LOOM_P0_PARENT: %s (%s)" % (key, what))
            continue
        output = tasks("new", title[:200], "--parent", parent, "--priority", "Now", "--owner", owner, "--content", body, dry=arguments.dry)
        created = re.search(r"#(\w{6,7})", output or "")
        filed[key] = {"task": created.group(1) if created else "dry", "owner": owner, "filed": arguments.run, "under": 0, "last": arguments.run}
        print("filed %s for %s: %s (%d leaves)" % (filed[key]["task"], owner, key, len(members)))
    # Closing: a family whose every leaf measured in this run passed under 60 s, on two runs in a row, is done.
    for key, entry in list(filed.items()):
        if len(key.split(" ")) == 2:
            # A leaf filed one by one before families (Oct 9, still being worked): closed on its own seconds.
            if key in seconds and not any(key == package + " " + test for _, package, test, _ in killed + slow):
                measured = [seconds[key]]
            else:
                continue
        else:
            package, stem, cause = key.split(" ")
            if (package, stem, cause) in families:
                continue
            measured = [value for leaf, value in seconds.items() if familyOf(*leaf.split(" ", 1)) == (package, stem)]
        if not measured:
            continue
        # A run counts once: p0.py run again over the same record must not make one run two (Oct 9: a re-run over main
        # c869cea9's record closed 26 tasks on a single run under 60 s).
        if entry.get("last") == arguments.run:
            continue
        entry["last"] = arguments.run
        entry["under"] = entry.get("under", 0) + 1 if all(outcome == "pass" and value < 60 for value, outcome in measured) else 0
        if entry["under"] >= 2:
            tasks("done", entry["task"], "Every leaf under 60 s on two runs in a row on Loom (last %s)." % arguments.run, dry=arguments.dry)
            print("closed %s: %s" % (entry["task"], key))
            del filed[key]
    if not arguments.dry:
        json.dump(filed, open(state + ".partial", "w"), indent=1)
        os.replace(state + ".partial", state)
        json.dump(sorted(notes), open(noted + ".partial", "w"), indent=1)
        os.replace(noted + ".partial", noted)
    return 0


def members_of(package, stem):
    return members.get(burndown.ownerOf(package, stem), "system_adamic")


if __name__ == "__main__":
    sys.exit(main())
