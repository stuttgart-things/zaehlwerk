# 9. Pong on two devices

Status: Accepted
Date: 2026-10-14

## Context

The browser game of ADR-0006 is played at one device: two people share a
keyboard or a screen, or one person plays a bot. #99 asks for player against
player on two devices, with each player at their own phone or laptop.

Some of this is already settled by ADR-0006 and ADR-0008, and by #99 itself.
This ADR does not reopen it:

- **One device is the host.** It runs the physics, decides every rally and
  reports, exactly like today's browser game. It stays the only reporter of
  ADR-0006 and uses the token it already gets from the lobby.
- **The second device is the guest.** It sends only its paddle position and its
  ready key, and it draws what the host sends. It never reports anything to
  zaehlwerk.
- **The match is a demo** (ADR-0008) and gives way to a real match like any Pong
  game.
- **Everything goes through zaehlwerk-pong.** zaehlwerk does not change.

That leaves the questions this ADR answers: how the devices talk, how the guest
finds the game, what goes over the wire, what happens when a device drops, how
the guest's latency is handled, and what it costs in operations and in
JavaScript.

What the code brings to the question:

- zaehlwerk-pong has one push mechanism, SSE. `follow` in `play.go` writes a
  game's view on connect and on every change, with a heartbeat. Both
  `/games/{id}/events` (the canvas) and `/ui/games/{id}/stream` (the board) use
  it.
- A page reports through `POST /games/{id}/presses` with `X-Pong-Token`.
  `tokenGame` lets only the page that started the game press or end it.
- A browser game nobody presses in for `PONG_BROWSER_IDLE`, ten minutes by
  default, ends its match.
- The server runs one game at a time (`Server.running`) on one replica
  (`kcl/pong.k`, Recreate). A game is in that pod's memory and nowhere else.
- `cmd/zaehlwerk-pong` sets `ReadTimeout` 15 s, `ReadHeaderTimeout` 5 s and
  `IdleTimeout` 60 s on its `http.Server`, and deliberately no `WriteTimeout`.
  `writeTimeout` in `play.go` bounds each write to a stream instead.
- `go.mod` has four direct dependencies, and zaehlwerk-pong uses none of them
  outside its tests. The standard library has no WebSocket.
  `golang.org/x/net/websocket` describes itself as lacking features and points
  to other packages.
- `web/game.js` is 477 lines, one embedded file served from `/static/game.js`,
  with no build step. It already has a pvc mode in which a person controls one
  player and that player's paddle follows the player through a change of ends.
- In the cluster, zaehlwerk-pong sits behind a Gateway API `HTTPRoute` with a
  single `PathPrefix /` rule, on the gateway's `https` listener. In Compose,
  port 8087 is mapped straight to the container, with nothing in between.

## Decision

### 1. Transport: SSE down and POST up, no new dependency

The relay between the two devices uses the mechanism zaehlwerk-pong already
has. **Each device keeps one SSE stream open to receive, and sends with a POST
per message. The rate is throttled, and each sender has at most one request in
flight.** The browser interpolates between the frames it receives.

- **Host → guest:** the host posts a frame 30 times a second. zaehlwerk-pong
  keeps only the latest frame and wakes the stream, the way `follow` wakes on
  `Changes()`. A guest whose stream falls behind therefore skips frames rather
  than building up a queue.
- **Guest → host:** the guest posts its paddle 30 times a second while the
  paddle moves, and twice a second while it is still, which is the guest's
  heartbeat. zaehlwerk-pong again keeps only the latest sample.
- **One in flight:** each sender waits for the answer before its next POST. A
  sample produced in the meantime replaces the one waiting, so a slow request
  delays the next one but never piles them up.

**Estimated load per game.** This is an estimate, not a measurement. About 60
POSTs a second reach zaehlwerk-pong. Each carries a body of 40 to 160 bytes and,
over HTTP/1.1, roughly 400 bytes of headers. Over HTTP/2 the headers shrink to
a few dozen bytes with HPACK. The two streams carry about 30 events a second of
200 bytes, and 30 of 80 bytes. Together that is 30 to 45 KB/s, under 0.4
Mbit/s, or about 25 MB for a ten-minute game. For zaehlwerk-pong it is 60 small
handlers a second that copy a struct under a mutex. The WebSocket alternative
moves the same bodies with a few bytes of framing instead of headers, about 10
to 15 KB/s. The difference matters to nobody on an office network.

