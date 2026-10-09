#!/usr/bin/env python3
"""zerorun.py: the specs in a fast job that named tests and ran none of them (@system_adamic, Oct 9 11:01Z: a unit
that ran "no tests to run" read as passed, and a void is never a pass).

	zerorun.py <work directory>...     # a fast job's work directory: its job.json and its test.jsonl
	zerorun.py --tests <file> ... <work directory>   # also counts tests these go test -json files ran (a split, a rerun)

A unit spec "<package>=^(A|B)$ skip=^(C)$" asks go test for the tests its run pattern matches, less those its skip
pattern matches. The job's test.jsonl holds every test that ran, so a spec whose patterns match none of its package's
tests there ran nothing. A spec that named tests, still had some left after its skip list (a skip list is the kept and
deferred tests), and ran none of them is zero-run. One line per zero-run spec:

	<work directory>	<unit>	<package>	requested <n>, ran 0: <names>

A whole package (run pattern ".") is never zero-run here: go test with -run . on a package with tests always runs one.
Exit 1 when any spec is zero-run, 0 when none is, 2 when a work directory can't be read.
"""
import json
import os
import re
import sys


def names(pattern):
    """The literal names in ^(A|B|C)$, or None for any other pattern (a whole package's ".")."""
    match = re.fullmatch(r"\^\((.*)\)\$", pattern)
    if not match:
        return None
    return [re.sub(r"\\(.)", r"\1", name) for name in match.group(1).split("|") if name]


def specs(unit):
    """A unit's package specs: (package, run pattern, skip pattern or "")."""
    argv = unit.get("argv") or []
    if "adamic-gate-unit" not in argv:
        return []
    out = []
    for spec in argv[argv.index("adamic-gate-unit") + 2:]:
        if spec.startswith("@") or "=" not in spec:
            continue
        package, pattern = spec.split("=", 1)
        skip = ""
        if " skip=" in pattern:
            pattern, skip = pattern.split(" skip=", 1)
        out.append((package, pattern, skip))
    return out


def ran(test_lines):
    """Every top-level test that reached a verdict, by package."""
    tests = {}
    for line in test_lines:
        try:
            event = json.loads(line)
        except ValueError:
            continue
        test = event.get("Test") or ""
        if test and "/" not in test and event.get("Action") in ("pass", "fail", "skip"):
            tests.setdefault(event.get("Package", ""), set()).add(test)
    return tests


def passed(work):
    """The units the run's own record says passed: only they can be a false green (a unit that never ran is a void)."""
    units = set()
    path = os.path.join(work, "record.jsonl")
    for line in open(path, errors="replace") if os.path.exists(path) else []:
        try:
            event = json.loads(line)
        except ValueError:
            continue
        event = event.get("event", event)
        if event.get("type") == "finished" and event.get("status") == "passed":
            units.add(event.get("unit"))
    return units


def sweep(work, extra=()):
    job = json.load(open(os.path.join(work, "job.json")))
    path = os.path.join(work, "test.jsonl")
    tests = ran(open(path, errors="replace") if os.path.exists(path) else [])
    for more in extra:
        for package, extra_tests in ran(open(more, errors="replace")).items():
            tests.setdefault(package, set()).update(extra_tests)
    green = passed(work)
    found = []
    for unit in job.get("units") or []:
        if unit.get("id") not in green:
            continue
        for package, pattern, skip in specs(unit):
            requested = names(pattern)
            if requested is None:
                continue
            skipped = set(names(skip) or [])
            wanted = [name for name in requested if name not in skipped]
            if not wanted:
                continue
            # A requested name is a family (@system_adamic, Oct 9 11:01Z): it ran if any test of the job, from any unit,
            # is that name or starts with it. A plan names tests from an older reference, and a parent split into
            # TestX_000 shards since then runs through its package's remainder spec, so only a family that ran nowhere
            # in the job is lost (Oct 9: the trio's internal/native ran no test of five requested families).
            lost = [name for name in wanted if not any(test == name or test.startswith(name) for test in tests.get(package, ()))]
            if lost:
                found.append("%s\t%s\t%s\trequested %d, ran 0 of: %s" % (work, unit.get("id", "?"), package, len(wanted), ", ".join(lost[:12])))
    return found


def main():
    if len(sys.argv) < 2:
        print(__doc__.strip().split("\n\n")[1])
        return 2
    status = 0
    arguments, extra = sys.argv[1:], []
    while arguments[:1] == ["--tests"] and len(arguments) > 1:
        extra.append(arguments[1])
        arguments = arguments[2:]
    for work in arguments:
        try:
            for line in sweep(work, extra):
                print(line)
                status = 1
        except (OSError, ValueError) as error:
            print("%s\tunreadable: %s" % (work, error), file=sys.stderr)
            return 2
    return status


if __name__ == "__main__":
    sys.exit(main())
