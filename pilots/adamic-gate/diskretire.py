#!/usr/bin/env python3
"""diskretire.py: a worker with a full disk is retired from its pool before it eats a queue (#0k03wz1, Oct 9 21:30Z:
three Codex instances at 398 MB free took 115 to 130 of the 222 product starts in each of five landing attempts, broke
each in 0.1 to 0.3 s and asked again, so every tests unit waited on a broken product and six attempts voided).

	pilots/adamic-gate/diskretire.py [--dry]     # every minute from launchd (com.loom.diskretire); a copy in ~/.loom/bin

It reads the events of every job that moved in the last 10 minutes (~/.loom/jobs/*/*.work/events.jsonl, and a
job directory's own *.work for the canaries), maps each unit to the machine that started it, and counts the units each
machine broke with a full-disk signature: the pilot's own "free on the instance after trimming" line, or the kernel's
"no space left on device". Two in 10 minutes retires the machine from the pool its run asked (codex unless the run
names another), by name, through the wire's retire with the reason written; the pool refuses it from its next ask. One
is weather: a unit can meet a full disk once on a machine that trims and recovers.

Retired machines go to ~/.loom/diskretire.json and are never retired twice; a restore is a human's (or Operations')
call, made on the wire. Every action is one line on stdout (launchd keeps it in ~/.loom/diskretire.log).
"""
import base64, calendar, glob, hashlib, hmac, json, os, sys, time, urllib.request

home = os.path.expanduser("~")
jobsRoot = os.environ.get("LOOM_DISKRETIRE_JOBS", os.path.join(home, ".loom", "jobs"))
statePath = os.environ.get("LOOM_DISKRETIRE_STATE", os.path.join(home, ".loom", "diskretire.json"))
wire = os.environ.get("LOOM_WIRE", "https://loom-wire.kirk-ouimet.workers.dev")
pool = os.environ.get("LOOM_DISKRETIRE_POOL", "codex")
window = 600
threshold = 2
signatures = ("free on the instance after trimming", "no space left on device")
dry = "--dry" in sys.argv[1:]


def recentEventFiles(now):
    for path in glob.glob(os.path.join(jobsRoot, "*", "*.work", "events.jsonl")):
        if now - os.path.getmtime(path) < window:
            yield path


def fullDiskBreaks(now):
    """machine -> the units it broke on a full disk inside the window, as (run, unit) pairs."""
    breaks = {}
    for path in recentEventFiles(now):
        machines = {}
        for line in open(path, errors="replace"):
            try:
                event = json.loads(line).get("event", {})
            except ValueError:
                continue
            key = (event.get("run"), event.get("unit"))
            if event.get("type") == "started":
                machines[key] = event.get("machine")
            elif event.get("type") == "output" and any(signature in event.get("text", "").lower() for signature in signatures):
                stamp = event.get("time", "")
                try:
                    seconds = calendar.timegm(time.strptime(stamp[:19], "%Y-%m-%dT%H:%M:%S"))
                except ValueError:
                    continue
                machine = machines.get(key)
                if machine and now - seconds < window:
                    breaks.setdefault(machine, set()).add(key)
    return breaks


def coordinatorToken():
    """A coordinator token for ten minutes, signed as protocol/token.go signs: the secret never leaves this process."""
    secret = open(os.path.join(home, ".loom", "token-secret")).read().strip().encode()
    encode = lambda raw: base64.urlsafe_b64encode(raw).rstrip(b"=").decode()
    claims = json.dumps({"run": "loom-diskretire", "scope": "coordinator", "expires": int(time.time()) + 600}, separators=(",", ":"))
    first = encode(claims.encode())
    return first + "." + encode(hmac.new(secret, first.encode(), hashlib.sha256).digest())


def retire(machine, reason):
    request = urllib.request.Request(
        "%s/pools/%s/retire" % (wire, pool),
        data=json.dumps({"worker": machine, "reason": reason}).encode(),
        headers={"Authorization": "Bearer " + coordinatorToken(), "Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(request, timeout=20) as response:
        return response.status


def main():
    now = time.time()
    state = json.load(open(statePath)) if os.path.exists(statePath) else {}
    for machine, units in sorted(fullDiskBreaks(now).items()):
        if len(units) < threshold or machine in state:
            continue
        sample = ", ".join(sorted(unit for _, unit in units)[:4])
        reason = "disk full: broke %d units in 10 minutes (%s), retired by diskretire.py (#0k03wz1)" % (len(units), sample)
        stamp = time.strftime("%H:%M:%S", time.gmtime())
        if dry:
            print("%s would retire %s from %s: %s" % (stamp, machine, pool, reason))
            continue
        try:
            status = retire(machine, reason)
        except Exception as error:  # the next minute tries again; nothing is remembered for a failed call
            print("%s couldn't retire %s from %s: %s" % (stamp, machine, pool, error))
            continue
        state[machine] = {"retired": int(now), "units": len(units), "status": status}
        print("%s retired %s from %s (%s): %s" % (stamp, machine, pool, status, reason))
    if not dry:
        json.dump(state, open(statePath + ".tmp", "w"), indent=2)
        os.replace(statePath + ".tmp", statePath)


main()
