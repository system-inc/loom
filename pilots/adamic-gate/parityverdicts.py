#!/usr/bin/env python3
"""parityverdicts.py <events.jsonl> [--store <url or directory>]: one parity change's verdict records out of its event
slice, with each record's tests resolved, for boxparity.py.

GET /changes/<change>/events (contract 5 on #ykg8g6k) returns the change's slice of the log as JSON lines. Judge logs
each decided unit as a verdict.decided event with the record under data.verdict. This prints those records one per
line, the latest per unitKey when a unit was decided twice (a rerun alone replaces the first attempt's record), so
boxparity.py reads exactly one verdict per unit.

A record's tests come by reference (Loom's ruling, 01:17Z; contract 3's shape, #sf8tgvh): tests is {sha256, passed,
failed, skipped, inline}, where sha256 names the canonical tests list, a blob in the action store, which adamic-store.kirkouimet.com/blobs/<sha256> serves with no token. This fetches each list,
checks the bytes hash to the name the hash-chained event committed to, checks the counts and the inline failed and
never-ended rows agree with the list, and puts the list back inline. A list that
can't be fetched, or whose bytes hash to anything else, fails the run (exit 1): a parity side with a list nobody
checked proves nothing. A record whose tests are already a list is taken as it is.

	curl -sS "$pipeline/changes/<change>/events" -H "Authorization: Bearer $(cat <token file>)" > events.jsonl
	pilots/adamic-gate/parityverdicts.py events.jsonl > verdicts.jsonl
	pilots/adamic-gate/boxparity.py <box record dir> verdicts.jsonl

--store names the blob store: a URL (default https://adamic-store.kirkouimet.com) or a local directory of blobs named
by sha256, for tests. Exit 1 when the slice holds no verdict.decided event, since an empty side would compare as
everything absent.
"""

import hashlib
import json
import os
import re
import sys
import urllib.request

hashPattern = re.compile(r"^[0-9a-f]{64}$")


def fetch(store, name):
    """The blob named name, as bytes, from a store URL or a local directory."""
    if os.path.isdir(store):
        return open(os.path.join(store, name), "rb").read()
    with urllib.request.urlopen("%s/blobs/%s" % (store.rstrip("/"), name), timeout=60) as response:
        return response.read()


def resolved(verdict, store):
    """The verdict with tests as a list: fetched by hash and checked against it, or as it came."""
    tests = verdict.get("tests")
    if isinstance(tests, list) or tests is None:
        return verdict
    summary = tests if isinstance(tests, dict) else {}
    name = tests if isinstance(tests, str) else summary.get("sha256", "")
    if not hashPattern.match(name or ""):
        raise ValueError("unit %s: tests is neither a list nor a sha256 (%r)" % (verdict.get("unitKey"), tests))
    body = fetch(store, name)
    if hashlib.sha256(body).hexdigest() != name:
        raise ValueError("unit %s: tests blob %s hashes to %s" % (verdict.get("unitKey"), name, hashlib.sha256(body).hexdigest()))
    rows = json.loads(body)
    if summary:
        # The record's counts and inline rows must agree with the list it names (contract 3, #sf8tgvh).
        outcomes = [row.get("outcome") for row in rows]
        for field, outcome in (("passed", "pass"), ("failed", "fail"), ("skipped", "skip")):
            if summary.get(field) != outcomes.count(outcome):
                raise ValueError("unit %s: tests.%s is %r, the list holds %d" % (verdict.get("unitKey"), field, summary.get(field), outcomes.count(outcome)))
        unfinished = sorted((row["package"], row["test"], row["outcome"]) for row in rows if row.get("outcome") in ("fail", "run"))
        inline = sorted((row.get("package"), row.get("test"), row.get("outcome")) for row in summary.get("inline") or [])
        if inline != unfinished:
            raise ValueError("unit %s: tests.inline doesn't match the list's failed and never-ended rows" % verdict.get("unitKey"))
    return dict(verdict, tests=rows)


def main():
    arguments = [argument for argument in sys.argv[1:]]
    store = "https://adamic-store.kirkouimet.com"
    if "--store" in arguments:
        index = arguments.index("--store")
        store = arguments[index + 1]
        del arguments[index:index + 2]
    if len(arguments) != 1:
        print(__doc__.strip().splitlines()[0])
        return 2
    latest = {}
    for line in open(arguments[0], errors="replace"):
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if event.get("type") != "verdict.decided":
            continue
        data = event.get("data") or {}
        # Queue logs one record per event under data.verdict; a list under data.verdicts is read the same way.
        for verdict in data.get("verdicts") or [data.get("verdict") or data]:
            if verdict.get("unitKey"):
                latest[verdict["unitKey"]] = (event.get("seq") or 0, verdict)
    if not latest:
        print("parityverdicts: no verdict.decided event in the slice", file=sys.stderr)
        return 1
    try:
        records = [resolved(verdict, store) for _, verdict in sorted(latest.values(), key=lambda pair: pair[0])]
    except (OSError, ValueError) as error:
        print("parityverdicts: %s" % error, file=sys.stderr)
        return 1
    for verdict in records:
        print(json.dumps(verdict, sort_keys=True, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    sys.exit(main())
