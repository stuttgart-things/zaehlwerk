# 14. Bot games played by the service

Status: Accepted
Date: 2026-10-17

> Supplements [ADR-0006](0006-a-game-as-an-ingest-source.md), which lets a
> browser decide a game's rallies, and
> [ADR-0009](0009-pong-on-two-devices.md), whose relay carries the frames.

## Context

A browser game between two bots (`mode: cvc`) is a game somebody watches. It
only goes on while somebody does (#153). The page that started it decides every
rally (ADR-0006), and `web/game.js` runs its physics on
`requestAnimationFrame`, which a browser pauses in a background tab and stops
in a closed one. Switch tabs, and the match on the panel stands still until
`PONG_BROWSER_IDLE` ends it. A second tab cannot take over: it watches, and
what it draws is played by the first.

There are two ways today to have bots play without anybody's tab, and neither
can be watched:

- **A headless game** (`driver: bots`), attract mode's included. It draws the
  winner of each rally from the bots' skill. There is no ball and no paddle,
  so there is nothing to draw, and a page that animated a rally to fit a winner
  drawn in advance would be invented (#120, ADR-0013).
- **The screensaver** (ADR-0013). It is a browser game, so it plays for as long
  as its screen stays on, and for no longer.

What the code already has:

- One `Driver` interface for whoever decides at the table: `Ready` and
  `Rally`, and nothing else. Truth, faults and reports are the game's, the
  same for every driver (ADR-0006).
- A relay (ADR-0009): `GET /games/{id}/relay` sends the latest frame of the
  table, ball and paddles, about 30 times a second, and keeps only the
  latest, so a slow tab skips frames rather than queueing them. The guest of a
  game on two devices draws exactly this, and `follow` in `game.js`
  interpolates between frames.
- The physics of a rally in `game.js`: a shot, the flight with spin, the
  bounces, the net, the paddles and the bots. About 150 lines, with no DOM in
  them.

## Decision

**A third driver, `server`, plays bot against bot in the service.** It runs the
physics of `game.js`, ported to Go, at 60 ticks a second, and decides each
rally and each ready press the way the page does in `cvc`. It hands them to
the game through `Ready` and `Rally`, like every driver. The game reports them
as it reports any game's, with the same truth, faults and press numbering.

**The service is the one reporter of a served game.** ADR-0006 asks for exactly
one reporter per game. For a served game that is the service itself, and no
page reports anything: every tab watches, the one that started the game too.

**Every tab draws the relay.** The driver posts a frame to the game's relay
every second tick, in the shape the host of ADR-0009 posts: tick, ball,
resting, paddles, `say` and `fx`. The page draws a served game as a guest draws
the host's, minus a paddle of its own. Phase, ends, waiting and serving still
come from `/games/{id}/events`, derived from zaehlwerk and from nowhere else.
Opening, closing, reloading or hiding a tab changes nothing about the game.

**The relay of a served game takes no frames from outside.** `POST
/games/{id}/frames` stays a game on two devices' route. A served game's frames
come from its driver only.

**The token only ends the game.** The page that started a served game gets a
token as any browser game's does, and with it the button "Spiel beenden". A
press with it is refused, as a button check's is.

**Bots only.** A person's paddle needs its physics where the person is. A game
somebody plays in a tab that goes to the background is a game nobody plays at
the moment, and it pausing is right. So `pvp`, `pvc` and `remote` stay in the
browser, and `driver: server` takes mode `cvc` or none.

**Started on purpose, not by default.** The lobby offers it as a choice under
"Computer gegen Computer", and `POST /games` takes `driver: "server"`. A `cvc`
game in the browser stays what it is: a game somebody watches, whose physics
are tried by eye.

**No idle limit.** A browser game ends after `PONG_BROWSER_IDLE` without a
press, because a closed tab would otherwise hold the table. A served game is
never idle: it plays to its last point and then frees the table. Like every
demo it gives way to a real match, and `maxReports` still ends one that stops
moving.

**The physics are ported, not shared.** There is no way to run `game.js` in Go
without a JavaScript engine, a dependency for zaehlwerk-pong and against the
spirit of ADR-0009's reasoning about dependencies. The port follows `game.js`
function by function, under the same names, so a change to one shows where
the other changes. A test reads the constants out of `game.js` and fails when
they and Go's drift apart, as one already does for the quiet period of a
counter reset (ADR-0006).

**Rallies are drawn from the game's seed.** The JavaScript draws from
`Math.random`. The Go port draws from a generator seeded like the headless
bots, so a served game played twice with the same seed and the same answers
from zaehlwerk plays the same rallies. Wall time does not enter the physics:
the clock is the tick count, and the 60 Hz only paces it.

## Consequences

- A bot match on the panel can run with no screen and be watched from any
  device that opens `/play/{id}`, at any moment, as often as anybody likes.
- **The next steps of #153 build on this and do not change it.** Pong gets
  `GET /live`, which sends a browser to the game playing now, and zaehlwerk's
  pages link to it while the current match is a demo; zaehlwerk needs no new
  setting for that, `PONG_URL` is already there. Attract mode can then play
  its demo on this driver, which answers #120, and the screensaver can watch
  it rather than start a game of its own. The diagonal serve (`web/serve.js`)
  follows the same port. Each is a change of its own.
- **Two copies of the physics.** A change to how the ball flies is made in
  `game.js` and in Go. The constants are held together by a test, the
  behaviour is not: like the physics in the browser today, it is tried by
  eye. Only bots play on the server, so a difference between the two is a
  difference in how bot games look, never in whether a person's shot was in.
- **Load.** A served game is one goroutine ticking 60 times a second over a
  handful of floats, and 30 frames a second of about 150 bytes for each
  watching tab, the same as a guest of ADR-0009. One game at a time, as
  before.
- **A pod restart ends a served game** as it ends every game: in memory only,
  with its match ended on SIGTERM.
- **`game.js` passes the size ADR-0009 sets for it.** Drawing a served game
  reuses the guest's drawing, which costs a handful of lines, and takes the
  file a little over 700. The two-device part, the relay included, moves into
  `web/remote.js` at the next change to it, as ADR-0009 already says.
- `View.Driver` gains `server`. A client that knew only `bots`, `browser` and
  `check` sees a fourth name; it is a game in mode `cvc`.
