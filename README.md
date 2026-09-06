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
go run ./cmd/zaehlwerk-api
```

| Variable | Default | Meaning |
| -------- | ------- | ------- |
| `HTTP_ADDR` | `:8080` | Listen address |
| `LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN`, `ERROR` |
| `MAX_RETAINED_MATCHES` | `200` | Finished matches kept in memory; a running one is never dropped |
| `REDIS_ADDR` | — | Redis holding the panel stream. Unset disables the panel entirely |
| `REDIS_PORT` | `6379` | |
| `REDIS_PASSWORD` | — | |
| `PANEL_STREAM` | `tabletennis` | Stream the score is published to |
| `ALLOWED_ORIGINS` | — | Comma-separated browser origins that may read the live stream. Empty means none |
| `STREAM_HEARTBEAT` | `25s` | Keepalive interval on an idle stream |
| `CATCHER_URL` | — | LED catcher base URL. Unset disables stream switching |
| `CATCHER_MATCH_STREAMS` | `tabletennis` | What the catcher listens to during a match |
| `CATCHER_IDLE_STREAMS` | `messages` | What it goes back to |
| `CATCHER_IDLE_TIMEOUT` | `20m` | Give the panel back after this long without a point |

State is in memory and deliberately so — a match lasts twenty minutes and the
finished result goes to Schmetterpause.

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
curl -s -X POST localhost:8080/matches/{id}/undo         # 409 if there is nothing to take back
curl -s -X POST localhost:8080/matches/{id}/end          # idempotent
```

`POST /matches` takes `players`, `best_of`, `points_per_set` and `first_server`,
all optional — the defaults are a best of five to eleven with `a` serving.

Undo is not gated on the match still running: taking back a wrongly awarded
match point is exactly when it is needed. Ending a match is not the same as
winning one, and both stay readable afterwards.

### Ingest

All three carry the four fields the ESP-NOW payload already has — `source`,
`player`, `delta`, `event_id` — because the hub forwards what it received and
one vocabulary across firmware, adapter and scorer is worth more than three
spellings of the same thing.

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

JSON rather than rendered HTML: the led-catcher's own UI swaps HTML partials
over SSE because htmx does the swapping there. Here the scoring frontend, a
spectator view and Schmetterpause will each render differently, and a shared
fragment would constrain all three to one layout.

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
| `tags` | `match=<id>,set=<n>` |

The catcher renders `{{ title }}` and nothing else, so the score has to fit in
a title: a 6x10 font at x=2 on a 64x64 panel is about ten glyphs, and anything
longer is not shortened, it runs off the edge. A test enforces the limit across
every score a match can reach.

`SET` and `WIN` are prefixed because a set score of `2:1` and a point score of
`2:1` are otherwise the same three characters, and the panel would be ambiguous
exactly when it matters. Whether that is the right wording is a decision for
the table — the simulator below is what makes it decidable without hardware.

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
browser, so the whole path is checkable with no hardware:

```bash
docker compose -f deploy/panel/compose.yaml up -d
REDIS_ADDR=localhost go run ./cmd/zaehlwerk-api
```

Simulator on <http://localhost:8081>, Redis on `localhost:6379`. Set
`ZW_REDIS_PORT` and `ZW_SIMULATOR_PORT` if either is taken.

It runs `LED_MODE=full` rather than `web` on purpose. `full` also loads the
hardware handler, which draws nothing without the rgbmatrix bindings but keeps
its timing — and the timing is the part that bites.

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

## Decisions

| ADR | Subject |
| --- | ------- |
| [0001](docs/adr/0001-independent-scorekeeping-api.md) | Why the running match state lives in its own service |
| [0002](docs/adr/0002-idempotent-ingest-contract.md) | The `ScoreEvent` shape and why ingest is idempotent |
| [0003](docs/adr/0003-led-catcher-stream-ownership.md) | Who may switch the LED catcher's stream, and when it switches back |

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
match.

```bash
go test ./... -race
golangci-lint run ./...

# The panel tests that go through a real redis-stack are skipped without this.
docker run -d --name zw-redis -p 6399:6379 redis/redis-stack-server:latest
REDIS_TEST_ADDR=localhost:6399 go test ./internal/panel/ -race
```