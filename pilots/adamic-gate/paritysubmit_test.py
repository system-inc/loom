#!/usr/bin/env python3
"""paritysubmit_test.py [<paritysubmit.py>]: paritysubmit.py on planted records in a scratch repository. The body pins
sha, base, the diff and every planned test (products included); a fixture-subset oracle run and a base that isn't an
ancestor are refused. The mutant that drops product tests from the selection must fail:

	python3 pilots/adamic-gate/paritysubmit_test.py
	sed 's/if unit.get("product") and/if False and/' paritysubmit.py > m.py && python3 paritysubmit_test.py m.py   # fails
"""

import json
import os
import subprocess
import sys
import tempfile

here = os.path.dirname(os.path.abspath(__file__))
paritysubmit = sys.argv[1] if len(sys.argv) > 1 else os.path.join(here, "paritysubmit.py")
failures = 0
package = "github.com/system-inc/adamic/internal/oracle"


def check(name, condition, output):
    global failures
    print(("PASS " if condition else "FAIL ") + name + ("" if condition else ": " + output))
    failures += 0 if condition else 1


def git(repository, *arguments):
    return subprocess.run(["git", "-C", repository] + list(arguments), check=True, capture_output=True, text=True).stdout.strip()


def commit(repository, path, text):
    open(os.path.join(repository, path), "w").write(text)
    git(repository, "add", path)
    git(repository, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", path)
    return git(repository, "rev-parse", "HEAD")


def record(root, sha, base, oracle=None):
    directory = tempfile.mkdtemp(dir=root)
    json.dump({"sha": sha, "base": base, "packages": [package], "oracle_selection": oracle,
               "test_outcomes": [{"package": package, "test": "TestA", "status": "passed"}, {"package": package, "test": "TestB", "status": "failed"}],
               "units": [{"package": package, "test": "TestProduct_X", "product": True}, {"package": package, "test": "TestA", "product": False}]},
              open(os.path.join(directory, "fast.json"), "w"))
    return directory


def run(*arguments):
    result = subprocess.run([sys.executable, paritysubmit] + list(arguments), capture_output=True, text=True)
    return result.returncode, result.stdout, result.stderr


root = tempfile.mkdtemp()
repository = os.path.join(root, "repo")
os.mkdir(repository)
git(repository, "init", "-q")
base = commit(repository, "a.txt", "a")
sha = commit(repository, "b.txt", "b")

code, output, error = run(record(root, sha, base), repository, "release")
body = json.loads(output) if code == 0 else {}
check("the body pins sha, base and owner, in parity mode", body.get("sha") == sha and body.get("base") == base and body.get("owner") == "release" and body.get("parity") is True, output + error)
check("paths are the diff base..sha", body.get("paths") == ["b.txt"], output)
check("select names every planned test, products included", body.get("select", {}).get("tests") == {package: ["TestA", "TestB", "TestProduct_X"]}, output)
check("select's packages cover the record's", body.get("select", {}).get("packages") == [package], output)

code, output, error = run(record(root, sha, base, oracle={"whole": False, "reason": "fixtures only"}), repository, "release")
check("a fixture-subset oracle run is refused, exit 1", code == 1 and "fixture subset" in error, output + error)

code, output, error = run(record(root, base, sha), repository, "release")
check("a base that isn't an ancestor is refused, exit 1", code == 1 and "not an ancestor" in error, output + error)

print("%d failed" % failures)
sys.exit(1 if failures else 0)
