# 6. A game as an ingest source

Status: Accepted
Date: 2026-10-13

> Supplemented by [ADR-0008](0008-demo-matches.md): zaehlwerk knows
> a Pong match as a demo, a real match ends it, and a demo is refused while a
> real match runs.

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
  API change and needs an ADR of its own (#98).
- A button pressed under the table during a Pong match scores into it, because
  it is the current match. The truth then disagrees with zaehlwerk, which is
  correct — something nobody accounted for did score.
- A game that loses `/live/stream` ends its match, as if displaced: it can no
  longer see another match become current, and playing on blind would put a
  match that is not the table's on the panel. Reconnecting is left for later.
- Faults sit between the truth and the ingest and never touch the truth. The
  game keeps what each injected fault should do to zaehlwerk's score, and a
  divergence it explains is expected; any other is a finding. A run with
  faults is good when it has no findings, not when it has no divergences.
- A `counter_reset` fault makes `event_id` and the rally number part: the
  counter starts again at 1, the rally number goes on. It also costs a quiet
  minute per reset, because zaehlwerk reads a fallen counter as a restart
  only after `scorer.DefaultRestartQuietPeriod`. The game copies that number
  rather than asking for it; a test fails if the two drift apart.
- In the browser the page decides rallies and the service does everything
  else: the truth, the faults, the reports. A game has one driver — two bots
  headless, or a browser — and the same code behind it, so a fault behaves
  the same whoever decided the rally. The page reports each rally to the
  service with its own number, which makes a retry from the page idempotent;
  the event id zaehlwerk sees stays the service's counter, because injected
  faults add presses of their own.
- One reporter per game is a token handed to the page that started it, in
  the URL fragment from the lobby. A second tab watches. A browser game nobody
  presses in for ten minutes (`PONG_BROWSER_IDLE`) ends its match, so a closed
  tab does not hold the table.
- The canvas is one embedded file of hand-written JavaScript, a little over
  400 lines rather than the 300 #78 hoped for: bots, touch, sound and the
  phases cost more than the ball. Everything around it is htmx and SSE.
- zaehlwerk's ingest answer is the state and no outcome, so the game tells a
  discarded or ignored press by the state staying the same. That is enough for
  every fault here, and an outcome on the wire would be an API change.
- zaehlwerk-pong adds an image to release, scan and clean up, built the same way
  as the piezo board and the buttons. Since attract mode it runs unattended, so
  its image is scanned like zaehlwerk's, and the KCL module can deploy it next
  to zaehlwerk as an optional part.
- An attract match never ends a match that is not its own, and always gives
  way. A match current at the table — real, on the page, or left running by
  somebody — blocks the demo and is never ended by it, because a match nobody
  scores any more looks exactly like one paused between two points. A match
  started at the table displaces a running demo, and a game asked for in the
  lobby or through `POST /games` ends it first, under the lock every start
  takes. The wait starts at `no_match` only, so demo and free table take turns.
- A demo gives way to anybody counting into it. It has no faults, so any
  divergence between its truth and zaehlwerk means another source scored into
  its match — buttons pressed during the demo, or a real match started in the
  instant between the demo seeing `no_match` and creating its own, which leaves
  the demo's match the newest and so the one the buttons score into. The demo
  then ends its match, and the table is the real match's again, or free. A game
  somebody plays keeps reporting its divergences instead; that is what it is
  for. The limit is a press that changes no score: a foreign ready press in
  `warmup` or `change_ends` diverges from nothing and does not end the demo.
  That is accepted, because a real match started at the table displaces the
  demo anyway. Both remaining cases would be closed for good by zaehlwerk
  knowing a match as a demo, which is an API change and needs its own ADR
  (#98).
- A demo is a match like any other to zaehlwerk: on the panel, on the page,
  on Schmetterpause's start page, named `Pong A` and `Pong B`. A pod killed
  mid-demo leaves its match current, and the restarted service treats it as
  somebody's — it waits rather than ending a match it cannot prove is its own.
- A button check is a game without a reporter: it says what to press and
  reads `/live/stream`, and the buttons press through `/ingest/button` like
  at any match. The stream carries the state after each transition and not
  who pressed, so the check judges a press by what it did — which half's
  player scored, which half stopped waiting — and against the prompt it was
  given. A press zaehlwerk ignores leaves no trace on the stream: a wrong
  half on a ready that half already gave is seen as the prompt going
  unanswered, and a bounce on a ready not at all. Telling them apart would
  need the source on the stream, which is an API change.
- A check's page has a token, as a browser game's has, and it only ends the
  check: `POST /games/{id}/presses` refuses a check. The token comes back
  from the fragment in a few lines of `web/check.js`, which also copies the
  summary; everything else on that page is htmx and SSE.
- A check's summary is English, like the issues it is written for; the page
  around it is German, like everything a player reads. Player vs player on
  two devices is not part of #78 and is #99.
