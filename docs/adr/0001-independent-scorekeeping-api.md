# 1. Independent scorekeeping API

Status: Accepted
Date: 2026-09-05

## Context

Live table tennis scores need to reach three places: the RGB LED matrix driven
by homerun2-led-catcher, a live tab in Schmetterpause, and eventually a match
history for TTR calculation. Points arrive from wireless buttons, piezo sensors
or a phone acting as scorekeeper.

Three places could own the running score:

- **Schmetterpause.** It already owns players, TTR and tournaments. But it would
  gain an ingest surface for hardware, and the matrix would then depend on it
  for something that is not its domain.
- **The led-catcher.** It is a display sink with no notion of state. Teaching it
  set logic would make one of several interchangeable catchers special.
- **A separate service.**

## Decision

A separate service, `zaehlwerk-api`, owns the state of the running match: set
logic, service changes, undo, and deduplication of incoming events.

It is the only writer of that state and fans out to its consumers — as a
homerun pitcher on the `tabletennis` stream, and over SSE for browsers.

Schmetterpause keeps players, TTR, tournaments and history, and receives a
finished result at match end.

## Consequences

- The matrix and the Schmetterpause live tab are fed by the same source and
  cannot disagree about the score.
- The led-catcher stays a generic sink. Table tennis points are just another
  event source with its own `system` label, filtered by a display rule.
- One more service to deploy and operate.
- Schmetterpause needs no backend work for the live tab — it embeds an SSE
  endpoint served by zaehlwerk-api.
- Set logic lives in one place rather than in firmware, so rule changes do not
  require reflashing.