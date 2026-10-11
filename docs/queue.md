# The queue and main's tip

The Queue (`wire/source/Queue.ts`) decides from its own hash-chained log, and it never asks git whether main moved. It records `main.green` or `main.red` from a witness of main. Before it does, it needs to know which commit main is on, and it learns that from readings (#6gj7n9p).

## Readings of main's head

- **Facts.** git's facts carry `mainHead`, which is main's head as origin holds it (the bridge asks `git ls-remote origin refs/heads/main`, never a local ref that a failed fetch could leave old), and `asOf`, which is the Queue's seq when the reading began. The bridge reads `GET /head` before it asks git. The Queue's own GitHub path notes its seq before it asks. The two fields come together or not at all, and an `asOf` past the log is refused.
- **Landings.** A landing's `main` is a reading too.
- **Ranking.** A landing at seq s ranks 2s. A fact read after seq s ranks 2s + 1.
  - The newest rank is main's tip.
  - A fact whose reading began before a landing was logged never displaces that landing. A fact whose reading began after does.
  - Two readings of the same rank that disagree leave the tip unknown until a newer one arrives.
- **Witnesses.** A witness records `main.green` or `main.red` only when two things hold: its own facts read its sha as main's head, and that sha is still the tip.
  - A witness of an older commit records nothing.
  - So does a witness of a head that main has since moved past.
  - So does a witness whose facts came from a bridge that predates `mainHead`.
- **When main's head can't be read.**
  - If the bridge can't read the Queue's seq or origin's main, or any git command it runs exits with a code its caller doesn't allow, it posts no facts that tick, so the change stays unchecked and is read again on the next tick. A sha's fetch counts as "origin has no such sha" only when origin says so (`not our ref`).
  - If the Queue's GitHub path can't read main's head, it answers 503 and logs nothing.
  - Either way, no change is checked without the head, because a witness checked without it would be silent about main forever.

## The window it leaves

Readings are ordered against the Queue's log, not against GitHub. Two readings can overlap with a push from outside Loom between their git reads. If the one that began later read git first, the tip briefly holds the older head, until the next reading or landing. In that window, a witness of the older head could record `main.green` or `main.red` for a commit main has just left.

Today only the bridge reads git, and it reads one change at a time under its lock, so its readings never overlap. A second reader would need its readings ordered by git's ancestry. The Queue has no git to ask that.

## A future built again on its tree

A re-witness of a sha builds a new future on the same tree. Run ids are `future-<tree>-<attempt>`, so the new future continues the attempt count. Its `future.built` logs `attemptsBefore`. The planned listing names `firstAttempt`, and the judge carries a pass only from `firstAttempt` on.

The Queue refuses any batch on such a future whose run, or a run a record says it carried (test or phase), is below `firstAttempt`. A judge or placer from before `firstAttempt` would carry an earlier future's passes. Deploy them first, then the Worker.

## Submodule pins

Every runner fetches every submodule pin with no key, so the bridge proves each pin the same way before the queue clears a change (#yt2jw5q). git's facts carry `pins`: every gitlink in the change's tree at any depth (adamic's `cohere`, cohere's `cohere/TypeScript`), each as `{path, url, sha, fetchable, reason?}`.

- **The url** follows one rule, shared with the runner's `prepare.sh` (`pinUrl`; `queuebridge/pins_test.go` runs both on one table). It must be a repository on github.com over https, as `owner/name`, or `git@github.com:owner/name`, which both read as https. Any other url is refused by name and never fetched: another host (a house address among them), ssh, a credential in the url, or a relative path.
- **fetchable** comes from a keyless fetch of that sha from that url, shallow and blob-less, into a scratch store that is removed afterwards.
  - Every git the bridge runs gets an environment built from nothing, not added to the process's. So no inherited `GIT_CONFIG_COUNT` or `GIT_CONFIG_PARAMETERS` reaches it, no global or system settings (no `insteadOf`, no credential helper), and `credential.helper=`, `http.extraHeader=` and `http.sslVerify=true` go on every command line, over anything the clone's own configuration says.
  - `HOME` is `/dev/null`, so no `~/.netrc` is read. No prompt runs, and `GIT_SSH_COMMAND=false` stops ssh.
  - The bridge's own clone's origin follows the pins' rule (github.com over https, no user or password in the url; a local path in tests). Any other origin is refused before anything is fetched.
  - A pin that fetches is followed into its own tree, four levels deep (`PinDepth`). A pin nested deeper is listed unfetchable, named, and never fetched.
- **Unfetchable, with our reason** (never git's words): a url the rule refuses; a path `.gitmodules` doesn't name; a commit GitHub won't give a fetch with no key (`not our ref`, an unadvertised object); a repository that isn't public (not found, a login asked for).
- **Unknown.** Any other failure says nothing about the pin: throttling (403, 429), GitHub's 5xx, a host that won't resolve, a timeout. The bridge posts no facts that tick, and the change waits. It is never refused or passed on a guess.

`prepare.sh` holds the same rule at every depth on a runner: its tree's urls before anything is fetched, and every nested one once its parent is checked out (`prepare.sh pin-urls <tree>`). It's a second line, since the queue refused any change that breaks the rule before a runner saw it.

The queue refuses a change whose pins aren't all fetchable before any future is built. The refusal is logged and names each unfetchable pin. Facts without `pins` are no clearance, like facts without `historyPaths`. So the queue's pins rule ships only once the bridge that posts pins is the one running.

`loom queue-bridge pins [--repository <clone>] <sha>` prints a commit's pins as the bridge would post them. It exits 1 when one is unfetchable and 3 when git or a remote couldn't say.
