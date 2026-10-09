#!/usr/bin/env python3
"""inputs.py: every unit's input hash, from git trees at the unit's sha, so a rerun on a later sha runs only the units
whose inputs moved and keeps every other verdict (Kirk, Oct 9 03:17Z; #apf5vdb, developer tools' #hpjftdj).

	pilots/adamic-gate/inputs.py hash --sha <sha> [--at <sha>] <job.json>... > inputs.json   # a copy in ~/.loom/bin
	pilots/adamic-gate/inputs.py rerun-plan --base-sha <A> --sha <B> --verdicts <verdicts.json> --out <directory> <job.json>...

rerun-plan takes a finished run's jobs, planned at A, and its units' verdicts ({"<unit>": "passed" | "failed" |
"killed" | "broken" | "missing"}), and plans the rerun at B, a descendant of A: a unit runs again when its input hash
at B differs from A's or its verdict wasn't passed, and keeps its verdict otherwise. It writes rerun-<n>.json (the
units that run again, at B, each job's own), inputs-base.json and inputs.json (every unit's hash at A and at B) and
plan.txt (each unit, kept or rerun and why). A census unit (phase census) is left out: it reads the merged go test
lines, so the rerun plans its own over the kept and the rerun units' lines.

The definition, agreed with developer tools on Oct 9 (03:24Z), named in every output as "loom-inputs-v1":
- shared: the git tree at the sha of every path except *_test.go files, anything under a testdata/ directory, review
  evidence (review/) and the records every landing writes (documentation/velocity/landings.csv, stage3/progress.json,
  stage3/meter/runs/), submodules entering as their commits. Any other change moves every unit, so the whole suite
  reruns. No Go code reads those records, and a test that reads review evidence declares it (below), so integration's
  test-only landings, which carry them, move only the packages whose tests they change.
- own: for a test unit, per package it runs, the package directory's own *_test.go files and its testdata/ tree, plus
  the paths cloud/fast-gate/test-reads.json declares that package reads ({"<package directory>": ["<path>", ...]}, a
  path being a file or a directory, "." the whole tree). Until that file exists, the reads found on Oct 9 stand in for
  it (knownReads); once it does, the file at the hashed sha is the whole declaration. A phase unit (run.py --phase) owns the whole tree, since its
  inputs aren't declared per package: any change at all reruns it.
- toolchain: the gate inputs' manifest hash the unit's body pins (Go, clang, TypeScript, corpora) and, for a phase
  unit, the tools sha it runs run.py from.
- run: sha256 of the unit's argv with the sha left out (its body, its -run and -skip patterns), its timeout and its
  resources: a unit planned differently is a different unit.
- input_sha256: sha256 of the canonical JSON (sorted keys, no spaces) of {shared, own, toolchain, run}.

A unit's argv is [bash, -c, <body>, <name>, <sha>, ...]: test units add one "<import path>=<pattern> [skip=...]"
argument per package, phase units the tools sha, the phase and its unit. The remainder unit's "@unplanned=<import
path>,..." runs every package go list names that the list doesn't, so it owns every test path outside those packages
(a package new since the plan's reference, wherever it lands).
"""

import argparse
import hashlib
import json
import os
import re
import subprocess
import sys

Definition = "loom-inputs-v1"
module = "github.com/system-inc/adamic/"
gateRepository = os.path.expanduser("~/Projects/system/adamic-gate")
testReadsPath = "cloud/fast-gate/test-reads.json"
# Paths no unit's shared part covers: test inputs a package owns, review evidence, and the landings' own records.
records = ("documentation/velocity/landings.csv", "stage3/progress.json")
# The reads of review evidence and of other packages' test files that Go tests made on main on Oct 9, standing in for
# cloud/fast-gate/test-reads.json until developer tools lands it: internal/lower reads review/optional-indexing, and
# cmd/adamic-gate's tests walk every _test.go and shards.json in the repository (the parallel check, the shards).
knownReads = {"internal/lower": ["review/optional-indexing"], "cmd/adamic-gate": ["."]}


def sha256(text):
    return hashlib.sha256(text.encode()).hexdigest()


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"))


def isTestPath(path):
    return path.endswith("_test.go") or "testdata" in path.split("/")


