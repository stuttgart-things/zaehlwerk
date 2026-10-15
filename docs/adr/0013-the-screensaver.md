# 13. The screensaver

Status: Accepted
Date: 2026-10-15

## Context

A free table has a screen next to it. Since #128 the watch screen shows a side
select there: a column per half, a QR code for each, and who has checked in.
That is the right screen while somebody is about to play. It is a still
picture for the rest of the day.

Attract mode (ADR-0008, #94) already fills a free table: after
`PONG_ATTRACT_AFTER` it plays a headless bot match, scored through the real
ingest path, so the panel shows a demo. It cannot be shown as a game. The
server only draws who wins each rally from the bots' skill, so there is no
trajectory to draw, and a page that animated a rally to fit an outcome already
drawn would be invented and look it (#120).

A browser game between two bots (`mode: cvc`) is the other way round. The page
plays every rally with the real physics and reports its winner through Pong to
zaehlwerk. What the screen shows and what the panel scores are the same rally.

Two things about it are new, and both need a decision:

1. **A browser starts a match nobody asked for.** Until now a match was
   started by a person (the scoring page, the lobby, two check-ins) or by
   attract mode on the server. A page that starts a game on its own is a
   third way.
2. **Two kinds of demo can want the table**, attract mode's and the screen's,
   and two screens at one table could each want it.

## Decision

**The screensaver is a Pong page, `GET /screensaver`, that starts a bot game
the moment it loads, and the next one ten seconds after that one is over.**
It is a browser game in `cvc` mode with a `Screensaver` flag on it. It decides
nothing about when the table is free. Opened on its own, it plays one demo
after another for as long as the table lets it.

**zaehlwerk's watch screen decides when to show it.** With the side select on
and `PONG_URL` set, a table that has been free with nobody holding a half for
`WATCH_SCREENSAVER_AFTER` (three minutes) gets the screensaver in a full-screen
frame, with the two QR codes small in the bottom corners. The watch screen
keeps polling the desk under it. A check-in brings the columns back at once,
and a real match brings the score. zaehlwerk never calls Pong: the address is
for the browser at the table.

**A screensaver game is a demo, with every rule of ADR-0008.** Its match is
created with `demo: true` and is never reported. A real match ends it, and a
real match running refuses it. It gives way the way attract mode's demo does:
a game somebody starts in the lobby ends it (`errYielded`), and a divergence of
any kind, which is somebody counting into it, ends it too (`errIntruded`).

**The screen moving on ends the demo.** When the watch screen swaps the frame
out for a check-in, the game's page sends `POST /games/{id}/end` with its token
on `pagehide`. Without that, the demo would hold the panel until
`PONG_BROWSER_IDLE` while the players who just checked in see bots on the
matrix. Only the tab holding the token does this, and only that tab starts the
next demo, so a second tab watching from the lobby does neither.

**One demo at a time, decided by the start lock that already exists.** A
screensaver start ends attract mode's demo, as any game asked for does, and
attract mode does not start while the screensaver's match is current, because
the table is not free. A screensaver start does **not** end another
screensaver's demo. It is refused as busy, and that page asks again every half
minute. Two screens at one table would otherwise take the table from each other
in turn, a new demo every few seconds and none played out. The screen that is
playing keeps it.

**On the watch screen, a demo is not a match to follow while the side select
is on.** The screen shows the side select or the screensaver over it, never a
demo's score. Without check-in the watch screen follows a demo as it always
has, because attract mode at a fair is something to watch.

## Consequences

- Pong gains no setting. zaehlwerk gains `PONG_URL` (unset: the side select
  stays up for as long as the table is free) and `WATCH_SCREENSAVER_AFTER`.
  Both are optional, as every coupling is (invariant 4).
- The screensaver's games are browser games, so the canvas, the rallies and
  the panel cannot disagree, and the comparison with the truth runs for them
  as it does for any browser game. A demo has no faults, so any divergence
  ends it.
- A screensaver left open with nothing to stop it plays demos all day, as a
  screensaver should. Each one is a match in zaehlwerk's registry, bounded by
  `MAX_RETAINED_MATCHES` like every other.
- The frame is cross-origin when Pong has its own host. The game's token is in
  the frame's own `sessionStorage`, which a browser that partitions
  third-party storage partitions by the top-level site. That does not change
  anything here: the token is only ever read back by the same frame.
- If `pagehide` does not fire, because the browser is killed or the screen
  loses power, the demo ends after `PONG_BROWSER_IDLE`, as any abandoned
  browser game does, and a real match ends it earlier.
- The headless attract mode stays. It is the fallback for a table without a
  screen.