**Estimated latency in the office WLAN.** The round trip between a device and
the pod is typically 3 to 15 ms. Under load, and especially on a phone that
saves power on its radio, it can rise to 50 to 150 ms. One message from one
device to the other crosses two such hops through zaehlwerk-pong: typically
10 to 30 ms, and 100 ms or more in the bad case. On a warm connection, SSE plus
POST and a WebSocket differ by the parsing of a header, less than a
millisecond. Both run over TCP, and both stall the same way when a packet is
lost on the radio. What the guest's experience depends on is the WLAN, not the
choice between the two. The throttling above keeps a slow moment from turning
into a backlog.

The case for a WebSocket was fewer bytes and one connection instead of two. The
case against is a first dependency for zaehlwerk-pong, a connection the server
has to manage itself (see Options considered), and a path through the gateway
that has never carried a WebSocket. SSE already runs through that gateway
today, for every browser game. **SSE plus POST is chosen.**

### 2. Pairing: a short code and a URL, no QR code

- **Starting.** The lobby gets a mode "Zwei Geräte" (`mode: "remote"` on
  `POST /games`). The host names both players, as in pvp today. zaehlwerk
  takes the names when the match is created, and the guest joins only
  afterwards, so the guest cannot name themselves without an API change.
  The host is player a and the guest player b. The defaults are "Gastgeber"
  and "Gast".
- **The code.** The answer to the start, and the host's page, carry a join code
  of four digits and the URL `/join` on the same host. The page says so in
  German: "Am zweiten Gerät <host>/join öffnen und 4711 eingeben".
- **Joining.** `/join` is an htmx form. A correct code redirects to
  `/play/{id}` with a **guest token** in the fragment, the same way the lobby
  hands over the host's token.
- **The guest token.** The guest token is a second random token on the game,
  next to the host's. It is accepted on the guest's own POST and nowhere else.
  It does not press and does not end the game: `/presses` and `/end` answer it
  with `403`, because `tokenGame` compares against the host's token only.
- **What the code is for.** The code exists to pick the game and to prove that
  the guest can see the host's screen. A service runs one game at a time, so
  four digits are enough to name the game. They are not enough to keep anybody
  out: there are only 10,000 codes. Without the following limits, anybody on
  the same network could join a running game by trying codes:
  - **Single use.** The code is valid only while the game is playing and has
    no guest. It is used up by the first correct entry, and after that no code
    joins the game.
  - **Expiry.** A code that nobody has entered expires after **five minutes**.
  - **Lockout.** After **five wrong attempts** the code is locked. Whoever
    guesses therefore has five chances in 10,000 for each code the host
    draws.
  - **A new code.** In both cases the host's page offers a new code ("Neuer
    Code"), which only the host's token can ask for.

  The limits are constants, like the rates (section 6). The route is still the
  access control (`kcl/pong.k`), as before.

**A QR code is not worth a library.** Making one in Go would need a module
such as `rsc.io/qr`, a small one that receives few updates. Making one in the
browser would need a JavaScript library, against invariant 8. Writing the
encoder by hand, with Reed–Solomon, would be a few hundred lines to keep. The
guest is standing at the same table and types four digits into a short URL.
That costs a few seconds, once per game. If trying it at a fair shows that this
is where people give up, a server-side QR code as SVG is the first candidate.
That would be a decision of its own.

### 3. Messages, rates, and sides

Every body is JSON, as everywhere else in this service. zaehlwerk-pong checks
the token, caps the body at 512 bytes and rejects unknown fields. It does not
interpret the contents. Physics and rules stay on the host.

| Direction | Route | Body | Rate | Size |
| --- | --- | --- | --- | --- |
| guest → pong | `POST /games/{id}/paddle` (guest token) | `{"tick": 812, "y": 241.5, "ready": 2}` | 30/s moving, 2/s still | ~40 B |
| host → pong | `POST /games/{id}/frames` (host token) | `{"tick": 815, "ball": {"x":…,"y":…,"z":…}, "resting": null, "paddles": [212.0, 241.5], "say": "NETZ", "fx": [1100]}` | 30/s | ~120–160 B |
| pong → both | `GET /games/{id}/relay`, SSE | event `frame` (the latest host frame), event `guest` (the latest guest sample) | as posted, latest only | as above |

