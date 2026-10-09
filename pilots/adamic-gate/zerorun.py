#!/usr/bin/env python3
"""zerorun.py: the specs in a fast job that named tests and ran none of them (@system_adamic, Oct 9 11:01Z: a unit
that ran "no tests to run" read as passed, and a void is never a pass).

	zerorun.py <work directory>...     # a fast job's work directory, or a published fast record's checkout: job.json,
	                                   # record.jsonl and test.jsonl (each may be .gz)
	zerorun.py --tests <file> ... <work directory>   # also counts tests these go test -json files ran (a split, a rerun)
	zerorun.py --tree-tests <file> <work directory>  # the tree's own tests at the job's sha (treetests.py's lines)

A unit spec "<package>=^(A|B)$ skip=^(C)$" asks go test for the tests its run pattern matches, less those its skip
pattern matches; a fast gate's selection writes "^(A|B)", each name a family matched as a prefix. The job's test.jsonl
holds every test that ran, and every unit that reported (passed or failed) is checked against it:

	<work directory>	<unit>	<package>	requested tests ran: 0, requested <n> of: <names>
	<work directory>	<unit>	<package>	requested tests started <n>, verdicts <m> of: <names>
	<work directory>	<unit>	<package>	whole package started <n>, verdicts <m> of: <names>

The first is a spec that named tests, still had some left after its skip list (the kept and deferred tests), and ran
none of them. The second and third are tests that started and never reached a pass, fail or skip: a test binary that
died partway (@system_adamic, Oct 9 06:13Z: the trio's lint died at a watchdog with TestNodeTableIsLinkOnly's 8 shards
started and none decided, and this exited 0). A family whose every test that reached a verdict
skipped is noted on stderr: a skip proves nothing, but many tests skip by design on the pool. A name the tree at the job's sha doesn't hold, as a test or a family's
prefix, can't have run anywhere: it's noted on stderr as stale, never a red (floor1 1e8eff51's plan named three, and
push-main refused a record whose every real test passed). The tree is --tree-tests, else treetests.py at the units'
sha in ~/Projects/system/adamic-gate; when neither can be read every name counts, as before, and stderr says so.
Exit 1 when any line is printed, 0 when none is, 2 when a work directory can't be read.
"""
import gzip
import json
import os
import re
import subprocess
import sys


familySuffix = "((Unit|Points|_)[0-9]+|_Setup|_Union)?$"


def names(pattern):
    """The literal names in ^(A|B|C)$ or a selection's ^(A|B|C), or None for any other pattern (a whole package's ".").
    A numbered stem the planner writes once, stem_(?:000|001), is stem_000 and stem_001 (main.go's unquoteAlternation)."""
    # A selection's family spec carries its shards' suffix after the names (verify.sh writes it).
    if pattern.endswith(familySuffix):
        pattern = pattern[:-len(familySuffix)] + "$"
    match = re.fullmatch(r"\^\((.*)\)\$?", pattern)
    if not match:
        return None
    out, name, body, index = [], "", match.group(1), 0
    while index < len(body):
        character = body[index]
        if character == "\\" and index + 1 < len(body):
            name += body[index + 1]
            index += 2
            continue
        if body.startswith("(?:", index):
            depth, end = 1, index + 3
            while end < len(body) and depth:
                if body[end] == "\\":
                    end += 1
                elif body[end] == "(":
                    depth += 1
                elif body[end] == ")":
                    depth -= 1
                end += 1
            out.extend(name + suffix for suffix in names("^(" + body[index + 3:end - 1] + ")$"))
            name, index = "", end
            continue
        if character == "|":
            out.append(name)
            name = ""
        elif character == "(":
            return None  # a group alternation never writes: not a list of names
        else:
            name += character
        index += 1
    out.append(name)
    return [name for name in out if name]


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


def ran(test_lines, tests=None, started=None, decided=None):
    """Every top-level test that reached a verdict, every one that started, and every one that passed or failed (a skip
    proves nothing), by package."""
    tests = {} if tests is None else tests
    started = {} if started is None else started
    decided = {} if decided is None else decided
    for line in test_lines:
        try:
            event = json.loads(line)
        except ValueError:
            continue
        test = event.get("Test") or ""
        if test and "/" not in test:
            if event.get("Action") in ("pass", "fail", "skip"):
                tests.setdefault(event.get("Package", ""), set()).add(test)
                if event.get("Action") != "skip":
                    decided.setdefault(event.get("Package", ""), set()).add(test)
            elif event.get("Action") == "run":
                started.setdefault(event.get("Package", ""), set()).add(test)
    return tests, started, decided


