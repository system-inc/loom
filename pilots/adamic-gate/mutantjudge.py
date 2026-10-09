#!/usr/bin/env python3
# mutantjudge.py: one gate mutant's pool run against the verdict a correct gate already knows (#fyvmsy8; the suite is
# adamic-gate's cloud/gate-mutants.tsv, the box watcher's judgeMutant reads the same table). canary.sh runs it from its
# own directory, never from the tools under test, so a candidate tool set can't soften the judge that holds it.
#
#	mutantjudge.py <step> <pattern> <sha> <verdict file> <reds.txt> <test.jsonl> <floor>
#
# Prints one line: "ok", "void <cause>" (the run proved nothing, so the suite holds and the mutant runs again), or
# "wrong <what it read>" (the tools misjudged a known answer, and nothing promotes).
#
# Steps, as the pool's verdicts name them: "tests" is a red whose first failure is a Go test ("first: <package> <test>",
# or "go tests only" when the landing's other stages passed); a phase step such as "census" is a red reading "first
# failure at <step>"; "thin" is a canary input whose landings select too little, which must read green under the floor
# of passed tests (canary.sh reads that as void and never promotes on it, hole 5).
import os, re, sys

step, pattern, sha, verdictPath, redsPath, testsPath, floor = sys.argv[1:8]
verdict = open(verdictPath).readline().strip() if os.path.exists(verdictPath) else ""

if not verdict:
    print("void no verdict")
    sys.exit(0)
if verdict.startswith("void:"):
    print("void " + verdict[:160])
    sys.exit(0)

if step == "thin":
    passed = set()
    for line in open(testsPath, errors="replace") if os.path.exists(testsPath) else []:
        match = re.search(r'"Action":"pass","Package":"([^"]*)","Test":"([^"/]*)"', line)
        if match:
            passed.add(match.group(1) + " " + match.group(2))
    if verdict.startswith("green: " + sha + " ") and len(passed) < int(floor):
        print("ok")
    else:
        print("wrong %s (expected a green under %s passed tests, read %d)" % (verdict[:160], floor, len(passed)))
    sys.exit(0)

# Green is the failure this suite exists to catch: the tools passed a change a correct gate reds.
if not verdict.startswith("red: " + sha + " "):
    print("wrong %s (expected red at %s)" % (verdict[:160], step))
    sys.exit(0)
# A selection that refused the change is a red at another step, not the declared one.
if "selection refused" in verdict:
    print("wrong %s (expected red at %s, read a refused selection)" % (verdict[:160], step))
    sys.exit(0)

if step == "tests":
    at = re.search(r"first failure at ([^ ,(]+)", verdict)
    if at:
        print("wrong red at %s (expected red at tests)" % at.group(1))
        sys.exit(0)
    fails = [line[5:].strip() for line in open(redsPath, errors="replace")] if os.path.exists(redsPath) else []
    fails = [line for line in fails if line]
    if pattern and not any(re.search(pattern, line) for line in fails):
        print("wrong red at tests whose failures don't match %s (%s)" % (pattern, "; ".join(fails[:3]) or "no FAIL lines"))
        sys.exit(0)
    print("ok")
    sys.exit(0)

if not re.search(r"first failure at " + re.escape(step) + r"([ ,(]|$)", verdict):
    print("wrong %s (expected red at %s)" % (verdict[:160], step))
    sys.exit(0)
if pattern and not re.search(pattern, verdict):
    print("wrong red at %s that doesn't match %s" % (step, pattern))
    sys.exit(0)
print("ok")