- **`tick`.** `tick` is the host's frame counter. The host stamps every frame
  with it. The guest echoes the tick of the frame it is drawing at that moment.
  That echo is the timestamp of fairness (section 5), and it needs no clock
  that both devices share.
- **`ready`.** `ready` counts the guest's presses of the ready key and only goes
  up. A sample that is replaced before it is sent therefore cannot lose a
  press. When the host sees the count rise, it sends `ready` for the guest's
  half through its own `/presses`. The guest's ready reaches zaehlwerk only
  through the host, and so the host remains the only reporter, with the press
  numbering ADR-0006 already has.
- **`say` and `fx`.** These carry the host's message ("NETZ", "AUS") and the
  sounds since the last frame, so the guest hears the same tock.
- **What the relay does not carry.** It carries no score, phase, ends, waiting
  or serving. The guest gets those exactly as a watching tab does today, from
  `/games/{id}/events` and the board stream, derived from zaehlwerk and
  nowhere else (ADR-0006).
- **Who can read the relay.** `/games/{id}/relay` takes no token, like
  `/games/{id}/events`. A third device can watch the ball, and nothing in it is
  secret.

**Sides.** Both devices draw the table as zaehlwerk's halves, left and right,
as the panel shows it. Neither view is mirrored, so both screens, the panel and
the real table agree. The guest controls player b wherever b stands, in the
same way pvc lets a person control player a. At a change of ends, both
canvases walk the players to the halves zaehlwerk sent (`shownEnds` follows
`st.ends`, as today). The guest's ready is pressed for the half where zaehlwerk
already has b. So the sides follow zaehlwerk's `ends` without the relay saying
anything about them.

### 4. Failures

Each device notices that the other is gone by silence: no message from the
other for **2 seconds**. The guest's heartbeat (2/s at rest) and the host's
frames (30/s) mean that silence is never just a still paddle.

- **The guest drops.** The host pauses. A ball in flight is taken off the
  table, nothing is reported, and the same side serves again. The host's canvas
  says "Gast getrennt – warten". The game does not end because the guest is
  gone. It ends the way every browser game ends: when nobody has pressed for
  `PONG_BROWSER_IDLE`, or when the host presses "Spiel beenden" at any time.
  There is no timeout of its own and no new variable (invariant 3).
- **The host drops.** The guest shows "Gastgeber getrennt" and draws nothing
  moving. With the host gone, nobody reports, so the same `PONG_BROWSER_IDLE`
  ends the match. This is the closed tab of ADR-0006, unchanged. A separate,
  shorter timeout was considered and rejected. A host whose phone locks its
  screen for a minute should find its game again, and a demo that holds the
  table a little longer costs nothing, because a real match ends it at once
  (ADR-0008).
- **Reconnecting with the same token.** Both tokens live in the tab's
  `sessionStorage`, as the host's does today, and stay valid for the whole game.
  A reload, or a stream the browser reopens by itself, carries on with the same
  token. The join code is the only thing that is used up. The host's press
  counter comes back from `last_press` in the view, as today.
- **zaehlwerk-pong restarts.** The game is gone, along with both tokens and the
  relay, as for every game today (no persistence). The SIGTERM path ends the
  match on zaehlwerk.

### 5. Fairness: the guest's input carries the tick it saw

The host plays with no latency. The guest sees the ball late, by the hop from
the host plus the interpolation delay, and its paddle reaches the host late,
by the hop back. **The guest's sample carries the tick it was drawing. The host
judges the guest's contact against the guest's paddle at that tick, not at the
moment the sample arrives.**

The host keeps the last 10 ticks of ball positions, about a third of a second.
When the ball crosses the guest's paddle line at tick N, the host does not
decide yet. It waits for a guest sample with a tick of N or later, and then
judges the contact with that sample's `y` against the ball at tick N. Today's
code already has a window of this kind: a missed ball is called only once it
is 60 px past the paddle (`b.passed && … > 60` in `game.js`). That is 115 ms
at the start speed and 57 ms at the top speed. For the guest's paddle, the
miss is called when the sample arrives instead, and at the latest **120 ms**
after the crossing, whatever the speed. Until then the ball flies on. If a late
sample shows a hit, the host plays the return from the crossing point. The
host's screen then shows a short jump of the ball. The guest's screen does not,
because the guest already drew the hit where it happened.

