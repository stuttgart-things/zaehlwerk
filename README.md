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

htmx and its SSE extension come off a CDN, the way the led-catcher's simulator
loads them: the browser opening the page needs to reach `unpkg.com` once, and
nothing else in the service does.

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
| `tags` | `match=<id>,set=<n>` |

The catcher renders `{{ title }}` and nothing else, so the score has to fit in
a title: a 6x10 font at x=2 on a 64x64 panel is about ten glyphs, and anything
longer is not shortened, it runs off the edge. A test enforces the limit across
every score a match can reach.

`SET` and `WIN` are prefixed because a set score of `2:1` and a point score of
`2:1` are otherwise the same three characters, and the panel would be ambiguous
exactly when it matters. Whether that is the right wording is a decision for
the table — the simulator below is what makes it decidable without hardware.

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

## CI

Two workflows, on every pull request and on main.

| | |
| --- | --- |
| **Build & Test** | golangci-lint, the suite with `-race` and a redis-stack bound, and a smoke test that plays a point through the built binary. Plus govulncheck |
| **Build, Push & Scan** | the image, built with ko and pushed to ghcr.io — `pr-<n>` on a pull request, `main` on main, the version and `latest` on a release tag — then scanned with Trivy |

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