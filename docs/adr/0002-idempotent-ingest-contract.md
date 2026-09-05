# 2. Idempotent ingest contract

Status: Accepted
Date: 2026-09-05

## Context

Points arrive from three sources: wireless buttons over ESP-NOW, piezo sensors,
and a web frontend. All three can deliver the same point twice.

A button bounces mechanically. An ESP-NOW send that is not acknowledged gets
retried by the sender, which has no way to know whether the first attempt was
processed. A phone on flaky wifi retries an HTTP POST the same way. A piezo
picks up a bounce off the table edge as a second hit.

A duplicate point is not a cosmetic problem: it changes who wins the set.

## Decision

Every ingest source sends a `ScoreEvent`:

    { "match_id": "...", "player": "a", "delta": 1,
      "event_id": 7, "source": "button-a" }

`event_id` is a counter, monotonic per `source`. The scorer keeps the highest
id seen per source and discards anything at or below it.

The three ingest endpoints (`/ingest/button`, `/ingest/piezo`, `/ingest/web`)
normalise their own wire formats into this shape. Source-specific concerns stay
in the adapter: the piezo adapter may emit `delta: 0` for a hit it cannot
attribute to a player, and debounce windows are an adapter concern, not the
scorer's.

Duplicates are answered with the current state and HTTP 200, not an error. A
retrying sender should stop retrying.

## Consequences

- Senders can retry freely, which is what makes the ESP-NOW path viable at all.
- Firmware must persist its counter across deep sleep — RTC memory, not a plain
  variable, or every wake resets it to zero.
- A firmware reflash resets the counter. The scorer therefore accepts a counter
  that jumps backwards to a low value if the source has been quiet, rather than
  locking the source out until the next match.
- The scorer never sees source-specific quirks, so a fourth source costs one
  adapter and nothing else.