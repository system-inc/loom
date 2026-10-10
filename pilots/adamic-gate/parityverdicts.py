#!/usr/bin/env python3
"""parityverdicts.py <events.jsonl>: one parity change's verdict records out of its event slice, for boxparity.py.

GET /changes/<change>/events (contract 5 on #ykg8g6k) returns the change's slice of the log as JSON lines. Judge logs
each decided unit as a verdict.decided event whose data is the whole verdict record (contract 3, tests included). This
prints those records one per line, the latest per unitKey when a unit was decided twice (a rerun alone replaces the
first attempt's record), so boxparity.py reads exactly one verdict per unit.

	curl -sS "$pipeline/changes/<change>/events" -H "Authorization: Bearer $(cat <token file>)" > events.jsonl
	pilots/adamic-gate/parityverdicts.py events.jsonl > verdicts.jsonl
	pilots/adamic-gate/boxparity.py <box record dir> verdicts.jsonl

Exit 1 when the slice holds no verdict.decided event, since an empty side would compare as everything absent.
"""

import json
import sys


def main():
    if len(sys.argv) != 2:
        print(__doc__.strip().splitlines()[0])
        return 2
    latest = {}
    for line in open(sys.argv[1], errors="replace"):
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
    for _, verdict in sorted(latest.values(), key=lambda pair: pair[0]):
        print(json.dumps(verdict, sort_keys=True, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    sys.exit(main())
