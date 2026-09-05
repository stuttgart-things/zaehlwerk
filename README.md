# zaehlwerk

Live scorekeeping for office table tennis.

Points arrive from wireless buttons, piezo sensors or a phone. `zaehlwerk-api`
owns the state of the running match and fans it out — to the LED matrix via
homerun, and to browsers over SSE.

```
buttons ──ESP-NOW──> hub ──┐
piezo ─────────────────────┼──> zaehlwerk-api ──┬──> homerun (tabletennis) ──> LED matrix
phone ─────────────────────┘                    ├──> SSE ──> live view
                                                └──> Schmetterpause (result)
```

## Why a separate service

The running score has three consumers that must not disagree, and the point
sources are hardware. Neither Schmetterpause nor the LED catcher is the right
owner for that — see [ADR-0001](docs/adr/0001-independent-scorekeeping-api.md).

Schmetterpause keeps players, TTR, tournaments and history, and receives a
finished result at match end. The LED catcher stays a generic sink: table tennis
points are just another event source with its own `system` label.

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

## Status

Early. See the open issues for the intended cut: scorer, pitcher sink, ingest
endpoints, SSE, stream switching.