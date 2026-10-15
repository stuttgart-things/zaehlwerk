# 12. Taking a point back through the ingest contract

Status: Accepted
Date: 2026-10-15

## Context

ADR-0002 makes every point safe to retry: `source` plus a counter that only
goes up, and a repeat is a duplicate. A correction has no such path (#68).

- `POST /matches/{id}/undo` carries no `source` and no `event_id`. A sender
  that retries after a lost response takes back two points, and nothing can
  tell the retry from a second correction. The button mock shows it: tick
  *API response lost*, press long once, and two points are gone.
- The ingest routes have no undo at all, and they take any `delta`. The scorer
  clamps a negative one (`max(0, points+delta)`), so `delta: -1` is not an undo:
  it does not reopen a set that was just won, and at 0 it is silently lost.

Every hardware path that is planned retries: the hub (zaehlwerk-firmware#3),
the board's offline queue (zaehlwerk-firmware#17), and corrections from the
board (zaehlwerk-firmware#41). None of them sends to this service yet, so the
contract can be fixed before they do; with only the undo route they would all
inherit the double undo the moment they send a correction.

## Decision

An ingest event can say `"action": "undo"` instead of scoring:

    { "source": "button-b", "event_id": 8, "action": "undo" }

- It goes through the same watermark as a point: same `source`, same counter,
  and a repeat is a `Duplicate`, answered 200 with the state like any other.
- Applied, it is exactly `POST /matches/{id}/undo`: it takes back the last
  event that changed the score, reopens a set whose last point it takes back,
  and after a won match it withdraws the report and the panel release as that
  route does (ADR-0007, #87).
- It names no side, because an undo takes back the last point whoever it went
  to. `player` and `half` are allowed, since a button sends what it knows, and
  ignored. A `delta` on an undo is refused: it would be two meanings in one
  event.
- With nothing to take back, the undo is recorded and changes nothing
  (`Ignored`), so a retry of it stays a duplicate rather than becoming an undo
  later.
- An undo may follow the point that won the match, in the same batch or in a
  later request that names the match: that is the mis-press at the table. A
  match ended by hand takes no events, undo included.
- A request without `match_id` goes to the running match, as a point does, and
  a won match is no longer running. So a hub that knows no match id can take
  back the match point only in the burst that scored it. Resolving an undo to
  a won match as well would change what "the current match" means for every
  device, and is left to its own decision.
- An action other than `undo` is a 400, and so is a `delta` other than 0 or 1.
  0 stays the piezo's hit that is not a point; 1 is a point.

`POST /matches/{id}/undo` stays, for the page and for people. It is not safe to
retry, and its documentation says so.

This extends ADR-0002 rather than replacing it: the event shape and the
watermark are unchanged, and an event without `action` means what it meant.

## Consequences

- A hub or a board can retry a correction as freely as a point. The firmware
  sends the new event with its next counter value and never calls the match's
  undo route (zaehlwerk-firmware#67). It needs a match id only to take back a
  match point outside the burst that scored it, as above.
- `delta: -1` and `delta: 2` stop being accepted. Nothing in this repository
  sends them; a sender outside it that did gets a 400 instead of a clamp that
  only looked like a correction.
- The scorer gains one entry point that takes an event and undoes. Deduplication
  stays where ADR-0002 put it, in the scorer, and the undo itself is the same
  code as before. The watermark is still not rolled back by an undo.
