# 5. Match phases and table halves

Status: Accepted
Date: 2026-10-13

## Context

A match used to start with its first point. The first server was chosen on
the form or defaulted to a, and there were no ends: player a was "left" on the
page for the whole match, and the hardware sent the player a point was for.

At the table it goes differently (#77):

- The players warm up first, then **play a rally for the first serve**. The
  winner serves.
- They **change ends** after every set, and in the deciding set once more when
  one of them reaches five points (ITTF law 2.14).
- In some groups **the loser of a set serves first in the next one**. That is
  not the ITTF rule (law 2.13.6), but it is the house rule on the
  Schmetterpause rules sheet.

The buttons are mounted under the table, one per half. A button knows its half,
not its player, and after a change of ends the same half stands for the other
player. zaehlwerk-firmware ADR-0006 had the board keep the half-to-player
mapping and invert it on an explicit change-of-ends control. Check-in per side
(#77) would let the server learn the mapping as well, which leaves two owners
that can disagree. When they do, a point counts for the wrong player.

## Decision

**The scorer owns the phases and the ends.** Both are rules of the match, like
service rotation, and need no player identity (invariant 5).

Phases: `warmup → serve_rally → playing ⇄ change_ends → complete`.

| Phase | A press (delta > 0) from a half means |
| --- | --- |
| `warmup` | this side is ready. When both have pressed, the match moves on |
| `serve_rally` | this side won the rally for the first serve. Nothing is scored |
| `playing` | a point, as before |
| `change_ends` | this player has arrived at the new half. When both have pressed, play goes on |

- A correction (delta ≤ 0) or a second press from the same half outside
  `playing` is `Ignored`.
- **Ends are derived, not stored**:
  - they swap after every set;
  - in the deciding set they swap again once one score reaches
    `PointsPerSet/2`.
  
  Undo therefore puts the ends back with the point it takes back. Only "both
  players still have to arrive" is stored.
- A won match does not change ends. A won set does, and the score of the
  deciding set carries on across its change.
- An undo during `change_ends` takes back the point that caused it, and with it
  the change. Confirmations are not undoable on their own. Undo is for the
  score, and the mis-press it exists for is a point given wrongly. The serve
  rally *is* undoable, because awarding it to the wrong side is that mis-press.
- **Ingest carries `half` (`left`/`right`) as an alternative to `player`.**
  Exactly one of the two is set. A half is resolved against the current ends.
  During a change of ends that is already the new end, which is where the
  confirming press comes from. `player` keeps working everywhere: the page and
  today's boards send it, and the half is derived from it.
- **All of this is opt-in per match**: `start` (`warmup`, `serve_rally`),
  `first_server: "rally"` and `change_ends`. A match that asks for none of them
  is exactly what it was before.
- **The set opener is a rule choice**: `alternate` (ITTF, the default) or
  `loser`. `SET_OPENER` sets it for the table, and a match may still choose.
- The state carries `phase`, `ends` (`[left, right]`) and `waiting` (the halves
  still to press). The panel gets `phase=` and `ends=` as the last tags, and a
  German instruction as the banner (`EINSPIELEN`, `UM DIE ANGABE`,
  `SEITENWECHSEL`, `ANGABE <name>`).

## Consequences

- zaehlwerk-firmware ADR-0006 is superseded on the board's side: a board or
  button sends its half and no longer keeps a mapping or a change-of-ends
  control. Until the firmware follows, a board that sends `player` still
  scores correctly. It just cannot follow a change of ends on its own.
- Check-in per side (#77) attaches to the halves: who checked in at the left
  half is `a`. That is the next step, together with the handover to
  Schmetterpause.
- The catcher can draw the phases without learning any rules
  (homerun2-led-catcher#132); `ends=` says who goes where.
- A press outside `playing` is a transition (`confirmed`, `warmup_over`,
  `serve_decided`, `ends_changed`), so every observer sees it. The panel keeps
  its inactivity clock going through a long warm-up, as long as someone
  presses.
- The scorer stores a little more: `warmup`, the decided first server,
  `endsPending` and the presses by half. They are cloned with the rest, so undo
  snapshots stay consistent.
