# Cutover, the one switch between the old path and the lander

`cutover/cutover.sh` flips adamic's main between two states and reads which one it is in (#95gw7a9, rollback #sjywpc5, today's house #mxg7y3a). Ruleset 24823318 (loom-lander-only-main) restricts update, deletion and non-fast-forward on refs/heads/main, and its only bypass is the actor type DeployKey, so which deploy keys can write decides who lands.

- **On.** The ruleset is active, in its shape (exactly update, deletion and non_fast_forward, bypassed by DeployKey alone, over refs/heads/main alone), and GitHub lists it on main. The lander's key (Workshop's `~/.ssh/loom_lander`, title "loom lander (workshop only)") has write; every other deploy key is read-only, the gate key (Cloud's `~/.ssh/adamic_deploy`, `gateKey`, 165969193) among them. The pusher's timer on Workshop (`loom-pusher.timer`, `--timer` for another unit, such as the Go pusher's of #nvq0tc8) is enabled and active, so a reboot keeps landing. Only a hand turns it on, with Kirk there for the first landing: no install ever enables or starts it (`loom push install` writes its units and leaves the timer as found, and `loom install-units` never touches it), so until then it stays disabled and dead on purpose.
- **Off.** The ruleset is disabled and GitHub lists none of its rules on main, which is all a person's own credential needs to push main: no Mac launch agent pushes main any more, so the old path today is Kirk pushing by hand. Only once the ruleset reads back disabled is the lander quieted: the pusher's timer stopped and disabled and its service (a pass in flight) stopped, so nothing lands. The lander's key stays, with write and unused; `off --delete-lander-key` deletes it too, again only after the read-back, and then GitHub refuses the lander as well. A ruleset that won't disable leaves the lander's key and timer as they are, so main always keeps a writer. On main the ruleset is disabled by its id even when it can't be read.

`check` exits 0 on either, 1 when mixed, and 3 when any part can't be read (Workshop not answering, a GitHub read failing, systemd not answering, or a `--timer` Workshop doesn't have), so an unreadable switch never reads as on or off. `prove` pushes an empty commit on the tip with each identity the state must refuse and passes only on GitHub's own refusal of that identity for that reason. A deploy key's probe first checks, where the key lives, that its ssh alias reaches github.com with a private key whose public half is the key under test, so a missing key file or alias is never read as GitHub refusing the key, and a GH013 counts only while no other ruleset applies to the ref. On, Kirk's credential from the Mac refused by GH013 and Cloud's gate key refused as read only; off with the lander's key deleted, the lander refused with Permission denied (publickey). Off with the lander's key kept has nothing to refuse, and says so: its guarantee is the timer `check` read. `accept` runs only in a rehearsal: the identity the state lets through (the lander when on, Kirk when off) pushes a probe, and it passes only when the push landed and the ref reads back as the probe. A probe that never reached GitHub, by an unreachable host or a credential that never authenticated, fails both.

The tests are `cutover/cutover_test.sh`: stubbed gh, ssh, git and systemctl over a model of the repository and the two hosts, each subcommand's success and failure, then each planted mutant, then shellcheck when installed.

## Rehearsal, main untouched

A rehearsal is `--ref <branch> --ruleset <id>`, the two together and neither of them main's. It flips only the scratch ruleset and reads everything else: deploy keys are repo-wide, so a scratch `off` that deleted the lander's key would delete the real one, and the pusher lands main. Before any write, `cutover.sh` checks that the ruleset targets exactly `refs/heads/<branch>`, so a ruleset over main is never flipped by a rehearsal. The keys need no change for it: today they are already in on's shape (the lander's writes, the gate key is read-only).

Every step that creates or removes the scratch branch and ruleset is Kirk's; the switch itself is run from the Mac, in a checkout of this repository, with `gh` signed in as Kirk.

1. **The scratch branch**, on main's tip:
   `gh api -X POST repos/system-inc/adamic/git/refs -f ref=refs/heads/loom-rehearsal -f sha="$(gh api repos/system-inc/adamic/git/ref/heads/main --jq .object.sha)"`
2. **The scratch ruleset**, main's shape over the scratch branch, disabled. Note the id it prints.
   ```
   gh api -X POST repos/system-inc/adamic/rulesets --input - --jq .id << 'EOF'
   {"name": "loom-rehearsal-only", "target": "branch", "enforcement": "disabled",
    "conditions": {"ref_name": {"include": ["refs/heads/loom-rehearsal"], "exclude": []}},
    "rules": [{"type": "update"}, {"type": "deletion"}, {"type": "non_fast_forward"}],
    "bypass_actors": [{"actor_type": "DeployKey", "bypass_mode": "always"}]}
   EOF
   ```
3. `scratch="--ref loom-rehearsal --ruleset <id>"`, and `cutover/cutover.sh check $scratch`: off.
4. `cutover/cutover.sh accept $scratch`: Kirk's credential lands a probe on loom-rehearsal.
5. `cutover/cutover.sh prove $scratch`: off keeps every key in a rehearsal, so nothing must be refused, and it says so.
6. `cutover/cutover.sh on $scratch`: only the scratch ruleset goes active; it reads on.
7. `cutover/cutover.sh prove $scratch`: Kirk refused by GH013, Cloud's gate key refused as read only, loom-rehearsal where it was.
8. `cutover/cutover.sh accept $scratch`: the lander's key, through `github-lander` on Workshop, lands a probe, and it reads back.
9. `cutover/cutover.sh off $scratch`, then `check $scratch`: off. `accept $scratch` again: Kirk lands, the rollback works.
10. **Main, read only**: `cutover/cutover.sh check`. It reads main, the keys and Workshop's timer and changes nothing; today it reads off.
11. **Clean up**: `gh api -X DELETE repos/system-inc/adamic/rulesets/<id>`, then `gh api -X DELETE repos/system-inc/adamic/git/refs/heads/loom-rehearsal`.

What a rehearsal can't reach, the keys and the timer, the tests cover against their stubs, and step 10 reads on the real house.

## Cutover and rollback, Kirk's

- **Cutover.** `cutover/cutover.sh on`, then `check` (on) and `prove`. The first landing after it is the lander's acceptance.
- **Rollback.** `cutover/cutover.sh off`, then `check` (off) and `prove`. `off --delete-lander-key` when GitHub, not only the stopped timer, should refuse the lander; `on` adds the same public key back.
