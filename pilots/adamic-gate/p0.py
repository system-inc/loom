#!/usr/bin/env python3
"""p0.py: a leaf killed at 90 s on Loom is a red and P0 (Kirk, Oct 9 03:17Z), filed by Loom itself (#2en3b4t, step 81's
(g), #kxnza1j).

	pilots/adamic-gate/p0.py <reds file> <go test lines> --run <name> [--dry]   # a copy in ~/.loom/bin

From the reds file's "KILLED <unit> <package> <test>: over budget, P0" lines (the unit's kill trap names each leaf
still running), each killed leaf goes to its owner: an open grain task under #5g5151k that names its top-level test
gets a comment; else Loom files one, Now, owned by the package's owner (burndown.py's map). The tasks Loom filed are
kept in ~/.loom/p0.json; from the run's go test lines, one whose leaf passes under 60 s on two runs in a row is closed.
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


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("reds")
    parser.add_argument("tests")
    parser.add_argument("--run", required=True)
    parser.add_argument("--dry", action="store_true")
    arguments = parser.parse_args()
    filed = json.load(open(state)) if os.path.exists(state) else {}
    killed = []
    for line in open(arguments.reds):
        match = re.match(r"^KILLED (\S+) (\S+) (\S+): over budget, P0$", line.strip())
        if match:
            killed.append((match.group(1), match.group(2), match.group(3)))
    tree = tasks("tree", "5g5151k", "--depth", "4")  # a read, so it runs in a dry run too
    for unit, package, test in killed:
        key = package + " " + test
        top = test.split("/")[0]
        relative = package[len(burndown.module):] if package.startswith(burndown.module) else package
        if key in filed:
            tasks("comment", filed[key]["task"], "--role", "Agent", "--text", "Killed at 90 s again in %s (%s)." % (arguments.run, unit), dry=arguments.dry)
            filed[key]["under"] = 0
            continue
        # An open grain task that names the test (and its package) is where the kill goes.
        existing = [found for found in re.findall(r"[◑○] \[\w\] (#\w+)\s+(.*)", tree) if top in found[1] and relative.split("/")[-1] in found[1]]
        if existing:
            tasks("comment", existing[0][0].lstrip("#"), "--role", "Agent", "--text",
                  "Killed at 90 s on Loom in %s (%s): %s %s was still running at the kill. Red and P0 (Kirk, Oct 9)." % (arguments.run, unit, relative, test), dry=arguments.dry)
            continue
        owner = members.get(burndown.ownerOf(package, top), "system_adamic")
        title = "P0: %s (%s) killed at 90 s on Loom; fix or split it under 60 s" % (test, relative)
        body = ("Loom's run %s killed unit %s at its 90 s budget with %s %s still running (the unit's kill trap named it). "
                "A test killed at 90 s is a red and P0 (Kirk, Oct 9 03:17Z): fix or split it until it runs under 60 s on one "
                "4-CPU Codex instance. Loom closes this task itself after two runs in a row with it under 60 s." % (arguments.run, unit, relative, test))
        output = tasks("new", title, "--parent", "5g5151k", "--priority", "Now", "--owner", owner, "--content", body, dry=arguments.dry)
        created = re.search(r"#(\w{6,7})", output or "")
        filed[key] = {"task": created.group(1) if created else "dry", "owner": owner, "filed": arguments.run, "under": 0}
        print("filed %s for %s: %s" % (filed[key]["task"], owner, key))
    # Closing: a filed leaf under 60 s on two runs in a row is done.
    seconds = leafSeconds(arguments.tests) if os.path.exists(arguments.tests) else {}
    for key, entry in list(filed.items()):
        if any(key == package + " " + test for _, package, test in killed):
            continue
        measured = seconds.get(key)
        if measured is None:
            continue
        entry["under"] = entry.get("under", 0) + 1 if measured[1] == "pass" and measured[0] < 60 else 0
        if entry["under"] >= 2:
            tasks("done", entry["task"], "Under 60 s on two runs in a row on Loom (last: %.0f s in %s)." % (measured[0], arguments.run), dry=arguments.dry)
            print("closed %s: %s" % (entry["task"], key))
            del filed[key]
    if not arguments.dry:
        json.dump(filed, open(state + ".partial", "w"), indent=1)
        os.replace(state + ".partial", state)
    return 0


if __name__ == "__main__":
    sys.exit(main())
