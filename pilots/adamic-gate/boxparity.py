#!/usr/bin/env python3
"""boxparity.py <box record dir> <verdicts.jsonl>: parity proof 1's comparator (#1th6k0h, contracts v1 on #ykg8g6k).

Does the new path reproduce the box's verdict on one candidate, test for test? The box record is a gate-logs/<sha12>/
<stamp>/fast tree: its fast.json and its test.jsonl or test.jsonl.gz (go test JSON). The new path's side is one candidate's verdict
records (contract 3), one JSON object per line. A test's outcome is its last word (pass, fail or skip) in its package,
as parity.py reads it; on the new side it is the verdict's tests[] entry, and the unitKey that ran it is named.

Either side can be not comparable, and that is never parity:
- the box record stopped (a ceiling or a kill) or holds a planned test that never ran;
- a new-path verdict is void, or isn't passed or failed;
- the new side holds one test twice with two outcomes (two units disagree, so neither is the answer).
The caller pairs the two sides by candidate: the box record names its sha and base, and the summary prints them.

The first line is the summary; each difference follows, sorted. Exit 0 when every test agrees, 1 when any differs,
2 when a file can't be read, 3 when either side isn't comparable.

	pilots/adamic-gate/boxparity.py gate-logs/26b08c000a07/20261009T211158Z/fast verdicts.jsonl [--top-level]

--top-level compares top-level tests only (no '/' in the name), for a new path whose tests[] carries no subtests.
"""

import gzip
import json
import os
import sys


def boxSide(directory, topLevel):
    """The box's outcomes by (package, test), and why it isn't comparable, or None."""
    summary = json.load(open(os.path.join(directory, "fast.json")))
    if summary.get("stopped"):
        return None, summary, "box record stopped: %s" % json.dumps(summary["stopped"], sort_keys=True)
    notRun = [outcome for outcome in summary.get("test_outcomes") or [] if outcome.get("status") == "not run"]
    if notRun:
        return None, summary, "box record holds %d planned tests that never ran" % len(notRun)
    outcomes = {}
    plain = os.path.join(directory, "test.jsonl")
    # Older records keep their go test lines gzipped.
    stream = open(plain, errors="replace") if os.path.exists(plain) else gzip.open(plain + ".gz", "rt", errors="replace")
    for line in stream:
        try:
            event = json.loads(line)
        except ValueError:
            continue
        test = event.get("Test")
        if not test or event.get("Action") not in ("pass", "fail", "skip") or (topLevel and "/" in test):
            continue
        outcomes[(event.get("Package") or "", test)] = event["Action"]
    return outcomes, summary, None


def newSide(path, topLevel):
    """The new path's outcomes by (package, test) with the unitKey that ran each, and why it isn't comparable, or None."""
    outcomes, units, changes = {}, {}, set()
    for number, line in enumerate(open(path, errors="replace"), 1):
        if not line.strip():
            continue
        verdict = json.loads(line)
        changes.add(verdict.get("change"))
        if verdict.get("status") not in ("passed", "failed"):
            return None, None, changes, "verdict %s (line %d) is %s, not passed or failed" % (verdict.get("unitKey"), number, verdict.get("status"))
        for entry in verdict.get("tests") or []:
            test = entry.get("test") or ""
            if not test or (topLevel and "/" in test):
                continue
            key = (entry.get("package") or "", test)
            if key in outcomes and outcomes[key] != entry.get("outcome"):
                return None, None, changes, "%s %s: units %s and %s disagree (%s, %s)" % (key[0], key[1], units[key], verdict.get("unitKey"), outcomes[key], entry.get("outcome"))
            outcomes[key], units[key] = entry.get("outcome"), verdict.get("unitKey")
    return outcomes, units, changes, None


def main():
    arguments = [argument for argument in sys.argv[1:] if not argument.startswith("--")]
    topLevel = "--top-level" in sys.argv
    if len(arguments) != 2:
        print(__doc__.strip().splitlines()[0])
        return 2
    try:
        box, summary, boxReason = boxSide(arguments[0], topLevel)
        new, units, changes, newReason = newSide(arguments[1], topLevel)
    except (OSError, ValueError) as error:
        print("boxparity: unreadable: %s" % error)
        return 2
    for reason in (boxReason, newReason):
        if reason:
            print("boxparity: not comparable: %s" % reason)
            return 3
    differences = []
    for key in sorted(set(box) | set(new)):
        if box.get(key) != new.get(key):
            differences.append("%s %s: box %s, new %s%s" % (key[0], key[1], box.get(key, "absent"), new.get(key, "absent"), " (unit %s)" % units[key] if key in units else ""))
    same = len(set(box) | set(new)) - len(differences)
    print("boxparity: %s, %s at %s: %d tests the same, %d differ (box %d, new %d, from %d units)" % (
        "identical" if not differences else "differs", summary.get("sha", "?")[:12], summary.get("base", "?")[:12], same,
        len(differences), len(box), len(new), len(set(units.values()))))
    for line in differences:
        print(line)
    return 1 if differences else 0


if __name__ == "__main__":
    sys.exit(main())
