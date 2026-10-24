# 16. The sandbox on the office panel

Status: Proposed
Date: 2026-10-24

> Builds on [ADR-0003](0003-led-catcher-stream-ownership.md) and changes
> nothing in it. Belongs with [ADR-0015](0015-pong-plays-reported-matches.md).

## Context

The sandbox of ADR-0015 is a second zaehlwerk, with its own Schmetterpause
and its own Pong, in a namespace of its own. Its matches are worth watching
in the office, and the office has one panel.

ADR-0003 gives that panel to a match by switching the catcher to
`tabletennis` alone and back to `messages` afterwards, and it reverts only a
switch it made itself. That rule is written for one automatic switcher and a
person at the catcher UI. Two zaehlwerks with `CATCHER_URL` would be two
automatic switchers: a sandbox match starting while somebody plays at the real
table would take the panel from them, and each release would find the other's
set and leave the catcher where neither wanted it. ADR-0008 settles that a
real match always wins the table, but only inside one registry; across two
instances nothing holds a lock for both.

The score also has two ways onto the bus. `REDIS_ADDR` writes to the stream
`PANEL_STREAM` names. Over omni-pitcher the stream is the pitcher's decision,
by the routes in its `ROUTES_CONFIG`, and a route for `system: tabletennis`
would put both instances on the same stream. `System` is also what the
catcher's display rules match on (`panel.Config`), and `Author` is fixed to
`zaehlwerk`, so today there is nothing a route could tell the two apart by.

## Decision

**The sandbox never switches the catcher, and is shown only while the office
table is free.**

- **The sandbox's zaehlwerk has no `CATCHER_URL`.** It never takes the panel.
  Without it, its score still reaches its stream, which is what the README
  already says a failed switch costs.
- **It publishes to a stream of its own**, `PANEL_STREAM=tabletennis-sandbox`,
  with the same `system: tabletennis`, so the catcher's display rules draw it
  exactly as they draw the office's. Only the stream differs.
- **The office zaehlwerk lists that stream among its idle streams**,
  `CATCHER_IDLE_STREAMS=messages,tabletennis-sandbox`. Between two office
  matches the catcher listens to both, and the sandbox's matches are on the
  panel next to the other notifications.
- **A real match in the office displaces the sandbox by the existing rule.**
  It switches the catcher to `CATCHER_MATCH_STREAMS`, `tabletennis` alone, and
  the sandbox is gone from the panel until the match releases it. Nothing
  across instances has to decide anything: the stream set does it.
- **Over omni-pitcher, a new `PANEL_AUTHOR`** sets the message's `Author`,
  default `zaehlwerk` as today. The sandbox sets `zaehlwerk-sandbox`, and a
  route on that author, placed before the one for the system, sends it to
  `tabletennis-sandbox`. With `REDIS_ADDR` the variable is not needed, but it
  still says on every message where it came from.
- **A demonstration on purpose goes through the catcher UI.** Whoever wants
  the sandbox on the panel alone switches the catcher to
  `tabletennis-sandbox` by hand. ADR-0003 already says the office zaehlwerk
  leaves a switch it did not make alone, and `reset` returns to the configured
  streams.

## Consequences

- Between office matches, sandbox scores share the panel with `messages`. A
  GitHub error can push a sandbox score aside for its display duration. That
  is what idle streams are for, and the office's own matches are not affected.
- The `demo=1` tag of ADR-0008 does not apply: a sandbox match is a real
  match in its own zaehlwerk. A catcher rule that wants to label the sandbox
  can match on the author.
- One variable, `PANEL_AUTHOR`, in `cmd/zaehlwerk-api` and the README's table;
  the sink already carries `Author`. Everything else is configuration of three
  existing services.
- The catcher's lag of about a minute on a switch (homerun2-led-catcher#56)
  applies when an office match takes over, as it does today: the first
  minute of that match is late on the panel, not lost.
- **To verify before this is accepted:** that the catcher subscribes to more
  than one idle stream at once and draws a `score` message from a stream its
  rules do not name, and that omni-pitcher evaluates routes in order. Both are
  in repositories outside this one.
