# 7. A grace period before reporting

Status: Accepted
Date: 2026-10-14

## Context

Undo after a won match is supported on purpose: the match point pressed for
the wrong player and corrected a second later is the mis-press that really
happens at a table, and `TestUndoAfterTheMatchIsWon` calls it the case undo
exists for. Two observers acted on `match_won` at once and neither took its
action back (#87).

**The report.** ADR-0004 posts the result from an observer on `match_won`. A
wrong match point therefore reached Schmetterpause before the undo. The right
point won the match a second time, but a match already reported is left alone,
so the right result never followed: Schmetterpause held the wrong one.

**The panel.** ADR-0003 gives the panel back when a match is won. After the
undo the match was running again, and the panel showed the idle streams while
play went on, because only creating a match takes it.

A sent result could be taken back only by a route in Schmetterpause that
withdraws or replaces it. There is none, and this decision does not ask for
one.

## Decision

**The report waits a grace period after `match_won`, and an undo in it stops
the report.** `SCHMETTERPAUSE_REPORT_DELAY` sets it, `15s` by default; `0` sends
at `match_won` as before. A value that is not a duration fails the start rather
than being ignored, because a wrong guess here sends a result early and a sent
result cannot be taken back.

- The reporter starts a timer per match at `match_won`, outside the observer:
  observers run under the scorer's lock and must not block. A second
  `match_won` for the same match, an undo and a new win, starts it again.
- When the timer runs out, the handover reads the score as it stands then and
  sends nothing for a match that is no longer complete. So what arrives is the
  result that holds once the grace period is over.
- An undo that reopens a match in its grace period stops the timer and logs
  `report withheld, the match was reopened`.
- The retry button on the page sends at once and stops a running timer.
  Whoever presses it gives up the rest of the grace period on purpose.
- At shutdown (`SIGTERM`) the results still waiting are sent at once, within
  the shutdown timeout the process already has.
- The page says, during the grace period, at what time the result goes and
  that an undo still takes it back.

**Delaying, not taking back.** Taking back would need a new route in
Schmetterpause, and a result there stays pending until one of the two players
confirms it (Schmetterpause ADR-0015), so a short wait costs nobody anything.
The wait needs nothing on the other side and keeps the coupling the one-way
post ADR-0004 decided on.

**The panel is taken back by the undo.** An undo that reopens a won match takes
the panel again, but only if no other match has been started since. If one has,
the panel is that match's (ADR-0003) and nothing happens. The release at
`match_won` stays immediate: the panel can be taken back at any time, so it
needs no grace period. The report cannot, and that is the whole reason for this
one.

## Consequences

- A result reaches Schmetterpause the grace period later than the last point.
  It then waits there for a confirmation that takes far longer, so nobody
  waits on the delay.
- **An undo after the grace period is still not reported.** The result in
  Schmetterpause is the first one and has to be rejected or corrected there.
  The page says so for as long as the score differs from what was sent, with
  the sets Schmetterpause holds. A match won again after that is not sent a
  second time.
- **A `SIGKILL` in the grace period loses the result.** This is the loss
  ADR-0004 already accepts for an unreachable Schmetterpause, with a slightly
  larger window. The timers live in memory like everything else here
  (invariant 2), and a shutdown that is given its signal sends them.
- The result card asks the server for itself every second while the result
  waits or is on its way, because the end of a grace period is no transition
  and the live stream has nothing to carry it on. It stops once there is an
  outcome.
- The setting is one more variable in the table, with a default that suits a
  table where the match point is pressed by hand.
- This supplements ADR-0003 and ADR-0004. It does not replace either.
