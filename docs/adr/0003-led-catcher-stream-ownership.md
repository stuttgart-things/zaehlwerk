# 3. Stream ownership when switching the LED catcher

Status: Accepted
Date: 2026-09-05

## Context

The LED matrix is a notification sink. Messages carry a display duration and are
replaced by whatever arrives next, so a running score pitched onto the shared
`messages` stream would flash up and then give way to the next GitHub error.

homerun2-led-catcher can switch its subscribed streams at runtime (#51), and its
web UI exposes the same switch to a human (#53). During a match the catcher
subscribes to `tabletennis` alone; afterwards it returns to `messages`.

That leaves two parties able to switch: zaehlwerk-api and whoever has the UI
open. If the API returns the catcher to `messages` at match end, it may undo a
switch a person made deliberately.

## Decision

zaehlwerk-api reverts a switch only if it made that switch itself. It records
that it holds the override and releases it at match end; a set it did not
establish is left alone.

A match that ends without a final point — abandoned, or the scorekeeper walked
away — releases the override after an inactivity timeout of 20 minutes.

The manual path back is the `reset` button in the catcher UI, which returns to
the configured streams.

## Consequences

- The two switchers do not fight. A deliberate manual override survives a match
  ending underneath it.
- A panel cannot stay stuck on a dead scoreboard, which is otherwise invisible
  until someone notices it has shown 0:0 for two days.
- zaehlwerk-api needs the catcher's address, so it depends on a service it does
  not own. The dependency is one-directional and failure is not fatal: if the
  switch fails the score still reaches the stream, it just competes with other
  events on the panel.
- The override is held in memory. A restart of zaehlwerk-api mid-match loses it,
  and the catcher stays on `tabletennis` until the manual reset. Acceptable at
  this scale; revisit if it happens.