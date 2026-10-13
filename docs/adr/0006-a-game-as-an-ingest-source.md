# 6. A game as an ingest source

Status: Accepted
Date: 2026-10-13

## Context

The chain can be played without hardware today, but only by mocks that
misbehave on purpose and never know who really won a rally: the piezo board
(`tools/chain-mock/piezo`) draws a half at random, and the table mock's buttons
are pressed by a person. Neither can say "zaehlwerk is wrong here", because
neither knows what right would be.

A table tennis game can (#78). Its rallies are decided before anything is
sent, so it holds a truth to compare zaehlwerk's score with: whether the phases
of ADR-0005 run in the right order, whether a half lands on the player standing
there after a change of ends, and whether a retried event is discarded
(ADR-0002). The same game keeps the panel and the page alive at a fair or in
the office, which gives it a life outside development: an image, a health check
and a deployment, like anything else that runs unattended.

Three things make that dangerous if done carelessly:

- A match that is reported to Schmetterpause becomes a `pending` result with
  `entered_via = scoreboard`. A game must never create one: it would end up in
  TTR and rankings, and skew the measurement of Schmetterpause ADR-0015.
- zaehlwerk resolves hardware ingest, the page and `/live/stream` against the
  **current** match, the most recently created one still running. A game that
  starts a match while somebody is playing takes the table away from them.
- Two instances reporting the same game are two sources, and every point
  counts twice.

## Decision

**zaehlwerk-pong is its own binary and its own image**,
`cmd/zaehlwerk-pong` → `ghcr.io/stuttgart-things/zaehlwerk-pong`. It lives in
this repository and this Go module, is released with zaehlwerk under the same
tag, and its e2e runs against the zaehlwerk built from the same commit.

**It is a pure HTTP client of zaehlwerk.** It imports wire types —
`scorer.State` and the names of players, halves and phases — and nothing that
holds state: no scorer, no registry, no hub. It does not change zaehlwerk; if
it needs something zaehlwerk does not say, that is a question for zaehlwerk's
API, not logic copied into the game.

**It starts its own match and never reports one.** `POST /matches` with
`start: "warmup"`, `change_ends: true`, the best-of from its configuration and
free-text names. It never sends a player id or an operator, so a Pong match
has no handover and never reaches Schmetterpause. It does not join a match
either: if `/live/stream` shows a current match, it does not start, and a
current match carrying `home_id`/`away_id` is a real one it must not touch.

**It scores through the browser's door.** A rally won is
`POST /ingest/web` with `match_id`, `half` (`left`/`right`) and `delta: 1`, as
invariant 6 asks of any new way of scoring. `match_id` keeps a stale game from
scoring into somebody else's match. `half` rather than `player`, because a game
that knows who stands where only because zaehlwerk told it would otherwise be
checking zaehlwerk against itself.

**`source` is per game, `event_id` is the rally number.** The source is
`pong-<game id>`. Every report the game makes — a rally won, a side pressing
ready — takes the next number of the game's own counter, so the rally's number
and its event id are the same thing, and a retry anywhere along the way is
discarded by ADR-0002 end to end.

**Exactly one reporter per game.** The instance that started the match is the
only one that sends for it. A second browser watching the same game watches; it
does not report.

**It derives nothing.** Phase, `ends`, `waiting` and `serving` come from
zaehlwerk's answer or from `/live/stream`. The bots stand where zaehlwerk sent
them at the last change of ends and walk only when the phase says
`change_ends`; who serves is whoever zaehlwerk says serves.

**It compares its truth with zaehlwerk after every rally.** The truth is who
really won each rally. It is compared with the points zaehlwerk has given each
player over the whole match, summed from `completed_sets` and the set in
progress: arithmetic over the state, not the rules of a set. Who won the serve
rally is compared with who serves, and the ends with where the bots stand.

**The game is an exception to invariant 8, in its own binary.** A game loop at
60 frames a second is not something htmx can carry, so the game itself will be
canvas and hand-written JavaScript in one file. The page around it — lobby,
banner, score — stays htmx and SSE. zaehlwerk's own surface is not affected:
invariant 8 holds there exactly as before, and the JavaScript ships in a
different image.

## Consequences

- The first slice is headless: two bots, a rally drawn by skill rather than
  played, and an HTTP route to start one. The browser game, fault injection,
  attract mode and the button check build on this and do not change the
  contract above.
- A Pong match is visible wherever the table is: on the panel, on the scoring
  page and on Schmetterpause's start page, which follows `/live/stream`. Only
  the free-text names say it is a game. Marking a match as a demo would be an
  API change and is left open (#78).
- A button pressed under the table during a Pong match scores into it, because
  it is the current match. The truth then disagrees with zaehlwerk, which is
  correct — something nobody accounted for did score.
- zaehlwerk-pong adds an image to release, scan and clean up, built the same way
  as the piezo board and the buttons.