def treeTests(job, given):
    """The tree's top-level tests at the job's sha, by package, or None when they can't be read."""
    lines = None
    if given:
        lines = open(given).read().splitlines()
    else:
        shas = {unit["argv"][unit["argv"].index("adamic-gate-unit") + 1] for unit in job.get("units") or []
                if "adamic-gate-unit" in (unit.get("argv") or [])}
        if len(shas) == 1:
            sha = shas.pop()
            repository = os.path.expanduser("~/Projects/system/adamic-gate")
            if subprocess.run(["git", "-C", repository, "cat-file", "-e", sha + "^{commit}"], capture_output=True).returncode != 0:
                subprocess.run(["git", "-C", repository, "fetch", "-q", "origin", sha], capture_output=True)
            listing = subprocess.run([sys.executable, os.path.join(os.path.dirname(os.path.abspath(__file__)), "treetests.py"), sha,
                                      "--repository", repository], capture_output=True, text=True)
            if listing.returncode == 0 and listing.stdout.strip():
                lines = listing.stdout.splitlines()
    if lines is None:
        return None
    tree = {}
    for line in lines:
        package, _, test = line.partition(" ")
        if test:
            tree.setdefault(package, set()).add(test)
    return tree


def reported(work):
    """The units whose latest attempt finished, passed or failed, on its own: a unit that never ran is a void already.
    The run's record, then its retry rounds' (verify.sh's again-<n>-record.jsonl), each attempt replacing the one before.
    An attempt that exited 2, or that uploaded no test results, is Loom's break, not a run of the unit's tests: the red
    list already names it BROKEN (Oct 9 14:30Z: gocacheprog 77dcb095 read 61 "ran: 0" reds from units that refused on a
    full /tmp or failed their checkout and uploaded nothing)."""
    paths = [os.path.join(work, "record.jsonl")]
    rounds = []
    for name in os.listdir(work):
        match = re.fullmatch(r"again-([0-9]+)-record\.jsonl(\.gz)?", name)
        if match:
            rounds.append((int(match.group(1)), os.path.join(work, name[:-3] if name.endswith(".gz") else name)))
    paths += [path for _, path in sorted(rounds)]
    latest = {}
    for path in paths:
        if not os.path.exists(path) and os.path.exists(path + ".gz"):
            lines = gzip.open(path + ".gz", "rt", errors="replace")
        else:
            lines = open(path, errors="replace") if os.path.exists(path) else []
        attempt = {}
        for line in lines:
            try:
                event = json.loads(line)
            except ValueError:
                continue
            event = event.get("event", event)
            if event.get("type") == "started":
                attempt[event.get("unit")] = {}
            elif event.get("type") == "exit":
                attempt.setdefault(event.get("unit"), {})["code"] = event.get("code")
            elif event.get("type") == "finished":
                attempt.setdefault(event.get("unit"), {})["status"] = event.get("status")
            elif event.get("type") == "uploaded" and re.fullmatch(r"loom-out/test\.jsonl(\.gz)?", event.get("path") or ""):
                attempt.setdefault(event.get("unit"), {})["results"] = True
        latest.update(attempt)
    return {unit for unit, attempt in latest.items()
            if attempt.get("status") in ("passed", "failed") and attempt.get("code") != 2 and attempt.get("results")}


def family(test, name):
    """A requested name is a family (@system_adamic, Oct 9 11:01Z): the test itself, or its split's shards, the name plus
    Unit, Points or _ and a number, as run.py 7adc5bf9 reads it. Never a bare prefix: TestWASI's family is TestWASIUnit00
    and on, never TestWASITargetFlags."""
    return test == name or re.fullmatch(re.escape(name) + r"(Unit|Points|_)[0-9]+", test) is not None


