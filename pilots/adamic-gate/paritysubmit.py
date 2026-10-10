#!/usr/bin/env python3
"""paritysubmit.py <box record dir> <adamic repository> <owner>: the POST /changes body that reruns a box record on the
new path, for parity proof 1 (#7rys8g0; parity mode #6c3xkws, its selection override in loom 02ea607).

Proof 1 is execution parity on the same selection: the new path runs exactly the tests the box ran, on exactly the tree
the box tested, uncached, and boxparity.py compares the two test for test. So the body pins:
- sha and base from the record's fast.json. The base must be an ancestor of sha, so merge(base, sha) is sha's own tree;
  a record that gated a merge is refused here (submit its refs/gate-merges/<sha> by hand, with its first parent as base);
- paths, the diff base..sha as git names it;
- select.packages, the record's packages, and select.tests, every package's top-level tests the box planned
  (test_outcomes) plus its product tests (units marked product), since run.py runs each test as its own unit.

A record whose oracle lanes ran a fixture subset (oracle_selection not whole) is refused: its lane tests ran on fewer
fixtures than their names say, and no test list can say which. Prints the body as canonical JSON; exit 1 on a refusal.
"""

import json
import os
import subprocess
import sys


def body(directory, repository, owner):
    summary = json.load(open(os.path.join(directory, "fast.json")))
    sha, base = summary["sha"], summary["base"]
    selection = summary.get("oracle_selection")
    if selection and not selection.get("whole"):
        raise ValueError("oracle lanes ran a fixture subset (%s), so no test list reproduces them" % selection.get("reason"))
    if subprocess.run(["git", "-C", repository, "merge-base", "--is-ancestor", base, sha]).returncode != 0:
        raise ValueError("base %s is not an ancestor of %s: the record gated a merge; submit refs/gate-merges/%s by hand" % (base[:12], sha[:12], sha))
    paths = subprocess.run(["git", "-C", repository, "diff", "--name-only", base, sha], check=True, capture_output=True, text=True).stdout.split()
    tests = {}
    for outcome in summary.get("test_outcomes") or []:
        tests.setdefault(outcome["package"], set()).add(outcome["test"])
    for unit in summary.get("units") or []:
        if unit.get("product") and "/" not in unit.get("test", ""):
            tests.setdefault(unit["package"], set()).add(unit["test"])
    packages = sorted(set(summary.get("packages") or []) | set(tests))
    return {"sha": sha, "base": base, "owner": owner, "paths": paths, "parity": True,
            "select": {"packages": packages, "tests": {package: sorted(names) for package, names in sorted(tests.items())}}}


def main():
    if len(sys.argv) != 4:
        print(__doc__.strip().splitlines()[0])
        return 2
    try:
        result = body(*sys.argv[1:])
    except ValueError as error:
        print("paritysubmit: refused: %s" % error, file=sys.stderr)
        return 1
    print(json.dumps(result, sort_keys=True, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    sys.exit(main())