**The limit.** The window covers the round trip plus the interpolation delay of
about 50 ms. If that total stays under 120 ms, the guest plays as fairly as the
host. Above it, the guest is judged by the last paddle position that arrived,
and in practice loses fast balls on a bad connection. The host is never helped
by this. Its own paddle is always judged at the current tick. A guest could lie
about its tick, but only within the same 120 ms, in a demo that reports
nothing.

**Why not the other two options.** Accepting the lag makes the guest lose every
fast ball whenever the WLAN is slow, and the guest cannot tell why. A slower
ball for the guest changes the game for one player and not the other, and
still does not fix a lag that comes and goes.

### 6. Operations

- **Gateway.** The chosen transport needs no change to `kcl/pong.k`. The
  `HTTPRoute` already carries two SSE streams per browser game, and
  zaehlwerk's `/live/stream` crosses a gateway the same way. A gateway's idle
  timeout on streams is already covered by `DefaultHeartbeat`, 25 s, and the
  relay sends far more often than that anyway. The POSTs are ordinary short
  requests. On the `https` listener the browser speaks HTTP/2, so all of a
  device's streams and POSTs share one connection.
- **Compose.** Compose needs no change either. It is HTTP/1.1 straight to the
  container. A device holds three connections there (two streams and one POST
  at a time), which fits the browser's six per origin. More watching tabs open
  on the same browser against the same Compose host could reach that limit.
  That is a development setup, and it is accepted.
- **Server timeouts.** `ReadTimeout` 15 s and `ReadHeaderTimeout` 5 s fit
  requests of a few hundred bytes. The streams read no body. There is still no
  `WriteTimeout`, and `writeTimeout` bounds each write to a stream, as for
  every stream today.
- **Replicas.** The relay is in the pod's memory, which is why one replica and
  Recreate stay as they are (`kcl/pong.k`). A second pod would not know the
  game.
- **No new environment variable.** Rates, the 2 s of silence, the 120 ms
  window, and the code's length, expiry and lockout are constants in the code,
  with the reason written next to each.

### 7. How much JavaScript, and where

The exception to invariant 8 stays as ADR-0006 drew it: the canvas is
hand-written JavaScript, and everything around it is htmx and SSE.

- **The join page.** `/join` is an htmx form, as the lobby is.
- **The host's page.** The code and the URL are text in `play.html`.
- **What game.js gains.** The estimate is 150 to 200 lines in `game.js`:
  - the throttled sender with one request in flight, for both roles;
  - frame interpolation on the guest;
  - the guest's paddle on the host, interpolated between samples;
  - the tick history and the deferred judgement;
  - the two "getrennt" states;
  - the guest's ready counter.
- **The result.** That makes 630 to 680 lines in one embedded file, served as
  it is, with no build step, no module bundler and no library.

**The limit.** If `game.js` grows past about 700 lines, the remote part moves
into a second embedded file, `web/remote.js`. It is served the way `check.js`
is, still with no build step. Splitting it is then a refactoring, not a new
decision.

## Options considered

- **WebSocket with `github.com/coder/websocket`.**
  - What it offers: a single full-duplex connection and the smallest frames.
  - The library itself: ISC-licensed, with no dependencies of its own, and a few
    thousand lines. Coder has maintained it since 2024, after taking over
    `nhooyr.io/websocket`.
  - Rejected for three reasons:
    - **Dependency.** It would be zaehlwerk-pong's first runtime dependency, to
      update and scan, for a gain of about 30 KB/s on a LAN.
    - **The hijacked connection.** The server would have to run that
      connection itself. A hijacked `net.Conn` keeps the deadline that
      `ReadTimeout` set on it (see the `http.Hijacker` documentation), so a
      WebSocket handler has to clear it and keep its own heartbeat.
    - **The gateway.** Gateway API conformance does not include upgrading to a
      WebSocket. Common implementations do it by default, but this route has
      never carried one.

  This is the option to revisit if a measurement ever shows that the POSTs, and
  not the radio, are what the guest feels.
