# zaehlwerk

Live scorekeeping for office table tennis.

Points arrive from wireless buttons, piezo sensors or a phone. `zaehlwerk-api`
owns the state of the running match and fans it out — to the LED matrix via
homerun, and to browsers over SSE.

```
buttons ──┐
          ├──ESP-NOW──> hub ──┐
piezo ────┘                   ├──> zaehlwerk-api ──┬──> homerun (tabletennis) ──> LED matrix
                              │                    ├──> SSE ──> live view
phone ────────────────────────┘                    └──> Schmetterpause (result)
```

## Why a separate service

The running score has three consumers that must not disagree, and the point
sources are hardware. Neither Schmetterpause nor the LED catcher is the right
owner for that — see [ADR-0001](docs/adr/0001-independent-scorekeeping-api.md).

Schmetterpause keeps players, TTR, tournaments and history, and receives a
finished result at match end. The LED catcher stays a generic sink: table tennis
points are just another event source with its own `system` label.

## Running it

```bash
task            # what to run to see a score on a simulated matrix
task --list     # every task, with a line each
```

[Task](https://taskfile.dev) is a convenience, not a dependency — every task is
a few lines of shell you can read in [Taskfile.yaml](Taskfile.yaml) and run by
hand. The service itself is one binary and needs nothing:

```bash
go run ./cmd/zaehlwerk-api
```

| Variable | Default | Meaning |
| -------- | ------- | ------- |
| `HTTP_ADDR` | `:8080` | Listen address |
| `LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN`, `ERROR` |
| `MAX_RETAINED_MATCHES` | `200` | Finished matches kept in memory; a running one is never dropped |
| `OMNI_PITCHER_URL` | — | homerun2-omni-pitcher base URL. Takes precedence over `REDIS_ADDR` |
| `OMNI_PITCHER_TOKEN` | — | Bearer token for it; `/pitch` answers 401 without one |
| `OMNI_PITCHER_PATH` | `/pitch` | |
| `SET_OPENER` | `alternate` | Who serves first in the sets after the first: `alternate` (ITTF law 2.13.6) or `loser` (whoever lost the previous set). A match may still choose its own |
| `REDIS_ADDR` | — | Redis holding the panel stream. Unset, and with no omni-pitcher, disables the panel |
| `REDIS_PORT` | `6379` | |
| `REDIS_PASSWORD` | — | |
| `PANEL_STREAM` | `tabletennis` | Stream the score is published to |
| `ALLOWED_ORIGINS` | — | Comma-separated browser origins that may read the live stream. Empty means none |
| `STREAM_HEARTBEAT` | `25s` | Keepalive interval on an idle stream |
| `CATCHER_URL` | — | LED catcher base URL. Unset disables stream switching |
| `CATCHER_MATCH_STREAMS` | `tabletennis` | What the catcher listens to during a match |
| `CATCHER_IDLE_STREAMS` | `messages` | What it goes back to |
| `CATCHER_IDLE_TIMEOUT` | `20m` | Give the panel back after this long without a point |
| `SCHMETTERPAUSE_URL` | — | Schmetterpause base URL. Unset, a match is scored and shown and reported nowhere |
| `SCHMETTERPAUSE_TOKEN` | — | Bearer token for its `/api`; without one configured there, the routes do not exist. The scoring page names both players from `/api/players` and whoever keeps score from `/api/operators`, which adds observers: accounts that never play but may count (Schmetterpause ADR-0023). Against a Schmetterpause without that route, the players are offered instead. It is also what Schmetterpause sends back on `POST /checkin/claim`, and set together with the URL it turns on check-in by phone (ADR-0011) |
| `CHECKIN_TOKEN` | — | Bearer token the hub sends on `POST /ingest/checkin`. Unset, check-in by fob is off: a check-in names a player, so the route takes none from anybody without it (#113). Check-in by phone does not need it |
| `CHECKIN_TIMEOUT` | `5m` | How long a lone check-in holds its half (ADR-0010). Check-in by fob is on with `SCHMETTERPAUSE_URL` and `CHECKIN_TOKEN`, by phone with `SCHMETTERPAUSE_URL` and `SCHMETTERPAUSE_TOKEN` |
| `CHECKIN_START` | `serve_rally` | The phase a checked-in match starts in: `serve_rally` or `warmup`. The first serve is always played for |
| `CHECKIN_CHANGE_ENDS` | `false` | `true` has the players of a checked-in match change ends |
| `SCHMETTERPAUSE_REPORT_DELAY` | `15s` | Grace period between a won match and its report (ADR-0007). An undo in it withholds the report, and the result that holds when it is over is the one sent. `0` reports at once; a value that is not a duration stops the start |

State is in memory and deliberately so — a match lasts twenty minutes and the
finished result goes to Schmetterpause.

**The result waits out a grace period before it goes**
([ADR-0007](docs/adr/0007-a-grace-period-before-reporting.md)). The match
point pressed for the wrong player and undone a second later is the mis-press
that happens at a table, and a result sent cannot be taken back. So the report
is sent `SCHMETTERPAUSE_REPORT_DELAY` after `match_won`, from the score as it
stands then: an undo in that time withholds it, and a match won again sends the
new result. The page says when the result goes and that an undo still takes it
back, and its button sends it at once. A shutdown sends what is waiting; a
`SIGKILL` in the grace period loses it, the loss ADR-0004 already accepts. An
undo after the report is not reported again: the page says the result over
there no longer holds, and it is rejected or corrected in Schmetterpause.

## API

Every route answers with the current match state, so a client renders from the
response without a second call:

```json
{
  "match_id": "9f3a1c02", "players": ["Anna", "Bernd"],
  "points": [10, 9], "sets": [1, 0], "set_number": 2, "serving": "b",
  "complete": false, "completed_sets": [[11, 7]]
}
```

Errors answer `{"error": "...", "state": {...}}`, with the state where the match
was identified.

### Match lifecycle

```bash
curl -s -X POST localhost:8080/matches \
  -H 'content-type: application/json' \
  -d '{"players": ["Anna", "Bernd"], "best_of": 5}'      # 201, Location: /matches/{id}

curl -s localhost:8080/matches/{id}
curl -s localhost:8080/matches/current                  # 404 when nothing is running
curl -s -X POST localhost:8080/matches/{id}/undo         # 409 if there is nothing to take back
curl -s -X POST localhost:8080/matches/{id}/end          # idempotent
```

`POST /matches` takes `players`, `best_of`, `points_per_set` and `first_server`,
all optional — the defaults are a best of five to eleven with `a` serving. `best_of` is any odd number; the scoring page offers 1 (a single game, one set decides), 3, 5 and 7.

`"demo": true` marks a match that is no result and gives way to one, as every
zaehlwerk-pong match is (ADR-0008). A real match always wins the table:

- Creating a real match ends every running demo at once, the way
  `POST /matches/{id}/end` would, and the table stream announces it ended.
- A demo asked for while a real match runs is refused with 409, and the answer
  carries that match's state.
- A demo is never reported; one with a player or operator id is a 400.

Without the field a match is real, so nothing that never heard of it changes.

### Phases and ends

A match can also start the way it starts at the table
([ADR-0005](docs/adr/0005-match-phases-and-table-halves.md)). All optional, and a
match that asks for none of it behaves exactly as before:

| Field | Value | What happens |
| --- | --- | --- |
| `start` | `warmup` | both sides press once when ready; then the serve rally, unless `first_server` names one |
| `start` | `serve_rally` | one rally for the first serve: the winner's side presses, nothing is scored |
| `first_server` | `rally` | the same as `start: serve_rally` |
| `change_ends` | `true` | the players change ends after every set, and in the deciding set at five; play waits until each has pressed at the new half |
| `set_opener` | `alternate`, `loser` | overrides `SET_OPENER` for this match |

```bash
curl -s -X POST localhost:8080/matches -H 'content-type: application/json' \
  -d '{"players": ["Anna", "Bernd"], "best_of": 3, "start": "warmup", "change_ends": true, "set_opener": "loser"}'
```

The state says where the match stands: `phase` (`warmup`, `serve_rally`,
`playing`, `change_ends`, `complete`), `ends` (who stands at the left and the
right half) and `waiting` (the halves still to press). Outside `playing` a press
is not a point; a correction or a second press from the same half changes
nothing.

`GET /matches/current` is the match hardware ingest would land in: the most
recently created one still running. A board asks it before its first point, to
label its halves and know who serves (zaehlwerk-firmware ADR-0006); a won or
ended match is not current.

Undo is not gated on the match still running: taking back a wrongly awarded
match point is exactly when it is needed. Ending a match is not the same as
winning one, and both stay readable afterwards.

### Ingest

All three carry the four fields the ESP-NOW payload already has — `source`,
`player`, `delta`, `event_id` — because the hub forwards what it received and
one vocabulary across firmware, adapter and scorer is worth more than three
spellings of the same thing.

A source that only knows where it is mounted sends **`half`** (`left` or
`right`) instead of `player`, never both. The scorer resolves the half against
the current ends, so a button under the left half scores for whoever stands
there after a change of ends (ADR-0005).

`event_id` is a counter, monotonic **per source**, and a repeat of one already
seen is discarded with a 200 and the current state. That is what makes retrying
safe, and it is the whole point of
[ADR-0002](docs/adr/0002-idempotent-ingest-contract.md) — the firmware counter
must live in RTC memory or every wake restarts it at zero.

```bash
# button — the hub batches a burst it queued while a request was in flight,
# and applies them in order. delta defaults to 1.
curl -s -X POST localhost:8080/ingest/button \
  -H 'content-type: application/json' \
  -d '{"events": [{"source": "button-a", "player": "a", "event_id": 7}]}'

# a button that knows its half, not its player
curl -s -X POST localhost:8080/ingest/button \
  -H 'content-type: application/json' \
  -d '{"events": [{"source": "button-left", "half": "left", "event_id": 8}]}'

# piezo — delta is required: the sensor knows which side of the table it sits
# under, not whether the hit was a point. 0 is recorded and changes nothing.
curl -s -X POST localhost:8080/ingest/piezo \
  -H 'content-type: application/json' \
  -d '{"source": "piezo-a", "player": "a", "delta": 0, "event_id": 12}'

# web — the phone. source per browser session, so two people scoring the same
# match do not share a counter.
curl -s -X POST localhost:8080/ingest/web \
  -H 'content-type: application/json' \
  -d '{"match_id": "9f3a1c02", "source": "phone-3f9a", "player": "a", "event_id": 3}'
```

**`match_id` is optional for hardware and required for the browser.** A button
wakes, sends and sleeps; the ESP-NOW payload has no room and no source for a
match id, so `/ingest/button` and `/ingest/piezo` resolve the running match
themselves. A browser knows the id of the match it created, and a tab left open
from yesterday should not score into today's match.

A malformed event rejects the whole batch with a 400 and applies none of it —
half a burst on the scoreboard is worse than none of it, and the hub retries the
whole request anyway.

### Check-in per half

With `SCHMETTERPAUSE_URL` and `CHECKIN_TOKEN` set, players can claim the halves
of the table with a key fob instead of being picked on the page
([ADR-0010](docs/adr/0010-check-in-per-table-half.md)). The hub reads the fob,
waits for the next press of a button to learn the half, and sends both once,
under the same `source` + `event_id` contract as a point
(zaehlwerk-firmware#63), and with `CHECKIN_TOKEN` as a bearer
(zaehlwerk-firmware#65):

```bash
curl -s -X POST localhost:8080/ingest/checkin \
  -H 'content-type: application/json' -H "authorization: Bearer $CHECKIN_TOKEN" \
  -d '{"source": "hub-1", "event_id": 41, "half": "left", "uid": "04:A3:1B:22"}'
# {"outcome":"claimed","half":"left","name":"Anna","left":"Anna"}

curl -s localhost:8080/checkin       # who holds which half, by name
```

zaehlwerk asks Schmetterpause who the fob is
(`POST /api/identities/rfid/resolve`, the same token as `/api/results`) and
keeps the player and the check-in it got back against the half. The UID is
hex, 4, 7 or 10 bytes, with `:`, `-`, a space or nothing between them. It is
passed on once and kept nowhere: not in a log line, not in an answer, not in
memory.

| `outcome` | Status | Meaning |
| --- | --- | --- |
| `claimed` | 200 | the half was free and is now this player's |
| `unchanged` | 200 | the same player at the same half again; the newer check-in is kept and the timeout starts again |
| `replaced` | 200 | another player held this half and no longer does |
| `moved` | 200 | this player held the other half and now holds this one |
| `started` | 200 | the second half: the match is created, `match_id` names it |
| `duplicate` | 200 | an `event_id` older than the last from this source; nothing changed |
| `unknown` | 404 | Schmetterpause does not know the fob; nothing changed |
| `refused` | 409 | a match is running; nothing changed and Schmetterpause was not asked |
| `unavailable` | 503 | Schmetterpause did not answer, or has no fob route; the half stays as it was |

A retry with the same `event_id` gets the first answer again, with
`"repeated": true`, and Schmetterpause is not asked twice: every resolve is a
new check-in over there. Without `SCHMETTERPAUSE_URL` or `CHECKIN_TOKEN` the
route answers 503, `GET /checkin` leaves out `"fob"` (and says
`"enabled": false` unless check-in by phone is on), and nothing else changes. A check-in without the token, or with another, is a 401 before its
body is read: a fob UID is no secret, a phone reads one in a second, and a
check-in names a player, or during "Fob koppeln" pairs a UID with one. Points
need no token, because a point names nobody.

Both halves claimed creates the match: the left half is `a` and home, the right
`b` and away. It starts in `CHECKIN_START`, and its result goes to
Schmetterpause with `home_checkin_id` and `away_checkin_id` and no
`operator_id` (Schmetterpause ADR-0024). A 409 for it means a check-in already
carries the result, which is the answer to a retry of one that arrived, and is
recorded as delivered. A lone claim is released after `CHECKIN_TIMEOUT`, a real
match started on the page or through the API releases every claim at once (a
demo does not), and a restart forgets every claim. A match started on the page still names an
operator, as before.

#### By phone, through Schmetterpause

With `SCHMETTERPAUSE_URL` and `SCHMETTERPAUSE_TOKEN` set, a player can also
check in with their phone ([ADR-0011](docs/adr/0011-check-in-by-phone-through-schmetterpause.md)).
They scan the QR code at a half, which opens a page on Schmetterpause, and
confirm there. Schmetterpause records the check-in and claims the half here,
with the token this service sends to it, in the other direction:

```bash
curl -s -X POST localhost:8080/checkin/claim \
  -H 'content-type: application/json' -H "authorization: Bearer $SCHMETTERPAUSE_TOKEN" \
  -d '{"half": "right", "player_id": "<uuid>", "checkin_id": "<uuid>"}'
# {"outcome":"started","half":"right","name":"Bernd","match_id":"…","left":"","right":""}
```

From there on it is the fob's claim: the same halves, `CHECKIN_TIMEOUT`, the
same rules for the same player, another player or the other half, and both
halves claimed start the match, reported with both check-ins and no operator.
One half by fob and the other by phone is the ordinary case. The display name
comes from `/api/players`, as for a fob; nothing is checked against
Schmetterpause beforehand, because the result is checked against the
check-ins there anyway.

The answer has `/ingest/checkin`'s body and statuses: 200 for `claimed`,
`unchanged`, `replaced`, `moved` and `started`, 409 `refused` while a match
runs, 400 for a half that is not `left` or `right` or an id that is not a
UUID, 401 without Schmetterpause's token or with another, compared before the
body is read. A claim with a `checkin_id` seen before claims nothing and gets
the first answer with `"repeated": true`; the last 256 are remembered. Without
`SCHMETTERPAUSE_URL` or `SCHMETTERPAUSE_TOKEN` the route answers 503, as
`/ingest/checkin` does without `CHECKIN_TOKEN`, and one line at start says so.
Each token opens only its own route: the hub's does not claim, and
Schmetterpause's does not check a fob in. `GET /checkin` says which paths are
on, as `"fob"` and `"phone"`.

## Live score

    GET /matches/{id}/stream

Server-Sent Events, one per transition, fed from the scorer rather than by
reading the Redis stream back — that would add latency to the one view where
latency is visible, and tie the live score to a stream it does not need.

```bash
curl -N localhost:8080/matches/$MATCH/stream
```

```
data: {"kind":"snapshot","state":{"match_id":"a1b2c3d4","points":[3,5],…}}

data: {"kind":"point","state":{"match_id":"a1b2c3d4","points":[3,6],…}}

: ping
```

The current state arrives on connect as `snapshot`, so a client that joins
mid-match renders immediately without a second request. After that it is one
event per transition — `point`, `set_won`, `match_won`, `undo` — carrying the
same state JSON the REST endpoints return. A comment every `STREAM_HEARTBEAT`
keeps the connection through a proxy; it carries no data, and there is no
polling or periodic resend.

### The table, not one match

    GET /live/stream

The same events for whatever match is **current** — the one hardware ingest
lands in — for a page that stays open all afternoon and cannot know a match id
in advance (Schmetterpause's running score, stuttgart-things/schmetterpause#188):

```bash
curl -N localhost:8080/live/stream
```

```
data: {"kind":"no_match"}

data: {"kind":"snapshot","state":{"match_id":"a1b2c3d4","points":[0,0],…},"home_id":"…","away_id":"…"}

data: {"kind":"point","state":{"match_id":"a1b2c3d4","points":[1,0],…},"home_id":"…","away_id":"…"}
```

- On connect: the current match's `snapshot`, or `no_match` between matches.
- A match that becomes current — started on the page or over the API — arrives
  as its `snapshot`, then its transitions; the client never reconnects.
- When the match is won (`match_won`) or ended early (`match_ended`), that
  event, then the next running match's `snapshot` if there is one, else
  `no_match`.
- `home_id` / `away_id` are the players' Schmetterpause ids, present exactly
  when the match is being reported. They are added at the API layer; the scorer
  knows players by name only. A match nobody reports has none.
- `demo: true` is on every event of a demo match (ADR-0008) and absent on a
  real one, added at the same layer. A page that follows the table can label
  a demo, or leave it out.

Same guarantees as the per-match stream: `ALLOWED_ORIGINS`, heartbeat, a write
deadline, slow clients dropped rather than waited for. One table, one current
match (ADR-0003); missed events are not replayed.

JSON rather than rendered HTML: the led-catcher's own UI swaps HTML partials
over SSE because htmx does the swapping there. The scoring page below does the
same, over a stream of its own — but this one is for the clients that do not
share its markup, a spectator view and Schmetterpause, which each render
differently. A shared fragment would constrain all of them to one layout.

**A watcher that stops reading is dropped, not waited for.** The hub is a
scorer observer like the panel sink, so it runs under the scorer's lock and
cannot block. A client that falls behind loses its oldest queued events and
keeps the newest — safe because the state is complete rather than a delta, so
a client that missed three points and receives the fourth is not missing
anything the score depends on. A phone that sleeps mid-set wakes up current.

**Origins are a list, never `*`.** `ALLOWED_ORIGINS` names who may read the
stream from a browser; unset means no cross-origin browser can, while curl and
anything server-side are unaffected. `"*"` in the list is an origin named
`"*"`, not a wildcard.

## Scoring from a browser

    GET /ui

A page that scores the same match the buttons under the table score: a point
per side, undo, the running score, and the title the panel is being given right
next to it. It is what `task demo` was — a way to play a match without hardware
— with a scoreboard instead of a shell loop, and it is a phone by the table as
much as it is a mock.

```bash
task run     # then http://localhost:8080/ui — or: task ui
```

The new-match form also starts a match the way the table does
([ADR-0005](docs/adr/0005-match-phases-and-table-halves.md)): **First serve →
Play for it** (one rally decides it), **Before the first point → Warm-up** (both
sides press once when ready) and **Ends → Change** (after every set, and at five
in the deciding set). The defaults are the match as it always was. While a phase
is waiting, a click on a side is that side's press rather than a point, and the
board says what it is waiting for.

`/` redirects to it, `a` and `b` score, `u` takes a point back. Opening the
page joins the match a button would score into, the same one `/ingest/button`
resolves; a match that has finished is still there, at `/ui?match=<id>` with
the id `task demo` and `POST /matches` print.

**The page counts the way a phone does.** A source drawn per page load and an
`event_id` that goes up by one per click, exactly the contract in
[ADR-0002](docs/adr/0002-idempotent-ingest-contract.md) — so a double-posted
click is discarded by the same deduplication that discards a retried button
press, and two phones scoring one match never share a counter. Starting and
ending a match go through the same code `POST /matches` does, so the LED panel
changes hands the same way
([ADR-0003](docs/adr/0003-led-catcher-stream-ownership.md)) whoever started the
match.

Points go in over `POST /ui/matches/{id}/point` rather than `/ingest/web`
because htmx posts form-encoded and swaps HTML partials back — the same split
homerun2-led-catcher makes between its `/streams` API and its `/ui/streams`
control. Nothing about the JSON contract changes for the hub or the firmware:
these routes are the page's, and they answer 200 with the reason rendered into
the board where the JSON API would answer 4xx, because htmx does not swap an
error response and a button that silently does nothing is worse than one that
says why.

The board is fed by its own stream, `GET /ui/matches/{id}/stream`, which sends
rendered HTML — so a point from a button, from `task demo` or from somebody
else's phone lands on it. That is why `GET /matches/{id}/stream` can stay JSON:
the clients that render differently keep the format that lets them, and the one
page whose markup this service owns gets the format htmx wants.

### Watching

    GET /ui/watch

The same match without anything to press: the score as large as the screen
allows, who serves, the sets and the phase line when play is waiting. It is for
a TV or a tablet next to the table, where a stray tap must not score — so it is
its own page with no buttons and no form rather than a mode of the scoring
page, which links to it.

`/ui/watch` follows the table: it shows the running match and moves on by
itself when the next one starts, leaving a finished match up until then.
`/ui/watch?match=<id>` stays on that match. The score comes over the same
stream as the board, `GET /ui/matches/{id}/stream?view=watch`, rendered without
buttons and without the timeline.

**It looks like schmetterpause and is laid out for a phone first.** The page
started as a copy of the led-catcher simulator's look, which was right for a
mock and wrong for the product family: somebody scores a match here and enters
it in schmetterpause, so the palette, the type (Space Grotesk and JetBrains
Mono) and the shapes are schmetterpause's, copied into the page rather than
shared. Each score has its `+1` directly under it, tall enough to hit with a
thumb while holding a bat; the keyboard hints only show where there is a
keyboard, and the new-match form folds away while a match is running.

htmx, its SSE extension and the two typefaces come off a CDN, the way the
led-catcher's simulator loads its dependencies: the browser opening the page
needs to reach `unpkg.com` and Google Fonts once, and nothing else in the
service does.

The page is as unauthenticated as `/ingest/web` is — same network, same
assumption.

## The panel

Every scorer transition — a point, a taken-back point, a set, the match — is
pitched as a homerun message onto the `tabletennis` stream, where
homerun2-led-catcher picks it up and renders it on the 64x64 matrix.

| Field | Value |
| ----- | ----- |
| `system` | `tabletennis` |
| `severity` | `INFO`, or `SUCCESS` on a set or match win |
| `title` | `7:5`, `SET 1:0`, `WIN 3:1` — the whole of what the panel shows |
| `message` | `Anna 7 : 5 Bernd` — for the catcher's log, never on the matrix |
| `author` | `zaehlwerk` |
| `tags` | `match=<id>,set=<n>,transition=<kind>[,side=a\|b]`, then the board (below) |

The catcher renders `{{ title }}` and nothing else, so the score has to fit in
a title: a 6x10 font at x=2 on a 64x64 panel is about ten glyphs, and anything
longer is not shortened, it runs off the edge. A test enforces the limit across
every score a match can reach.

`SET` and `WIN` are prefixed because a set score of `2:1` and a point score of
`2:1` are otherwise the same three characters, and the panel would be ambiguous
exactly when it matters. Whether that is the right wording is a decision for
the table — the simulator below is what makes it decidable without hardware.

The tags are what other catchers on the stream filter on without learning any
table tennis. `transition` is `point`, `set_won`, `match_won` or `undo`. `side`
is the side the transition went to:

| `transition` | `side` |
| ------------ | ------ |
| `point` | the player who scored |
| `point`, a correction that lowered a score | absent — it went to nobody |
| `set_won`, `match_won` | the side that took the set, decided by the score, which after a correction is not always the player the event was for |
| `undo` | absent |

`match` and `set` stay first and in that order, so a `tags_contain` rule
written against them keeps matching.

```
match=36c17b30,set=2,transition=point,side=a
match=36c17b30,set=2,transition=set_won,side=b
match=36c17b30,set=3,transition=match_won,side=a
match=36c17b30,set=2,transition=undo
```

After the routing tags comes the board — what the catcher's `score` display
needs to draw the whole match instead of the title alone:

| Tag | Value |
| --- | ----- |
| `points` | points of the set in progress, `5:10`; after a set win `0:0`, after the match the final set |
| `sets` | sets won, `1:2` |
| `serve` | `a` or `b`, who serves the next point — absent once the match is won, and before the serve rally has decided it |
| `winner` | `a` or `b`, only once the match is won |
| `banner` | `SATZ <name>` on a won set, `SIEG <name>` on the won match, `ANGABE <name>` on the won serve rally — the name cut at six characters, as the board shows it. Outside play the instruction instead: `EINSPIELEN`, `UM DIE ANGABE`, `SEITENWECHSEL`. Absent on every other transition |
| `player_a`, `player_b` | the names; a comma or semicolon in a name becomes a space |
| `played` | finished sets, space-separated: `11-8 9-11` — absent before the first |
| `phase` | `warmup`, `serve_rally`, `playing`, `change_ends`, `complete` (ADR-0005) |
| `ends` | who stands at the left and the right half, `b:a` after a change of ends |
| `demo` | `1` on a demo match, always the last tag (ADR-0008) — absent on a real one, so a display rule can label or hide a demo without learning any table tennis |

```
match=36c17b30,set=3,transition=point,side=b,points=5:10,sets=1:1,serve=b,player_a=Anna,player_b=Bernd,played=11-8 9-11,phase=playing,ends=a:b
```

Who serves is said here because the rules are here: the catcher draws the
server's name in orange and applies no table tennis of its own.

### Two ways onto the bus

`REDIS_ADDR` writes to Redis directly — fewest moving parts, and right when
there is a Redis to reach. `OMNI_PITCHER_URL` posts to
[homerun2-omni-pitcher](https://github.com/stuttgart-things/homerun2-omni-pitcher)
instead, which is what a zaehlwerk running by the table needs: Redis in the
cluster is a ClusterIP service and not reachable from there, while omni-pitcher
has an HTTPRoute and a bearer token.

```bash
OMNI_PITCHER_URL=https://omni-pitcher.example OMNI_PITCHER_TOKEN=...   go run ./cmd/zaehlwerk-api
```

Both are a `panel.Pitcher`, so the sink is the same either way — the retry
policy, the drop-rather-than-block, the drain on shutdown all behave
identically.

**Over omni-pitcher the destination stream is the server's decision.** It
routes by rules an operator declares in `ROUTES_CONFIG`, matching on system,
author, tags or title; without a rule for `system: tabletennis` everything
lands on its `default_stream`. The pitch still succeeds — so if the panel has
been switched to the match stream, it shows nothing at all and the logs look
fine.

`PANEL_STREAM` therefore doubles as an expectation on this path: the stream the
server reports back is compared against it, and a mismatch is logged once.

```
the score is landing on a different stream than the panel is watching
  expected=tabletennis actual=messages
  hint=omni-pitcher needs a route for this system, otherwise everything goes to its default stream
```

The route it is asking for:

```yaml
streams: [messages, tabletennis]
default_stream: messages
routes:
  - match: { system: tabletennis }
    stream: tabletennis
```

**A failed pitch never fails a match.** The sink hands transitions to a bounded
queue and returns; the scorer calls observers under its own lock, so anything
slower would stall the next point. A publish that fails is logged and dropped —
no queue, no retry. A point re-sent thirty seconds late would be worse than one
never sent. Redis being unreachable costs the panel, not the score.

### Giving the panel to a match

With `CATCHER_URL` set, the catcher is switched to `tabletennis` alone when a
match is created and back to `messages` when it ends, so the score is not
interleaved with GitHub errors for the length of a game. Implements
[ADR-0003](docs/adr/0003-led-catcher-stream-ownership.md).

It is switched back **only if we still hold it.** Two parties can switch the
catcher — this service and whoever has its UI open — so before reverting, the
current set is read back and compared with what we left. Anything else means a
person made a deliberate choice, and it is left alone.

A match releases the panel when it is ended, when it is won, when the process
shuts down, or after `CATCHER_IDLE_TIMEOUT` without a point — the last of those
because a match nobody finished would otherwise leave a dead score up until
someone noticed.

An undo that reopens a won match takes the panel back, as long as no other
match has been started since: that one has the panel then, and keeps it. The
release at `match_won` does not wait for the grace period the report waits
for, because the panel can be taken back and a result cannot
([ADR-0007](docs/adr/0007-a-grace-period-before-reporting.md)).

The switch happens on match creation rather than on the first point, so the
catcher has changed over before that point is pitched. A failed switch is
logged and changes nothing else: the score still reaches the stream, it just
shares the panel.

> **The panel lags a switch by about a minute.** The catcher's read loop keeps
> reading the old stream until a socket timeout tears it down, rather than
> picking the new set up within `BLOCK_MS` as documented —
> [homerun2-led-catcher#56](https://github.com/stuttgart-things/homerun2-led-catcher/issues/56),
> measured at 59s against v0.5.1. Nothing is lost; the points published in the
> gap are delivered once the loop comes round. But the first minute of a match
> is not on the panel, and that is the catcher's to fix, not ours.

### Seeing it without a matrix

The catcher ships a web simulator that renders the same 64x64 panel in a
browser, so the whole path is checkable with no hardware.

**Everything runs on your machine.** Two containers and one local process —
nothing leaves the network except the one-time image pull from ghcr.io.

```
                          your machine
┌──────────────────────────────────────────────────────────────────────────┐
│                                                                          │
│ task demo                                                                │
│     │  POST /ingest/web                                                  │
│     v                                                                    │
│ zaehlwerk-api ── go run, :8080                                           │
│     │                                                                    │
│     ├──> live hub ──> GET /matches/{id}/stream ──> task watch, or a tab  │
│     │                                                                    │
│     └──> panel sink                                                      │
│              │  XADD tabletennis                                         │
│              v                                                           │
│          redis-stack ── container, :6379                                 │
│              │  XREADGROUP                                               │
│              v                                                           │
│          homerun2-led-catcher ── container, LED_MODE=full                │
│              │                                                           │
│              ├──> rgbmatrix bindings ── absent here, draws nothing       │
│              └──> web simulator, :8081 ──> your browser                  │
│                                                                          │
└──────────────────────────────────────────────────────────────────────────┘
```

On a Pi the right-hand branch is the real matrix and the simulator is just a
second view of the same thing. That is why the containers run `LED_MODE=full`
and not `web`: `full` also loads the hardware handler, which draws nothing
without the rgbmatrix bindings but keeps its timing — and the timing is the
part that bites (see below).

#### Running the demo

Two terminals:

```bash
task panel:up      # terminal 1: redis + the real led-catcher, in simulator mode
task run           # terminal 1: zaehlwerk, stays in the foreground

task demo          # terminal 2: play a match, a point every 3s
task panel:open    # terminal 2: the 64x64 panel in a browser
```

`task ui` opens the scoring page instead, if you would rather play the match by
hand and watch the panel follow — same match, same stream, one browser window
each.

`task` on its own prints exactly that, and `task --list` has the rest.

`task demo` plays a best-of-3 through a deuce and writes the score as it goes:

```
  Anna  9 : 9  Bernd   sets 0:0   serving Anna
  Anna 10 : 9  Bernd   sets 0:0   serving Anna
  Anna 11 : 9  Bernd   sets 1:0   serving Bernd
```

It is long enough that the panel shows all three kinds of output rather than a
single score sitting there: points in white as `10:9`, the set win in green as
`SET 1:0`, and `WIN 2:0` at the end.

| | |
| --- | --- |
| `PACE=0.5 task demo` | faster; `PLAYERS=Ada,Grace task demo` for other names |
| `task demo:quick` | five points as fast as they go, no waiting |
| `task ui` | the scoring page, to play the match by hand |
| `task panel:events` | what the panel showed, in the terminal, no browser needed |
| `task panel:logs` | the catcher saying what it displayed and why |
| `task panel:streams` | which streams it is subscribed to right now |
| `task watch ID=<match>` | follow one match's SSE stream; the id is printed by `task demo` |
| `task panel:restart` | restart the catcher, e.g. after redis was recreated under it |
| `task panel:down` | stop both containers |

The `task run` terminal logs an `event ingested` line per point, so you can see
a point land before it reaches the panel.

#### Ports

Simulator on <http://localhost:8081>, Redis on `localhost:6379`, the API on
`:8080`. All three are overridable — `ZW_PORT`, `ZW_SIMULATOR_PORT`,
`ZW_REDIS_PORT` — and `.env` sets them once for every task instead of on each
command:

```bash
echo 'ZW_PORT=8090' >> .env && task run
```

`task run` checks the port is free before starting and prints that line for you,
with a port it has checked is actually free — rather than failing with a bind
error part-way through the startup log.

#### Watching the stream switch

`task panel:up:switching` starts the catcher on `messages` instead, so ADR-0003
has something to switch away from:

```bash
task panel:up:switching
CATCHER_URL=http://localhost:8081 task run
```

Then `task panel:streams` shows `tabletennis` once a match is created and
`messages` again once it ends. The panel trails the switch by about a minute —
[led-catcher#56](https://github.com/stuttgart-things/homerun2-led-catcher/issues/56).

### Why the score is held, not timed

[deploy/panel/profile.yaml](deploy/panel/profile.yaml) is the display rule for
the panel side. It sets `hold: true`, which tells the catcher to keep the score
up until the next point replaces it. Points fall every five to fifteen seconds,
so anything time-based leaves the panel dark for most of a match.

That took a fix in the catcher first. `duration: 3600` — the obvious way to
express "keep it up" — did not mean what it looked like: `led_catcher` called
its display handler synchronously from the consumer loop, and a static display
was a blocking `time.sleep(duration)` on the same asyncio loop that served its
HTTP endpoints. At 3600 the catcher showed the first point of a match, stopped
reading the stream, and stopped answering `/healthz` — a liveness probe would
have restarted it after every first point.

Fixed in
[homerun2-led-catcher#55](https://github.com/stuttgart-things/homerun2-led-catcher/pull/55):
displays run on a worker thread that owns the matrix, and `hold` is a rule
setting rather than a very large number. **Needs catcher v0.5.1 or newer.** The
profile still carries `duration: 3` underneath, so an older catcher ignores
`hold` and behaves as it did before instead of falling back to a longer default.

A finite `duration` still means what it says there — the message gets its
screen time and is not cut short. That is why the `fallback` rule keeps one:
something that is not the score should scroll past rather than sit on the panel
until the next point lands.

## The chain, without a board or Schmetterpause

`tools/chain-mock` stands in for both ends of piezo → zaehlwerk →
Schmetterpause, so the whole path runs on a laptop
([#55](https://github.com/stuttgart-things/zaehlwerk/issues/55)). It is a
development tool and is not deployed.

```bash
task chain:schmetterpause   # the fake: roster out, results in, in memory
task chain:run              # zaehlwerk with SCHMETTERPAUSE_URL pointed at it
task chain:piezo            # a board plays a best of three to the end
task chain:results          # what arrived at the other end
```

**The fake Schmetterpause** answers `/api/players`, `/api/operators`,
`/api/results` and the fob resolve route with the refusals the real one gives. Those are a 401 without the
token, and a 400 or 422 for a result it would not store: an operator who is
playing, an observer as a side, an unknown id, a set won by one. Its roster is
fixed: Anna, Bernd and Clara play, Olga only watches. A contract test drives it
with this service's own client, so it cannot quietly speak a different API.
`task chain:mode MODE=refuse` or `MODE=hang` loses the next result on purpose,
and the page then offers the retry that ADR-0004 promises. `MODE=accept` lets
that retry through.

It also answers `POST /api/identities/rfid/resolve` for check-in (ADR-0010).
Anna's fob is `04:A3:1B:22`, Bernd's `04:5C:7E:91`, Clara's
`04:11:22:33:44:55:66`; `DE:AD:BE:EF` belongs to nobody and is an empty 404.
Every resolve records a check-in, and a result proven by two of them is checked
the way the real one checks it: each belongs to its side, they are at different
halves, neither is older than three hours, and a check-in that already carries
a result is a 409. A result with an operator and check-ins is a 400.
`curl -X POST localhost:8082/link -d player_id=<id>` opens the "Anhänger
koppeln" window for a minute, and the next unknown fob becomes that player's.
`GET /checkins` lists what was recorded, with `method` `rfid` or `phone`.
`SCHMETTERPAUSE_FOBS=off` leaves the route out, as the real one does without
`SP_RFID_UID_KEY`.

It plays the phone path too (ADR-0011). `POST /phone` with `half` and
`player_id` is a player confirming at the table page: the fake records a phone
check-in and claims the half at zaehlwerk's `POST /checkin/claim` with its
token, and answers with zaehlwerk's status and body. A `checkin_id` repeats
the claim of that check-in. It needs `ZAEHLWERK_URL`, which
`task chain:schmetterpause` sets:

```bash
task chain:phone HALF=right PLAYER=bernd   # Bernd checks in at the right half by phone
```

**The board** does what zaehlwerk-firmware#41 describes, and is meant as the
reference for it:

- It joins through `GET /matches/current` and asks again before every rally,
  so an undo from the phone changes who serves.
- It sends one `POST /ingest/piezo` per rally, with a counter that only goes
  up.
- It maps table half to player and swaps them at each set boundary.
- It misbehaves the way a real sender does:
  - some rallies go out as `delta: 0`
  - some events go out twice, and it stops with an error if the second one
    changes the score
  - some points are taken back through the undo route
- It never counts. The score always comes from the response.

By default the board starts the match itself, through the page's form. It
picks two players from `GET /api/players` and whoever keeps score from
`GET /api/operators`, preferring an observer. It never assumes a name or an id:
against the fake it finds Anna, Bernd, Clara and Olga, and against a real
instance it finds whoever is there. `task chain:piezo JOIN=1` behaves like the
real board instead: it waits until somebody starts a match on the page, then
joins it.

**Against a real Schmetterpause**, a preview with seed data for instance, point
zaehlwerk and the board at the same instance:

```bash
SCHMETTERPAUSE_URL=https://schmetterpause.example SCHMETTERPAUSE_TOKEN=… task run:bare
SCHMETTERPAUSE_URL=https://schmetterpause.example SCHMETTERPAUSE_TOKEN=… task chain:piezo
```

Against a deployed zaehlwerk, set `ZAEHLWERK_URL` and run
`go run ./tools/chain-mock piezo` directly. The result arrives there pending,
and one of the two players confirms it as with any scoreboard result
(Schmetterpause ADR-0015). If zaehlwerk names the players differently from the
roster the board read, the two are not using the same instance. In that case
the result would be refused at the end of the match, so the board stops before
the first rally and says so.

| Variable | Default | |
| -------- | ------- | - |
| `SCHMETTERPAUSE_ADDR` | `:8082` | where the fake listens (`ZW_SCHMETTERPAUSE_PORT` in `.env`) |
| `SCHMETTERPAUSE_TOKEN` | — | the fake: unset, `/api` does not exist, as with the real one. The board: the bearer token it sends |
| `SCHMETTERPAUSE_MODE` | `accept` | `refuse`, `hang` |
| `SCHMETTERPAUSE_FOBS` | — | `off`: the fake has no fob resolve route |
| `SCHMETTERPAUSE_URL` | — | where the board reads its players from. Unset, it starts a match nobody reports |
| `ZAEHLWERK_URL` | `http://localhost:8080` | the board: what it scores into. The fake: where `POST /phone` claims a half; unset, it has no phone check-in |
| `PIEZO_SOURCE` | `piezo-mock` | |
| `PIEZO_PACE` | `1s` | between rallies |
| `PIEZO_AMBIGUOUS`, `PIEZO_RESEND`, `PIEZO_UNDO` | `0.1`, `0.1`, `0.05` | shares of rallies |
| `PIEZO_SEED` | `1` | so a run can be repeated, and the same players picked |
| `PIEZO_MATCHES` | `1` | `0` keeps playing |
| `PIEZO_JOIN` | `false` | `true` waits for the page instead of starting a match |
| `PIEZO_BEST_OF` | `3` | when it starts the match |
| `PIEZO_CONTROL_ADDR` | — | a listener for `GET`/`POST /control`: pause, pace and the three shares, changed while it plays. Unset, no listener. The table mock's page drives it |

`task chain:e2e` plays all of this once without anybody at the keyboard. It
checks that the result arrives, that a resent event is a duplicate, that a
refused result gets through on the retry, and that a match point given to the
wrong player and undone in the grace period is never reported while the right
result is, once. It runs zaehlwerk with a grace period of `2s`. CI runs the
same script on every pull request (`chain-e2e.yaml`).

The board also ships as its own image, `ghcr.io/stuttgart-things/zaehlwerk-piezo`,
built from `tools/chain-mock/cmd/piezo`. It has the same tags as the zaehlwerk
image (`pr-<n>-<sha>` and `pr-<n>` on pull requests, `main` and the release on
main), and takes the variables above. It logs JSON rather than text, because it
runs where logs are collected.

The board pushes, because the ingest contract is a push (ADR-0002). A board
could instead follow `/matches/{id}/stream` to see a correction without asking
for it, but that is the firmware's choice and the mock does not make it.

### The buttons, and the table mock

`tools/chain-mock/buttons` is the two wireless buttons under the table and the
hub they report to, with a page to press them on. With `PIEZO_CONTROL_URL`
set, the same page also runs the piezo board, so the whole table is one page. It is there to decide what
the buttons have to do before anybody builds one: the gestures of
zaehlwerk-firmware#56, the button of #2, the hub of #3 and #58.

```bash
task run             # zaehlwerk
task ui              # start a match on the page — a button never starts one
task chain:buttons   # then http://localhost:8084
```

With the board next to it:

```bash
task chain:piezo JOIN=1      # the board, with its control endpoint on :8085
task chain:buttons PIEZO=1   # the page, with a piezo section
```

The reader at the hub is on the page too (zaehlwerk-firmware#63). "hold Anna's
fob", then a press on a half within five seconds, sends
`POST /ingest/checkin` for that half instead of a point; two halves start the
match. It needs a zaehlwerk that resolves fobs, so run it against the fake:

```bash
task chain:schmetterpause    # the fake, with its fobs
task chain:run               # zaehlwerk with check-in on
task chain:buttons           # then hold a fob and press
```

"a stranger's badge" is a card nobody owns. The log names fobs by their label
and never writes a UID, as the firmware must not. "API response lost" sends
the check-in twice with one event id, and the second answer is the first one
repeated, with nothing resolved again.

The piezo section pauses and resumes the board, and sets its pace and the
shares of ambiguous, resent and taken-back rallies while it plays. Pausing
happens between rallies, never inside one. Pause it to try the buttons without
a second scorer in the same match. In a cluster, `EDGE_ESP_MOCK_REPLICAS` stays
the hard switch for the day real boards are at the table. It lives in Git,
where Flux would undo anything a page did to it.

Hold a big button and let go. A short press is a point for that half. A press
held past the ring is a long press and takes the last point back. Both held is
`both_long`, and on a phone both can be held at once. The log underneath shows
every step under the layer that took it:

| Layer | What it does |
| --- | --- |
| `button` | decides the gesture from how long it was held, and numbers the frame |
| `radio` | the ESP-NOW frame as #58 lists it |
| `hub` | drops repeated frames, pairs two long presses, maps the half to a player, calls zaehlwerk |
| `api` | what zaehlwerk answered |

The page also switches faults on, one at a time. Lines the chain got wrong are
red:

- **Debounce off.** A bounce wakes the button twice, with two ids. Nothing
  downstream can tell it from two presses.
- **Radio ACK lost.** Every frame arrives twice with the same id. A point is
  still safe without the hub, because ingest discards the repeat. An undo is
  only safe if the hub drops it.
- **API response lost.** The hub sends every call twice. A point is safe. An
  undo takes back two points, because `POST /matches/{id}/undo` carries no
  event id.
- **Both window.** A long press is held back for this long, in case the other
  side follows. Every undo is late by the same amount.

**A display instead of a button.** *Device: display* replaces the two buttons
with a small screen per half, as zaehlwerk-firmware#61 (prototype B)
proposes. Each screen has two zones: **+1** scores, and **undo** takes one back
at once. There is no long press, so there is no both-long window to wait for.
The frame carries the intent (`point` or `undo`) instead of a gesture. The
screen shows the score the hub sent back in the ACK of that screen's own last
tap, and how old it is. A device that sleeps between taps knows nothing newer,
and that is the question the variant is there to answer: is a score that is
only as fresh as your own last tap worth a screen?

`both_long` calls nothing unless *both_long ends the match* is ticked. Ending
is the only API call that comes close to "new game", and a match ended by
accident cannot be resumed.

| Variable | Default | |
| -------- | ------- | - |
| `ZAEHLWERK_URL` | `http://localhost:8080` | what the hub calls |
| `ZAEHLWERK_PUBLIC_URL` | `ZAEHLWERK_URL` | what the page links to, for when the hub reaches zaehlwerk on a cluster address |
| `BUTTONS_ADDR` | `:8084` | where the page listens (`ZW_BUTTONS_PORT` in `.env`) |
| `BUTTONS_SOURCE` | `button-mock` | the two sources are this with `-a` and `-b` |
| `BUTTONS_LONG_PRESS` | `1s` | where the page starts |
| `BUTTONS_BOTH_WINDOW` | `400ms` | where the page starts |
| `BUTTONS_DEVICE` | `button` | `display` starts with a screen per half instead; the page can switch either way |
| `PIEZO_CONTROL_URL` | — | a piezo board's control endpoint (its `PIEZO_CONTROL_ADDR`). Unset, the page has no piezo section |
| `CHECKIN_TOKEN` | — | what the hub sends as a bearer with a check-in; zaehlwerk's `CHECKIN_TOKEN`. `task chain:run` and `task chain:buttons` share one |

It also ships as `ghcr.io/stuttgart-things/zaehlwerk-buttons`, built from
`tools/chain-mock/cmd/buttons` with the same tags as the board. It serves
`/healthz` for a probe. The settings live in memory and are shared by everyone
who opens the page, which is right for one table and one hub.

## zaehlwerk-pong

A table tennis game that plays whole matches against a running zaehlwerk —
warm-up, the rally for the serve, points, the change of ends, the deciding set —
and scores them through the same ingest contract the buttons use
([ADR-0006](docs/adr/0006-a-game-as-an-ingest-source.md), #78). It needs no
hardware and nobody pressing a button, and it knows who really won every rally,
which is what the mocks above cannot know.

It is its own binary, `cmd/zaehlwerk-pong`, and its own image,
`ghcr.io/stuttgart-things/zaehlwerk-pong`, built and tagged like the others
and released with zaehlwerk. It plays in the browser — two people at one
device, a person against the computer, two people on two devices, or two
computers to watch — and headless,
two bots whose rallies are drawn by skill, with faults injected on purpose if
asked, and by itself in attract mode when the table has been free for a while.
And it checks the real buttons: it says which one to press and compares what
arrives.

```bash
task chain:pong        # zaehlwerk and zaehlwerk-pong side by side
open http://localhost:8087/   # the lobby: start a game in the browser
task chain:pong:play   # or, in a second terminal: a headless bot match
```

`task chain:pong:play PACE=0s` plays the headless one as fast as zaehlwerk
answers. With `task panel:up` and `task run` instead, `task chain:pong:game`
starts only the game, and the match plays on the simulated panel.

**In the browser.** The lobby (`/`) starts a game: the mode, the names, the
computer's skill — a preset (`leicht`, `mittel`, `schwer`) that fills three
sliders, reaction time, paddle speed and aim error, which can then be moved —
and the faults below. The game's page (`/play/{id}`) is the table from above,
one bounce per half, height as shadow and ball size, spin from the paddle's
movement, an angle from where the ball meets the paddle, rallies that speed up,
and a tock on paddle and table (`M` mutes it). Above it are the banner and the
score, the panel's words (`EINSPIELEN`, `UM DIE ANGABE`, `ANGABE <name>`,
`SEITENWECHSEL`, `SIEG <name>`); below it, truth against zaehlwerk, with the
first unexplained divergence marked and the expected ones told apart, and the
faults as they are injected.

| | Left half | Right half |
| - | --------- | ---------- |
| Move | `W` / `S` | `↑` / `↓` |
| Serve, ready | `D` | `←` |
| Touch | drag on the left half, tap to serve or confirm | the same on the right |

Against the computer and on two devices either set of keys works, and `Space`
serves. In the warm-up both sides play freely and confirm with their key; in a change of ends
the paddles and names swap once both have pressed at their new half.

The page decides rallies and nothing else. Every rally won and every ready key
is a press, `POST /games/{id}/presses` with `{"rally": n, "kind": "rally" |
"ready", "half": "left" | "right"}`, numbered from 1 without gaps; the same
number again is answered without a second press. The service keeps the truth,
injects the faults and forwards to zaehlwerk, exactly as for a headless game,
and answers once it is ready for the next press — so the answer already holds
whatever happened in between. Phase, ends, waiting and serving reach the page
from zaehlwerk only, on `GET /games/{id}/events` (the view as JSON, for the
canvas) and `GET /ui/games/{id}/stream` (the board as HTML, for htmx).

Exactly one page reports for a game. Starting one hands out a token, in the
answer to `POST /games` with `"driver": "browser"` and from the lobby in the
URL fragment, which no server sees; the page keeps it in that tab. A press
without it is a 403. A duplicated tab carries the token too, but asks the
other tabs first and watches when one already holds it. Another tab, or another device, opens the same page and
watches the board. `POST /games/{id}/end` with the token ends the game and its
match; a browser game nobody presses in for ten minutes (`PONG_BROWSER_IDLE`)
ends itself the same way, so a closed tab does not keep the panel. A game
whose match zaehlwerk ends, on the scoring page or through
`POST /matches/{id}/end`, ends at once.

**On two devices** ([ADR-0009](docs/adr/0009-pong-on-two-devices.md)). The
lobby's mode "Zwei Geräte" (`"mode": "remote"`) makes the page that starts it
the host: it plays A, runs the ball and is the only page that reports, with
the token as above. It shows a four-digit code and `<host>/join`. The guest
enters the code there and is sent to the same `/play/{id}` with a token of its
own in the fragment; it plays B, sends its paddle and its key, and draws what
the host sends. The code is used up by the first join, expires after five
minutes and is locked after five wrong tries; the host's page then offers a new
one (`POST /games/{id}/code`, host token only). The guest's token is refused on
`/presses` and `/end`: its ready key reaches zaehlwerk only as the host's
press.

| Route | Token | What |
| ----- | ----- | ---- |
| `GET /join`, `POST /ui/join` | — | the code, answered with `/play/{id}#guest:<token>` |
| `POST /games/{id}/code` | host | the code that still joins, or a new one |
| `POST /games/{id}/frames` | host | `{"tick", "ball": {"x","y","z"} \| null, "resting", "paddles": [l, r], "say"?, "fx"?}`, 30/s |
| `POST /games/{id}/paddle` | guest | `{"tick", "y", "ready"}`: the host tick it drew, its paddle, its key as a count |
| `GET /games/{id}/relay` | — | SSE: the latest `frame` and the latest `guest`, each once |

The service keeps only the latest message each way, caps a body at 512 bytes,
refuses fields it does not know and reads nothing else in it. The host judges a
guest's contact against the guest's paddle at the tick the guest saw, at most
120 ms late, so a slow WLAN costs the guest less than its latency. Two seconds
without the other device is "Gast getrennt" on the host — the game pauses, and
a ball in flight is not reported — or "Gastgeber getrennt" on the guest; the
game still ends by `PONG_BROWSER_IDLE` or by the host's button. `GET /games/{id}`
says `"remote": {"joined", "locked", "expires_at"}` and never the code.

**How it scores.** A game starts its own match with `POST /matches`: free-text
names (`Pong A`, `Pong B`), `start: "warmup"`, `change_ends: true` and
`demo: true` — every Pong match is a demo, whether attract mode, a browser game
or a button check started it (ADR-0008). It never sends a player id or an
operator, so a Pong match is never reported to Schmetterpause, and it never
joins a match: while `/live/stream` shows one as current, it does not start,
because that match belongs to somebody — and one carrying `home_id`/`away_id`
is a real match between real players. A 409 from `POST /matches`, zaehlwerk
refusing a demo next to a real match, is the same busy table. The one current
match a game asked for does not wait for is a demo that is not the service's
own, left behind by a pod that is gone or run by another instance: it is ended,
and the game starts. Every press
goes to `POST /ingest/web` with the `match_id`, the `half` the bot stands at,
and `delta: 1`, under the source `pong-<game id>` and the game's own counter as
`event_id`, so a rally's number is its event id — until a `counter_reset` fault
starts the counter again. A report whose answer is lost is sent again with the
same id, and zaehlwerk discards the repeat.

**What it checks.** It reads phase, `ends`, `waiting` and `serving` from
zaehlwerk's answers and derives none of them. The bots walk only when zaehlwerk
calls a change of ends. After every rally the game compares the rallies each
bot really won with the points zaehlwerk has awarded over the whole match, the
winner of the serve rally with who serves, and where the bots stand with
`ends`. A rally where they part, or where how far apart they are changes, is a
divergence, logged with its number.

```bash
curl -s -X POST localhost:8087/games -d '{"pace": "0s"}'   # 201, Location: /games/{id}
curl -s -X POST localhost:8087/games -d '{"driver": "browser", "mode": "pvc"}'   # 201, with a "token"
curl -s localhost:8087/games/{id}
```

```json
{
  "id": "e8f92912", "source": "pong-e8f92912", "match_id": "6cede9fe",
  "status": "won", "seed": 1, "players": ["Pong A", "Pong B"],
  "rallies": 59, "truth": [33, 26], "zaehlwerk": [33, 26], "explained": [0, 0],
  "divergences": [], "unexplained": 0, "fault_shares": {"…": "all 0"}, "faults": [],
  "ends_changes": [{"set": 2, "points": [0, 0]}, {"set": 3, "points": [0, 0]}, {"set": 3, "points": [5, 3]}],
  "state": {"…": "zaehlwerk's last answer"}
}
```

`status` is `playing`, `won`, `ended` (ended on the page, another match became
current, or the service stopped) or `failed`. One game plays at a time; a
second `POST /games` answers 409, as does one while a match is current, and 502
when zaehlwerk cannot be reached. A game whose match is taken over by a newly
started one ends its own match and stops. On shutdown the running game ends its
match, so it does not stay current with nobody scoring it. Games are in memory,
the last twenty of them.

**Attract mode.** With `PONG_ATTRACT_AFTER` set, the game keeps the panel
alive at a fair or in the office: once the table has been free that long, it
starts a headless bot match by itself, one report every `PONG_ATTRACT_PACE`
(a second, which the panel can be followed at), and never with faults, whatever
`PONG_FAULTS` says. Unset, there is no attract mode, and the log says so at
startup. The service stays on `/live/stream` for this — it listens rather than
polls, and connects again when the stream breaks — and "free" is what the
stream says: the wait starts at a `no_match` and at nothing else.

It always gives way, and never takes the table from anybody:

- A match started at the table ends the demo inside zaehlwerk, at once, and
  the demo stops when it sees the new match current. The wait then starts
  again only at the next `no_match`, once that match is over. A demo that
  would start in the instant after a real match is refused by zaehlwerk
  (ADR-0008).
- A game somebody starts, in the lobby or with `POST /games`, ends a running
  demo and takes the table. Every start holds one lock from looking at the
  table to the new game running, so the demo cannot start again in between.
- Anybody counting into the demo ends it. A demo has no faults, so any
  divergence from zaehlwerk means another source scored into its match —
  buttons pressed during a demo. The demo ends its match, leaving the table
  free. A ready press in the warm-up or at a change of ends changes no score
  and does not end it.
- A match somebody left running is never ended. It blocks the demo, and the
  log names it once, after it has been quiet for the whole wait.
- The exception is a demo that is not the service's own — left behind by a
  pod that was killed mid-demo, or another instance's. Once it has been quiet
  for the whole wait, it is ended, and the wait starts again at the `no_match`
  after it. A demo somebody plays is never quiet that long.
- A won demo is followed by a whole wait before the next, so demo and free
  table take turns rather than match following match.

The lobby says it is on and when the next demo starts, or that a match is at
the table; `/healthz` is the same either way. A demo is marked `"attract":
true` on `GET /games/{id}`.

```bash
task chain:pong:game ATTRACT=1m        # from source, against task run
docker compose -f deploy/panel/compose.yaml --profile pong up -d   # the image, with the simulated panel
```

**Button check.** "Taster prüfen" in the lobby, or `POST /games` with
`{"driver": "check"}`, tests the buttons, the hub and the radio against a known
truth. It starts a match like every game — warm-up, change of ends, no ids, so
never reported — and sends nothing itself. The page says what to press, big
enough to read from the table: "Bereit: LINKS" in the warm-up and at a change of
ends, "RECHTS drücken" for the rally for the serve and for a point, with the
player it counts for. The prompts are drawn from the seed and from where
zaehlwerk has the match, so a run with the same seed asks for the same thing.
They cover both ready presses, the rally for the serve, points on both halves,
a change of ends, and after it both halves again — the same button now
counting for the other player. That is about twenty presses.

The game reads `/live/stream` and judges every transition of its match against
the prompt: the right half, the right phase, one transition for one prompt. A
prompt waits up to `PONG_CHECK_TIMEOUT` for its press, and the next comes a
second after it is answered. Each ends as one of:

| Outcome | What it means |
| ------- | ------------- |
| `ok` | the press arrived at the half asked for |
| `wrong_half` | it arrived at the other half |
| `double` | the same button again in the second after the press: a bounce, or a hub sending twice |
| `missing` | nothing arrived before the timeout; on a ready prompt it may also be a press at the wrong half, which zaehlwerk ignores |
| `unexpected` | a press in the second between two prompts that is not a bounce, or a transition no press makes |

The page and `GET /games/{id}` (`check`) keep the counts, the first findings
with their time, and a summary as text to paste into a firmware issue. Three
prompts in a row with nothing pressed end the check and its match. Two limits
follow from what the stream says, which is a state and not who pressed: a
ready press at a half that has already pressed is ignored by zaehlwerk, so a
wrong half on the second ready shows as `missing`, and a bounce on a ready is
not seen at all. A demo gives the table to a check like to any game.

**Faults.** Between the truth and the ingest, a game can send what a real table
sends when something goes wrong. The truth is never touched; every fault is
logged with its rally number and listed under `faults`, with the `effect` it
should have on zaehlwerk's points. Each is a share from 0 to 1, drawn from the
seed, so the same seed injects the same faults:

| Fault | What is sent | What zaehlwerk must do | A chance on |
| ----- | ------------ | ---------------------- | ----------- |
| `wrong_half` | the press for the other half; with `correct=true` an undo follows, then the right half | count it; take it back on the undo, in whatever phase it left the match | every rally, the serve rally too |
| `resend` | the same press again, same `event_id` | discard it | every press |
| `bounce` | the same half again, new `event_id` | count it | every point |
| `lost` | nothing | nothing — one point short | every point |
| `ghost` | a press for a random half, before the rally | count it | every point |
| `wrong_phase` | the same half confirming again in `warmup` or `change_ends` | ignore it | every confirmation still waited on |
| `counter_reset` | after a quiet gap, the counter starting again at 1 | accept it (ADR-0002) | every press |

The game keeps what the faults should have done to the score next to the
truth, as `explained`. A divergence the faults explain is `expected`; any other
is a finding and counts in `unexplained`. For points, a finding is the press
where the unexplained part changed — a button pressed under the table is one
finding, not one for every rally after it. A run is good when that is 0, with
faults or without. zaehlwerk's answers carry no outcome, so "discarded" and
"ignored" are checked as the state staying the same. `counter_reset` waits
62 seconds first: zaehlwerk takes a counter that falls back as a restart only
after a minute of silence from that source and only for an id up to 8
(`scorer.DefaultRestartQuietPeriod`, `DefaultRestartMaxEventID`), and anything
shorter is a reset it rightly discards — which shows as a finding.

`PONG_FAULTS` sets them for every game; `POST /games` replaces them for one,
as a whole:

```bash
curl -s -X POST localhost:8087/games \
  -d '{"pace": "0s", "faults": {"wrong_half": 0.1, "correct": true, "lost": 0.05}}'
```

`task chain:e2e` plays seeded best of threes until one reaches a deciding set,
and fails on the first rally where truth and zaehlwerk part, on a deciding set
without its change of ends at five, and on a Pong result arriving at the fake
Schmetterpause. Then it plays one game with every fault above zero and fails on
any unexplained divergence, or if the seed no longer injects every fault. That
game waits out one counter reset, so the e2e takes a minute longer. A game on
two devices is played with a curl host and a curl guest: the code must join
once, the guest's token must not press, the guest's ready must reach zaehlwerk
only through the host's press, and truth and zaehlwerk must agree. Last, it
starts the service again in attract mode with a two-second wait and fails if no
demo starts on the free table, if a demo carries faults, if one does not give
way to a match started at the table or to a game started through the API, or if
a demo starts while that match is still current. The match that ends the demo
is started on the page: zaehlwerk must end the demo at once, the buttons must
count into the page's match, a demo asked for next to it must be refused, and
Schmetterpause must hear of neither. Then two button checks, with
a curl per button posting to `/ingest/button` with a half the way the hardware
does: one pressed as told must end without a finding, one with a press at the
wrong half must report exactly that.

| Variable | Default | |
| -------- | ------- | - |
| `HTTP_ADDR` | `:8081` | Listen address. Not `:8080`, so it runs next to a local zaehlwerk. `task chain:pong` uses `ZW_PONG_PORT` (`8087`), because `:8081` is the simulator's there |
| `LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN`, `ERROR` |
| `ZAEHLWERK_URL` | `http://localhost:8080` | The zaehlwerk it plays against. Only the service calls it, never a browser |
| `PONG_BEST_OF` | `3` | Any odd number |
| `PONG_BOT_SKILL` | `mittel` | `leicht`, `mittel` or `schwer` for both bots, or `leicht,schwer` for Pong A and Pong B. A shot misses 30, 15 or 6 times in a hundred |
| `PONG_FAULTS` | none | Faults for every game, e.g. `wrong_half=0.05,resend=0.1,correct=true`: names from the table above with a share from 0 to 1, and `correct`. An unknown name or a share out of range stops the service at startup |
| `PONG_SEED` | the clock | The n-th game of a service started with the same seed draws the same rallies. A clock seed is logged at startup, so a run can be repeated |
| `PONG_ATTRACT_AFTER` | none | Attract mode: how long the table must be free before a demo starts, e.g. `10m`. Unset is off |
| `PONG_ATTRACT_PACE` | `1s` | The demo's pause between two reports |
| `PONG_BROWSER_IDLE` | `10m` | How long a browser game waits for a press before it ends itself and its match |
| `PONG_CHECK_TIMEOUT` | `10s` | How long a button check's prompt waits for its press before it counts as missing |

Every duration is a Go duration and must be above zero; anything else stops
the service at startup.

It answers `/healthz` the way zaehlwerk does, with its version, and does not
ask zaehlwerk: a zaehlwerk that is down is a game that cannot start, not a pod
to restart.

**Deployment.** The image is scanned with Trivy like zaehlwerk's, because in
attract mode it runs unattended. On Kubernetes it is an optional part of the
KCL module: `config.pongEnabled` renders its own Deployment, Service and
ConfigMap next to zaehlwerk's, `config.pongHttpRouteEnabled` a route, and
`ZAEHLWERK_URL` is zaehlwerk's Service. Off, the render does not change by a
line. The options are in [kcl/README.md](kcl/README.md#zaehlwerk-pong).

## CI

Two workflows, on every pull request and on main.

| | |
| --- | --- |
| **Build & Test** | golangci-lint, the suite with `-race` and a redis-stack bound, and a smoke test that plays a point through the built binary. Plus govulncheck |
| **Build, Push & Scan** | the image, built with ko and pushed to ghcr.io — `pr-<n>` on a pull request, `main` on main, the version and `latest` on a release tag — then scanned with Trivy; zaehlwerk-pong's image the same way |

The first three go through [dagger/main.go](dagger/main.go), which hands
everything that is the same in every Go service here to
[stuttgart-things/dagger/go](https://github.com/stuttgart-things/dagger) and
keeps the two things that are not:

- **the test run binds a redis-stack** and sets `REDIS_TEST_ADDR`, so the panel
  tests run instead of skipping themselves — they are the ones that would notice
  a change in what reaches the stream. With `-race`, because a scorer read by
  three observers under its own lock has no other kind of bug worth catching.
- **the smoke test plays a point through the running binary** — a real listener,
  a real Redis, and the match reading back what was scored on it. That seam is
  the one a green unit suite hides.

govulncheck and the Trivy scan are the reusable stuttgart-things workflows, so
they are the call every other repository makes. **Both start report-only**: a
gate whose first run is red teaches people to ignore it, and neither list has
been read yet. `fail-on-finding` in
[build-test.yaml](.github/workflows/build-test.yaml) is the flip.

The same steps locally, if you want to reproduce a red run rather than guess:

```bash
task ci:lint     # golangci-lint in the container CI uses
task ci:test     # -race, with the redis-stack
task ci:smoke    # a point through the binary
task ci:vuln     # FAIL=true to make it a gate
task ci:image    # ko, pushed to ttl.sh for an hour
```

They need a `dagger` CLI and a container runtime, and they are slower than
`task check` — which stays the fast gate before a commit.

## Releases

[release-please](https://github.com/googleapis/release-please) reads the
conventional-commit history, keeps a release pull request open with the
changelog it would write, and on merge tags the commit and creates the GitHub
release. Nothing is released by hand and no version is typed anywhere.

The binary is stamped at build time and says so:

```console
$ docker run --rm -p 8080:8080 ghcr.io/stuttgart-things/zaehlwerk:v0.1.0
$ curl -s localhost:8080/healthz
{"status":"ok","version":"v0.1.0","commit":"cd68a93f3d9b1e84bf8b6f1d964b7dc271682142","date":"2026-09-08T08:28:33Z"}
```

The values come from the shared ko workflow, which exports `VERSION`
(`git describe --tags --always`), `COMMIT` and `BUILD_DATE` to the build. An
unstamped build — a local `go build`, or `task ci:image`, which goes through
dagger and sets none of them — reports `dev` rather than an empty string, which
is the honest answer and not a broken endpoint.

**A release tag builds its own image.** GitHub suppresses workflow triggers for
events created with the `GITHUB_TOKEN`, and release-please tags with exactly
that token, so a `push: tags` trigger would never fire. The release workflow
therefore dispatches the image build at the new tag explicitly.

### There is no `/readyz`, and that is the answer

`/healthz` is the only probe, and a readiness probe would ask the same question
twice. Ready and live differ where a process is up but cannot yet serve — it is
still opening a database, warming a cache, waiting on something it needs. This
service has none of that. It holds its state in memory, so there is nothing to
load; the panel sink and the catcher are optional by design, so their absence is
a configuration and not an outage; and a match is served from the registry
whether or not anything downstream is reachable.

So `/healthz` deliberately touches neither Redis nor the omni-pitcher. It says
the process is up and serving, which here is the whole of what a scheduler needs
to know, and it stays a liveness probe rather than quietly becoming a
dependency check that would take the pod down when the panel is off.

The day this service gains something it must wait for — persistence is the
obvious candidate, and `docs/adr/0004` explains why it does not have any — that
is the day to add `/readyz`, and it should arrive with the thing it waits on.

## Kubernetes

Manifests live in [kcl/](kcl/) as a KCL module, rendered into a kustomize base
and published to GHCR as an OCI artefact that Argo CD or Flux consumes.

```sh
task kcl:render      # the manifests, --- separated
task kcl:check       # do all profiles still render?
task kcl:apply       # to the current kube-context
```

CI publishes the base to `ghcr.io/stuttgart-things/zaehlwerk-kustomize`
alongside the image and under the same name — the version on a release tag,
`main` on main — so an Application pinned to `v0.1.0` gets the manifests that
belong to that image rather than to whatever the profile said last.

[kcl/README.md](kcl/README.md) is the module: what each profile switches on,
what an environment patches, and why `kcl run` directly is the way it goes
wrong. Two things from it are worth knowing before reading any of the rest.

**One replica, and the schema refuses more.** The running match lives in this
process's memory and this service is its only writer (ADR-0001). A second pod
does not share the load, it keeps a second score — a point routed to one pod is
invisible to the other, and no session affinity fixes that, because the panel
is fed by whichever pod took the point. The strategy is `Recreate` for the same
reason: a rolling update would run two processes that each believe they own the
match.

**The route belongs on an internal gateway.** There is no authentication in
this service, so the route is the access control: whoever reaches the hostname
can start a match, score it and end it. The single rule at `/` is a deliberate
refusal to pretend a path split protects anything — the scoring page is
self-contained under `/ui`, but it grants everything the JSON API grants, so a
narrower rule would shrink the surface without removing a capability.

**The panel stays outside.** Redis and the led-catcher are things this service
talks to, not things it owns. They outlive any revision of it and are shared
with everything else on the homerun bus, so a base that carried them would be a
base whose removal can prune somebody else's panel.

## Decisions

| ADR | Subject |
| --- | ------- |
| [0001](docs/adr/0001-independent-scorekeeping-api.md) | Why the running match state lives in its own service |
| [0002](docs/adr/0002-idempotent-ingest-contract.md) | The `ScoreEvent` shape and why ingest is idempotent |
| [0003](docs/adr/0003-led-catcher-stream-ownership.md) | Who may switch the LED catcher's stream, and when it switches back |
| [0004](docs/adr/0004-reporting-results-to-schmetterpause.md) | Handing the finished match to Schmetterpause |
| [0005](docs/adr/0005-match-phases-and-table-halves.md) | Warm-up, the serve rally and the change of ends; halves instead of players |
| [0006](docs/adr/0006-a-game-as-an-ingest-source.md) | zaehlwerk-pong: a game that scores through `/ingest/web` and is never reported |
| [0007](docs/adr/0007-a-grace-period-before-reporting.md) | A grace period before reporting, so an undo can still take the match point back |
| [0008](docs/adr/0008-demo-matches.md) | Demo matches: a real match always wins the table |
| [0009](docs/adr/0009-pong-on-two-devices.md) | Pong on two devices: the host reports, the guest only moves a paddle |
| [0010](docs/adr/0010-check-in-per-table-half.md) | Check-in per table half: a fob claims a half through Schmetterpause, two claims start a match reported without an operator |
| [0011](docs/adr/0011-check-in-by-phone-through-schmetterpause.md) | Check-in by phone: Schmetterpause claims a half for a player with `POST /checkin/claim` |

## Related

| Project | Role |
| ------- | ---- |
| [zaehlwerk-firmware](https://github.com/stuttgart-things/zaehlwerk-firmware) | ESP32 buttons, piezo units and the ESP-NOW hub |
| [homerun2-led-catcher](https://github.com/stuttgart-things/homerun2-led-catcher) | Renders the score on the RGB LED matrix |
| [homerun-library](https://github.com/stuttgart-things/homerun-library) | Message types and the Redis Streams pitcher |
| [Schmetterpause](https://github.com/stuttgart-things) | Takes the finished result |

## Status

All five issues are in: the scorer, the ingest endpoints, the panel sink, the
live stream, and switching the LED catcher's stream for the duration of a
match. The browser scoring page came after them, on top of exactly those.

```bash
task check       # gofmt, vet, lint, tests with -race
task test:redis  # plus the panel tests that need a real redis-stack
```