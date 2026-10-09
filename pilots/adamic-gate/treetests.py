#!/usr/bin/env python3
"""treetests.py: every top-level Go test in the tree at a sha, one "<import path> <test>" per line, read from git
without building anything (#2en3b4t: the plan's test list comes from the tree it plans, not from the last green
record, which test-only landings outrun by dozens of splits an hour).

	pilots/adamic-gate/treetests.py <sha> [--repository <adamic checkout>] > tree-tests.txt   # a copy in ~/.loom/bin

A test is a `func TestName(t *testing.T)` at the top of a *_test.go file, in a package of the main module: not under
a testdata/ directory, a node_modules/ one, or a nested module (a directory with its own go.mod, such as the cohere
submodule's). A file whose build constraints keep it out of the default build is listed anyway; its tests are planned
and run no test, which costs a unit nothing but its setup.
"""

import argparse
import os
import re
import subprocess
import sys

module = "github.com/system-inc/adamic"
testFunction = re.compile(r"^func (Test[A-Za-z0-9_]*)\(\w+ \*testing\.T\)", re.M)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("sha")
    parser.add_argument("--repository", default=os.path.expanduser("~/Projects/system/adamic-gate"))
    arguments = parser.parse_args()
    listing = subprocess.run(["git", "-C", arguments.repository, "ls-tree", "-r", "--full-tree", "--name-only", arguments.sha],
                             check=True, capture_output=True, text=True).stdout.splitlines()
    nested = [os.path.dirname(path) for path in listing if path.endswith("/go.mod")]
    files = [path for path in listing if path.endswith("_test.go")
             and not any(part in ("testdata", "node_modules", "vendor") for part in path.split("/"))
             and not any(path.startswith(directory + "/") for directory in nested)]
    tests = set()
    # git grep over the whole tree at the sha in one call, each line <sha>:<path>:<line>:<text>; the files that count
    # are filtered after it.
    counted = set(files)
    grep = subprocess.run(["git", "-C", arguments.repository, "grep", "-n", "--full-name", "-E", r"^func Test[A-Za-z0-9_]*\([A-Za-z0-9_]+ \*testing\.T\)",
                           arguments.sha, "--", "*_test.go"], capture_output=True, text=True)
    if grep.returncode not in (0, 1):
        sys.exit("treetests: git grep failed: " + grep.stderr.strip())
    for line in grep.stdout.splitlines():
        _, path, _, text = line.split(":", 3)
        match = testFunction.match(text)
        if match and path in counted:
            directory = os.path.dirname(path)
            tests.add("%s %s" % (module + ("/" + directory if directory else ""), match.group(1)))
    for test in sorted(tests):
        print(test)
    return 0


if __name__ == "__main__":
    sys.exit(main())