- **A WebSocket upgrade written by hand, or `golang.org/x/net/websocket`.**
  Writing it by hand means framing, masking, fragmentation and the closing
  handshake: a small protocol implementation in this repository, which only
  its tests would vouch for. `x/net/websocket` is no dependency saved, since it
  is a module too, and its own documentation recommends other packages.
  Rejected.
- **WebRTC peer to peer (a data channel).** It would take the relay out of
  zaehlwerk-pong and save one hop, a few milliseconds on a LAN. The costs:
  - **Signalling.** Offer, answer and ICE candidates still have to be exchanged
    through a server, which would be zaehlwerk-pong. That means an exchange
    protocol on top of the relay it was supposed to save.
  - **STUN and TURN.** In an office WLAN, client isolation often blocks traffic
    between two devices. Then only TURN helps, which is a relay server again,
    this time a new one to run.
  - **JavaScript.** Connection setup, ICE restarts and a data channel without
    retransmission is far more code in `game.js` than this whole decision. It is
    also hard to test from Go.

  Rejected. The single hop through zaehlwerk-pong is the reliable path, and a
  peer-to-peer path would need it as a fallback anyway.
- **SSE plus POST at full rate (60/s, every frame).** It would double the load
  for no visible gain, because the guest interpolates anyway. It would also
  queue requests whenever the WLAN stalls. Rejected in favour of 30/s with one
  request in flight and the latest message winning.
- **The guest reports its own presses.** It would be a second reporter, which
  ADR-0006 rules out, with press numbering on both devices. Rejected, as in
  #99.
- **The host simulates the guest's paddle beyond the last sample**
  (extrapolation). A paddle that turns would jump back. Interpolating between
  samples that have already arrived costs one sample interval and never draws
  a position the guest did not have. Rejected.
- **The guest picks its own name on joining.** zaehlwerk fixes the names when
  the match is created, and renaming a match is an API change in zaehlwerk,
  which this decision excludes. Rejected for now. It could follow from a
  zaehlwerk ADR of its own.
- **A separate, shorter timeout for a lost host or guest.** It would be a new
  variable (invariant 3), or a constant that cuts off a host whose phone locked
  its screen. The existing idle time already ends a game nobody presses in, and
  ADR-0008 already ends a demo the moment a real match needs the table.
  Rejected.

### Open questions

- **The 120 ms window** is today's 60 px out-margin at the start speed, kept
  for every speed, and rests on an assumed WLAN. Whether it is enough on phones in power-saving mode
  needs a measurement at the first trial. The constant is the one place to
  change.
- **A copied guest tab.** Duplicating a tab copies its `sessionStorage`, so the
  copied tab sends the same guest token, and both tabs then move the paddle,
  with the last sample winning. This is #97, for the guest token as for the
  host's. It is not solved here.
- **The lockout can be abused.** Anybody on the same network can lock every
  new code with five wrong entries, and the guest never gets in. For a demo
  behind the internal route this is accepted. Should it happen, a lockout per
  client address is the next step.
- **Two Pong instances** (ADR-0008 Consequences). A code entered on the wrong
  instance finds no game there and says so. Nothing beyond that is planned.

## Consequences

- **What zaehlwerk-pong gains:**
  - routes: `/join`, `POST /ui/join`, `POST /games/{id}/paddle`,
    `POST /games/{id}/frames`, `POST /games/{id}/code` (a new code, host token
    only) and `GET /games/{id}/relay`;
  - a mode `remote`, and a second token and a join code on a game.

  `go.mod` does not change.
- **zaehlwerk** does not change. For zaehlwerk, a game on two devices is a
  browser game. It is a demo with one source, `pong-<game id>`, reported by the
  host.
- **The guest.** A guest can neither score nor end the game. Losing the guest
  costs a pause, not a result.
- **What the guest's experience depends on.** It depends on the WLAN, and the
  limit is stated (section 5). The host is the reference: its game plays exactly
  as today's.
- **game.js.** It grows by about a third and stays one file without a build
  step. The exception to invariant 8 does not widen beyond the canvas.
- **Tests.** The relay is testable in Go like the presses: tokens, the latest
  message winning, the caps and the code. The fairness rule is in the
  JavaScript, and like the physics today it is tried by hand. A Go test cannot
  check it.
- **Relation to ADR-0006.** This supplements ADR-0006 and does not replace it.
  There is still exactly one reporter per game, it is the host, and it uses
  ADR-0006's token.
