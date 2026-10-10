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

Stages too (Loom's ruling, Oct 10 01:41Z): the box's verdict is its stages as well as its tests. A new-path phase unit's
record names its stage (parityverdicts.py's "phase", run.py's own unit line, whose first word is the stage), and a stage
passes there when every unit of it passed. Each stage is compared with the box's stages_exit (0 passes). The stages the
new path decides another way aren't phases: tests and products (their units, compared test for test above) and census
(the judge's census step). A stage the box ran that the new path has no phase for is a difference, since the new path
didn't check what the box did; a phase the box doesn't run (gofmt) differs only when it fails. A stage ruled out of
the new path by name (ruledOut: darwin-compile, Kirk's decision of Oct 10) is printed as ruled, never counted.
--no-stages compares tests alone, and the summary says so.
"""

import gzip
import json
import os
import sys

# The box stages the new path decides without a phase unit, and how.
decidedOtherwise = {"tests": "test units", "products": "product units", "census": "the judge's census step"}

# Box stages ruled out of the new path's gate by name: a known loosening, printed and never counted as a difference.
ruledOut = {"darwin-compile": "Kirk, Oct 10: after Loom 1.0 (#36v8xkq)"}


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
    outcomes, units, changes, stages = {}, {}, set(), {}
    for number, line in enumerate(open(path, errors="replace"), 1):
        if not line.strip():
            continue
        verdict = json.loads(line)
        changes.add(verdict.get("change"))
        if verdict.get("status") not in ("passed", "failed"):
            # The cause says what happens next: overBudget (Kirk's 90 s shape) is split and rerun, a void is rerun.
            cause = "/".join(part for part in (verdict.get("cause"), verdict.get("infra")) if part)
            return None, None, changes, None, "verdict %s (line %d) is %s%s, not passed or failed" % (verdict.get("unitKey"), number, verdict.get("status"), " (%s)" % cause if cause else "")
        if verdict.get("phase") is not None:
            stage = (verdict["phase"].split() or [""])[0]
            stages.setdefault(stage, []).append((verdict.get("unitKey"), verdict["status"]))
            continue
        for entry in verdict.get("tests") or []:
            test = entry.get("test") or ""
            if not test or (topLevel and "/" in test):
                continue
            key = (entry.get("package") or "", test)
            if key in outcomes and outcomes[key] != entry.get("outcome"):
                return None, None, changes, None, "%s %s: units %s and %s disagree (%s, %s)" % (key[0], key[1], units[key], verdict.get("unitKey"), outcomes[key], entry.get("outcome"))
            outcomes[key], units[key] = entry.get("outcome"), verdict.get("unitKey")
    return outcomes, units, changes, stages, None


def stageDifferences(summary, stages):
    """Each stage whose outcome differs, and how many stages agree."""
    boxExits = summary.get("stages_exit") or {}
    differences, same, ruled = [], 0, []
    for stage in sorted(set(boxExits) | set(stages)):
        if stage in decidedOtherwise and stage not in stages:
            continue
        if stage in ruledOut and stage not in stages:
            ruled.append("stage %s: box exit %s, not on the new path, ruled: %s" % (stage, boxExits.get(stage), ruledOut[stage]))
            continue
        box = None if stage not in boxExits else ("pass" if boxExits[stage] == 0 else "fail (exit %s)" % boxExits[stage])
        units = stages.get(stage)
        new = None if units is None else ("pass" if all(status == "passed" for _, status in units) else "fail")
        if box is None and new == "pass":
            same += 1
            continue
        if box is not None and new is not None and box.split()[0] == new:
            same += 1
            continue
        named = "" if not units else " (units %s)" % ", ".join(sorted(unitKey[:12] for unitKey, status in units if status != "passed" or new == "pass"))
        differences.append("stage %s: box %s, new %s%s" % (stage, box or "absent", new or "no phase unit", named))
    return differences, same, ruled


def main():
    arguments = [argument for argument in sys.argv[1:] if not argument.startswith("--")]
    topLevel = "--top-level" in sys.argv
    withStages = "--no-stages" not in sys.argv
    if len(arguments) != 2:
        print(__doc__.strip().splitlines()[0])
        return 2
    try:
        box, summary, boxReason = boxSide(arguments[0], topLevel)
        new, units, changes, stages, newReason = newSide(arguments[1], topLevel)
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
    stageLine = "stages not compared (--no-stages)"
    if withStages:
        stageDiffers, stagesSame, stagesRuled = stageDifferences(summary, stages)
        stageLine = "%d stages the same, %d differ, %d ruled out" % (stagesSame, len(stageDiffers), len(stagesRuled))
        differences += stageDiffers
    print("boxparity: %s, %s at %s: %d tests the same, %d differ (box %d, new %d, from %d units); %s" % (
        "identical" if not differences else "differs", summary.get("sha", "?")[:12], summary.get("base", "?")[:12], same,
        len(differences) - (len(stageDiffers) if withStages else 0), len(box), len(new), len(set(units.values())), stageLine))
    for line in differences:
        print(line)
    for line in (stagesRuled if withStages else []):
        print(line)
    return 1 if differences else 0


if __name__ == "__main__":
    sys.exit(main())
