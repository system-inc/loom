# The queue and main's tip

The Queue (`wire/source/Queue.ts`) decides from its own hash-chained log, and it never asks git whether main moved. It records `main.green` or `main.red` from a witness of main. Before it does, it needs to know which commit main is on, and it learns that from readings (#6gj7n9p).

## Readings of main's head

- **Facts.** git's facts carry `mainHead`, which is origin/main as the reader fetched it, and `asOf`, which is the Queue's seq when the reading began. The bridge reads `GET /head` before it asks git. The Queue's own GitHub path notes its seq before it asks. The two fields come together or not at all, and an `asOf` past the log is refused.
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
  - If the bridge can't read the Queue's seq or origin/main, it posts no facts that tick, so the change stays unchecked and is read again on the next tick.
  - If the Queue's GitHub path can't read main's head, it answers 503 and logs nothing.
  - Either way, no change is checked without the head, because a witness checked without it would be silent about main forever.

## The window it leaves

Readings are ordered against the Queue's log, not against GitHub. Two readings can overlap with a push from outside Loom between their git reads. If the one that began later read git first, the tip briefly holds the older head, until the next reading or landing. In that window, a witness of the older head could record `main.green` or `main.red` for a commit main has just left.

Today only the bridge reads git, and it reads one change at a time under its lock, so its readings never overlap. A second reader would need its readings ordered by git's ancestry. The Queue has no git to ask that.

## A future built again on its tree

A re-witness of a sha builds a new future on the same tree. Run ids are `future-<tree>-<attempt>`, so the new future continues the attempt count. Its `future.built` logs `attemptsBefore`. The planned listing names `firstAttempt`, and the judge carries a pass only from `firstAttempt` on.

The Queue refuses any batch on such a future whose run, or a run a record says it carried (test or phase), is below `firstAttempt`. A judge or placer from before `firstAttempt` would carry an earlier future's passes. Deploy them first, then the Worker.