def isShared(path):
    return not (isTestPath(path) or path.startswith(("review/", "stage3/meter/runs/")) or path in records)


class Tree:
    """One sha's tree, listed once: every blob and submodule with its mode, type and object."""

    def __init__(self, repository, sha):
        self.sha = sha
        listing = subprocess.run(["git", "-C", repository, "ls-tree", "-r", "-z", "--full-tree", sha], check=True, capture_output=True).stdout
        self.entries = []
        for record in listing.split(b"\0"):
            if record:
                meta, path = record.decode().split("\t", 1)
                self.entries.append((path, meta))
        self.entries.sort()
        self.root = subprocess.run(["git", "-C", repository, "rev-parse", sha + "^{tree}"], check=True, capture_output=True, text=True).stdout.strip()
        self.shared = sha256("".join("%s\t%s\n" % (meta, path) for path, meta in self.entries if isShared(path)))
        reads = [meta for path, meta in self.entries if path == testReadsPath]
        self.reads = knownReads
        if reads:
            blob = reads[0].split()[2]
            self.reads = json.loads(subprocess.run(["git", "-C", repository, "cat-file", "blob", blob], check=True, capture_output=True, text=True).stdout)
        self.owned = {}

    def unplanned(self, known):
        """Every test path whose package directory isn't one of known: what an @unplanned spec may run."""
        lines = []
        for path, meta in self.entries:
            if not isTestPath(path):
                continue
            parts = path.split("/")
            directory = "/".join(parts[: parts.index("testdata")]) if "testdata" in parts else os.path.dirname(path)
            if directory not in known:
                lines.append("%s\t%s\n" % (meta, path))
        return sha256("".join(lines))

    def own(self, directory):
        """The package directory's own test files and testdata, and the paths test-reads.json says it reads."""
        if directory not in self.owned:
            declared = self.reads.get(directory) or []
            lines = []
            for path, meta in self.entries:
                mine = (os.path.dirname(path) == directory and path.endswith("_test.go")) or path.startswith(directory + "/testdata/")
                read = any(other in (".", "./") or path == other or path.startswith(other.rstrip("/") + "/") for other in declared)
                if mine or read:
                    lines.append("%s\t%s\n" % (meta, path))
            self.owned[directory] = sha256("".join(lines))
        return self.owned[directory]


def packagesOf(unit):
    """A test unit's packages, as directories in the repository, from its "<import path>=<pattern>" arguments."""
    directories = []
    for argument in unit["argv"][5:]:
        if argument.startswith("@unplanned="):
            continue
        importPath = argument.split("=", 1)[0]
        if not importPath.startswith(module):
            raise ValueError("%s: %r names no package of %s" % (unit["id"], argument, module))
        directories.append(importPath[len(module):])
    return sorted(set(directories))


def unitInputs(unit, tree):
    argv = unit["argv"]
    if len(argv) < 5 or argv[:2] != ["bash", "-c"]:
        raise ValueError("%s: an argv this hash doesn't know: %r" % (unit["id"], argv[:4]))
    if argv[4] != tree.sha:
        raise ValueError("%s: planned at %s, hashed at %s" % (unit["id"], argv[4], tree.sha))
    pinned = re.search(r"\bgateInputs=([0-9a-f]{64})\b", argv[2])
    toolchain = {"gate_inputs": pinned.group(1) if pinned else ""}
    if argv[3] == "adamic-gate-phase":
        toolchain["tools"] = argv[5]
        own = {"tree": tree.root}
    else:
        own = {directory: tree.own(directory) for directory in packagesOf(unit)}
        for argument in argv[5:]:
            if argument.startswith("@unplanned="):
                known = {name[len(module):] for name in argument[len("@unplanned="):].split(",") if name.startswith(module)}
                own["@unplanned"] = tree.unplanned(known)
    run = sha256(canonical({"argv": argv[:4] + argv[5:], "timeoutSeconds": unit.get("timeoutSeconds"), "resources": unit.get("resources")}))
    parts = {"shared": tree.shared, "own": own, "toolchain": toolchain, "run": run}
    return {"id": unit["id"], "input_sha256": sha256(canonical(parts)), "parts": parts}


