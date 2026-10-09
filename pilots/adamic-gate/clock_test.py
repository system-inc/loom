#!/usr/bin/env python3
"""clock_test.py [<clock.py>]: clock.py on planted jobs. A green stops its clock at its verdict; a void and a job with no
verdict run to now and count. The mutant that drops jobs with no verdict (the dishonest clock) must fail:

	python3 pilots/adamic-gate/clock_test.py
	sed 's/            clocks.append(now - submit)/            pass/' clock.py > m.py && python3 clock_test.py m.py   # fails
"""

import os
import subprocess
import sys
import tempfile

here = os.path.dirname(os.path.abspath(__file__))
clock = sys.argv[1] if len(sys.argv) > 1 else os.path.join(here, "clock.py")
failures = 0


def check(name, condition, output):
    global failures
    print(("PASS " if condition else "FAIL ") + name + ("" if condition else ": " + output))
    failures += 0 if condition else 1


def plant(jobs, sha, submit, verdict=None, at=None):
    path = os.path.join(jobs, sha + ".json")
    open(path, "w").write('{"branch": "cloud/land-x", "sha": "%s"}' % sha)
    os.utime(path, (submit, submit))  # an mtime before the birth time moves the birth time back on macOS
    if verdict:
        open(os.path.join(jobs, sha + ".verdict"), "w").write(verdict + "\n")
        os.utime(os.path.join(jobs, sha + ".verdict"), (at, at))


jobs = tempfile.mkdtemp()
now = 1000000000
plant(jobs, "a" * 40, now - 9000, "green: a passed", now - 8400)  # 600 s to its verdict
plant(jobs, "b" * 40, now - 8000, "void: b broke", now - 7000)  # void: runs to now, 8000 s
plant(jobs, "c" * 40, now - 1000)  # no verdict yet: 1000 s
plant(jobs, "d" * 40, now - 200000, "green: d", now - 199000)  # outside the 24 hours
plant(jobs, "e" * 40, now - 5000, "void: e broke", now - 4900)  # withdrawn at now - 4000: answered at 1000 s, apart
open(os.path.join(jobs, "e" * 40 + ".withdrawn"), "w").write("by owner: superseded\n")
os.utime(os.path.join(jobs, "e" * 40 + ".withdrawn"), (now - 4000, now - 4000))
environment = dict(os.environ, LOOM_CLOCK_JOBS=jobs, LOOM_CLOCK_GATE=os.path.join(jobs, "no-gate"))
output = subprocess.run([sys.executable, "-I", clock, "--now", str(now)], capture_output=True, text=True, env=environment).stdout
check("counts-every-job-in-the-window", "4 jobs" in output, output)
check("void-and-unanswered-run-to-now", "2 with no verdict (oldest 133 min, bbbbbbbbbbbb)" in output, output)
check("p50-includes-the-running", "p50 17 min" in output, output)
check("p90-is-the-void", "p90 133 min" in output, output)
check("withdrawn-counted-apart", "1 withdrawn by their owners" in output and "2 with no verdict" in output, output)
print("failures: %d" % failures)
sys.exit(1 if failures else 0)