def sweep(work, extra=(), given=None):
    job = json.load(open(os.path.join(work, "job.json")))
    # A work directory holds test.jsonl; a published fast record (gate-logs/<sha12>/<stamp>/fast) gzips it when large.
    path = os.path.join(work, "test.jsonl")
    tests, started, decided = {}, {}, {}
    if os.path.exists(path):
        ran(open(path, errors="replace"), tests, started, decided)
    elif os.path.exists(path + ".gz"):
        ran(gzip.open(path + ".gz", "rt", errors="replace"), tests, started, decided)
    for more in extra:
        ran(open(more, errors="replace"), tests, started, decided)
    tree = treeTests(job, given)
    if tree is None:
        print("%s\tnote: the tree's tests at the job's sha couldn't be read, so every requested name counts" % work, file=sys.stderr)
    finished = reported(work)
    found, attributed = [], set()
    for unit in job.get("units") or []:
        if unit.get("id") not in finished:
            continue
        for package, pattern, skip in specs(unit):
            verdicts, begun, real = tests.get(package, set()), started.get(package, set()), decided.get(package, set())
            requested = names(pattern)
            if requested is None:
                # A whole package: every test that started reached a verdict, or its binary died partway.
                lost = sorted(test for test in begun - verdicts if (package, test) not in attributed)
                if lost:
                    attributed.update((package, test) for test in lost)
                    found.append("%s\t%s\t%s\twhole package started %d, verdicts %d of: %s" % (work, unit.get("id", "?"), package,
                                 len(begun), len(begun & verdicts), ", ".join(lost[:12])))
                continue
            skipped = set(names(skip) or [])
            wanted = []
            for name in requested:
                if name in skipped:
                    continue
                if tree is not None:
                    members = {test for test in tree.get(package, ()) if family(test, name)}
                    if not members:
                        print("%s\t%s\t%s\tnote: stale, the tree holds no test %s" % (work, unit.get("id", "?"), package, name), file=sys.stderr)
                        continue
                    if members <= skipped:
                        continue  # every member is kept or deferred: nothing was asked of this spec
                wanted.append(name)
            if not wanted:
                continue
            # A family ran if any test of the job, from any unit, is in it: a plan names tests from an older reference,
            # and a parent split into shards since then runs through its package's remainder spec, so only a family that
            # ran nowhere in the job is lost (Oct 9: the trio's internal/native ran no test of five requested families).
            none = [name for name in wanted if not any(family(test, name) for test in verdicts | begun)]
            if none:
                found.append("%s\t%s\t%s\trequested tests ran: 0, requested %d of: %s" % (work, unit.get("id", "?"), package, len(wanted), ", ".join(none[:12])))
            # One line per family, so each names its own counts (lint's TestNodeTableIsLinkOnly: 8 started, 0 decided).
            for name in wanted:
                begunHere = {test for test in begun if family(test, name)}
                lost = sorted(test for test in begunHere - verdicts if (package, test) not in attributed)
                if lost:
                    attributed.update((package, test) for test in lost)
                    found.append("%s\t%s\t%s\trequested tests started %d, verdicts %d of: %s (%s)" % (work, unit.get("id", "?"), package,
                                 len(begunHere), len(begunHere & verdicts), name, ", ".join(lost[:6]) + (", ..." if len(lost) > 6 else "")))
                # Every test of the family that reached a verdict skipped: it proved nothing, but many skip by design on
                # the pool (throughput and measurement tests, 26 families on floor1 1e8eff51's record), so it's a note,
                # never a red. TestWASI's shards, which skipped this way on every pool record, are guarded where they
                # broke: the planner's own test of their spec and its clang.
                decidedHere = {test for test in verdicts if family(test, name)}
                if decidedHere and not decidedHere & real:
                    print("%s\t%s\t%s\tnote: requested tests skipped %d of %d: %s" % (work, unit.get("id", "?"), package,
                          len(decidedHere), len(decidedHere), name), file=sys.stderr)
    return found


def main():
    if len(sys.argv) < 2:
        print(__doc__.strip().split("\n\n")[1])
        return 2
    status = 0
    arguments, extra, given = sys.argv[1:], [], None
    while arguments[:1] in (["--tests"], ["--tree-tests"]) and len(arguments) > 1:
        if arguments[0] == "--tests":
            extra.append(arguments[1])
        else:
            given = arguments[1]
        arguments = arguments[2:]
    for work in arguments:
        try:
            for line in sweep(work, extra, given):
                print(line)
                status = 1
        except (OSError, ValueError) as error:
            print("%s\tunreadable: %s" % (work, error), file=sys.stderr)
            return 2
    return status


if __name__ == "__main__":
    sys.exit(main())