def retarget(job, sha):
    """The job as it runs at another sha: each unit's argv names its sha at index 4, and nothing else does."""
    moved = json.loads(json.dumps(job))
    for unit in moved["units"]:
        unit["argv"][4] = sha
    return moved


def hashJobs(sha, jobs, repository=gateRepository):
    tree = Tree(repository, sha)
    units = []
    for job in jobs:
        for unit in job["units"]:
            units.append(unitInputs(unit, tree))
    return {"definition": Definition, "sha": sha, "units": units}


def isCensus(unit):
    return unit["argv"][3:4] == ["adamic-gate-phase"] and unit["argv"][6:7] == ["census"]


def rerunPlan(baseSha, sha, verdicts, jobs, out, repository=gateRepository):
    ancestor = subprocess.run(["git", "-C", repository, "merge-base", "--is-ancestor", baseSha, sha])
    if ancestor.returncode != 0:
        raise ValueError("%s doesn't descend from %s: a rerun keeps verdicts only along history" % (sha, baseSha))
    jobs = [{**job, "units": [unit for unit in job["units"] if not isCensus(unit)]} for job in jobs]
    base = hashJobs(baseSha, [retarget(job, baseSha) for job in jobs], repository)
    moved = [retarget(job, sha) for job in jobs]
    here = hashJobs(sha, moved, repository)
    before = {unit["id"]: unit["input_sha256"] for unit in base["units"]}
    if len(before) != len(base["units"]):
        raise ValueError("two units share an id across the jobs; a rerun keys verdicts by id")
    os.makedirs(out, exist_ok=True)
    lines, rerun = [], set()
    for unit in here["units"]:
        verdict = verdicts.get(unit["id"], "missing")
        if unit["input_sha256"] != before[unit["id"]]:
            why = "inputs moved"
            rerun.add(unit["id"])
        elif verdict != "passed":
            why = "was " + verdict
            rerun.add(unit["id"])
        else:
            why = None
        lines.append("%s %s%s" % (unit["id"], "rerun" if why else "kept", ": " + why if why else ""))
    for index, job in enumerate(moved):
        job["units"] = [unit for unit in job["units"] if unit["id"] in rerun]
        job["name"] = job.get("name", "job") + "-rerun"
        json.dump(job, open(os.path.join(out, "rerun-%d.json" % index), "w"))
    json.dump(base, open(os.path.join(out, "inputs-base.json"), "w"), indent=1)
    json.dump(here, open(os.path.join(out, "inputs.json"), "w"), indent=1)
    open(os.path.join(out, "plan.txt"), "w").write("\n".join(lines) + "\n")
    return len(rerun), len(lines)


def main():
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="command", required=True)
    hashing = commands.add_parser("hash", help="every unit's input hash at the sha its jobs were planned at")
    hashing.add_argument("--sha", required=True, help="the sha the jobs were planned at")
    hashing.add_argument("--at", help="hash them as they would run at this sha instead")
    hashing.add_argument("--repository", default=gateRepository)
    hashing.add_argument("jobs", nargs="+")
    planning = commands.add_parser("rerun-plan", help="the units of a finished run that run again at a later sha")
    planning.add_argument("--base-sha", required=True)
    planning.add_argument("--sha", required=True)
    planning.add_argument("--verdicts", required=True)
    planning.add_argument("--out", required=True)
    planning.add_argument("--repository", default=gateRepository)
    planning.add_argument("jobs", nargs="+")
    arguments = parser.parse_args()
    jobs = [json.load(open(path)) for path in arguments.jobs]
    if arguments.command == "hash":
        if arguments.at:
            jobs = [retarget(job, arguments.at) for job in jobs]
        json.dump(hashJobs(arguments.at or arguments.sha, jobs, arguments.repository), sys.stdout, indent=1)
        print()
    elif arguments.command == "rerun-plan":
        verdicts = json.load(open(arguments.verdicts))
        rerun, total = rerunPlan(arguments.base_sha, arguments.sha, verdicts, jobs, arguments.out, arguments.repository)
        print("rerun %d of %d units at %s, the rest keep %s's verdicts (%s)" % (rerun, total, arguments.sha[:12], arguments.base_sha[:12], arguments.out))


if __name__ == "__main__":
    sys.exit(main())
