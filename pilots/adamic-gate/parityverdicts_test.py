#!/usr/bin/env python3
"""parityverdicts_test.py [<parityverdicts.py>]: parityverdicts.py on planted event slices and a local blob store. A
tests hash resolves to its list; a rerun's record replaces the first; a blob whose bytes hash to another name, a
missing blob and an empty slice each fail. The mutant that skips the hash check must fail:

	python3 pilots/adamic-gate/parityverdicts_test.py
	sed 's/if hashlib.sha256(body).hexdigest() != name:/if False:/' parityverdicts.py > m.py && python3 parityverdicts_test.py m.py   # fails
"""

import hashlib
import json
import os
import subprocess
import sys
import tempfile

here = os.path.dirname(os.path.abspath(__file__))
parityverdicts = sys.argv[1] if len(sys.argv) > 1 else os.path.join(here, "parityverdicts.py")
failures = 0
tests = [{"outcome": "pass", "package": "github.com/system-inc/adamic/internal/oracle", "test": "TestA"},
         {"outcome": "pass", "package": "github.com/system-inc/adamic/internal/oracle", "test": "TestA/sub"}]


def check(name, condition, output):
    global failures
    print(("PASS " if condition else "FAIL ") + name + ("" if condition else ": " + output))
    failures += 0 if condition else 1


def blob(store, value):
    body = json.dumps(value, sort_keys=True, separators=(",", ":")).encode()
    name = hashlib.sha256(body).hexdigest()
    open(os.path.join(store, name), "wb").write(body)
    return name


def events(root, verdicts):
    path = tempfile.mktemp(dir=root, suffix=".jsonl")
    with open(path, "w") as stream:
        stream.write(json.dumps({"seq": 1, "type": "change.submitted", "data": {}}) + "\n")
        for seq, verdict in enumerate(verdicts, 2):
            stream.write(json.dumps({"seq": seq, "type": "verdict.decided", "data": {"verdict": verdict}}) + "\n")
    return path


def run(path, store):
    result = subprocess.run([sys.executable, parityverdicts, path, "--store", store], capture_output=True, text=True)
    return result.returncode, result.stdout, result.stderr


root = tempfile.mkdtemp()
store = os.path.join(root, "store")
os.mkdir(store)
good = blob(store, tests)

code, output, error = run(events(root, [{"unitKey": "k1", "status": "passed", "tests": good}]), store)
records = [json.loads(line) for line in output.splitlines()]
check("a tests hash resolves to its list", code == 0 and records and records[0]["tests"] == tests, output + error)

code, output, error = run(events(root, [{"unitKey": "k1", "status": "failed", "tests": good}, {"unitKey": "k1", "status": "passed", "tests": good}]), store)
records = [json.loads(line) for line in output.splitlines()]
check("a rerun's record replaces the first", code == 0 and len(records) == 1 and records[0]["status"] == "passed", output + error)

code, output, error = run(events(root, [{"unitKey": "k1", "status": "passed", "tests": tests}]), store)
check("an inline list is taken as it is", code == 0 and json.loads(output)["tests"] == tests, output + error)

forged = "f" * 64
open(os.path.join(store, forged), "wb").write(json.dumps(tests).encode())
code, output, error = run(events(root, [{"unitKey": "k1", "status": "passed", "tests": forged}]), store)
check("a blob whose bytes hash to another name fails, exit 1", code == 1 and "hashes to" in error, output + error)

code, output, error = run(events(root, [{"unitKey": "k1", "status": "passed", "tests": "e" * 64}]), store)
check("a missing blob fails, exit 1", code == 1, output + error)

code, output, error = run(events(root, []), store)
check("a slice with no verdict fails, exit 1", code == 1 and "no verdict.decided" in error, output + error)

print("%d failed" % failures)
sys.exit(1 if failures else 0)
