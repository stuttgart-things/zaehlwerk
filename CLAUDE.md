# zaehlwerk

Live scorekeeping for table tennis: points arrive from wireless buttons, piezo
sensors or a phone, and the running score goes to an RGB LED matrix, to a
browser, and at match end to Schmetterpause. Go, no framework, no database.

## Invariants

These hold for every change. A task that breaks one is a question, not a
commit.

1. **This service owns the running match.** It is the only writer of that state
   and fans out to its consumers. It never asks anybody else what the score is.
2. **No persistence.** State is in memory, bounded by `MAX_RETAINED_MATCHES`,
   and a finished match is forgotten when it falls out. There is no
   `database/sql`, no driver, no file the service writes to. Adding storage is
   an ADR, not a commit — `docs/adr/0004` accepts a lost result rather than
   quietly introducing a queue.
3. **Configuration only through environment variables.** No flags, no config
   file, no hardcoded hosts. Defaults live in the code, and the table in
   `README.md` is the whole surface.
4. **Every outbound coupling is optional.** `CATCHER_URL`, `OMNI_PITCHER_URL`,
   `REDIS_ADDR`: set turns it on, unset turns it off with a log line saying so
   and changes nothing else. A run at the table needs none of them, and a new
   one is written the same way.
5. **The scorer is rule-only.** Set logic, service rotation, deduplication —
   no HTTP, no storage, no player identity. Anything derivable is derived and
   not stored, because a stored field is one that undo and set boundaries can
   leave out of step with the score.
6. **Ingest is idempotent, and new input paths use the contract.** `source` plus
   a counter that only goes up (`docs/adr/0002`). A new way of scoring counts
   the way a phone does rather than getting a door of its own — the browser page
   is the worked example.
7. **The led-catcher stays a generic sink.** Table tennis points are one event
   source among several, filtered by a display rule. No set logic in the
   catcher, ever.
8. **No JavaScript framework.** htmx, and hand-written JS only where it does not
   reach — currently the per-page event counter and the keyboard shortcuts.

## Architecture decisions

Read `docs/adr/` before changing the ingest contract, the panel path or the
Schmetterpause coupling. A change that contradicts an ADR needs a new one that
supersedes it, not a quiet rewrite.

| | |
| --- | --- |
| 0001 | Independent scorekeeping API — why this is its own service |
| 0002 | Idempotent ingest contract — `source` + `event_id` |
| 0003 | Stream ownership when switching the LED catcher |
| 0004 | Reporting results to Schmetterpause |

## Terms

- **Source** — who is counting: one wireless button, one piezo, one browser
  page. Each keeps its own counter, so two phones on one match never collide.
- **Event ID** — that source's counter. It only goes up; a repeat is a
  duplicate and is discarded, which is what makes a retried request safe.
- **Transition** — what applying an event did: `point`, `set_won`, `match_won`,
  `undo`. Observers hang off it — the panel sink, the live hub, and the
  Schmetterpause reporter when it exists.
- **The panel** — the 64×64 LED matrix, driven by homerun2-led-catcher. It
  displays what it is told and holds it; the timing rules are in `README.md`.
- **The catcher** and **the pitcher** — the two ends of the homerun bus. This
  service pitches; the catcher displays.

## Conventions

- **Everything is English.** Code, comments, log and error messages, ADRs,
  README, commits, issues and pull requests. This differs from schmetterpause,
  which is German for anything a player reads — do not carry its split over
  when working across both.
- **Conventional commits, without scopes.** `feat:`, `fix:`, `docs:`, `chore:`.
  schmetterpause writes `fix(ui):`; we do not. Branches carry the same type:
  `feat/browser-scoring-ui`, `docs/claude-md`.
- **ADRs** are `docs/adr/NNNN-kebab-case.md`, numbered in order, with
  `# N. Title`, `Status`, `Date`, and the sections Context, Decision,
  Consequences. There is no generated index and no link check here — adding one
  is a decision, not a chore.
- **`task check` is the gate**: `gofmt`, `go vet`, `golangci-lint`, and
  `go test ./... -race`. It is what CI runs. `task test:redis` additionally
  starts a real redis-stack for the tests that skip themselves without one.
- **Tests use `testify/require`** and read as sentences —
  `TestThePageJoinsTheRunningMatch`, `TestAWonMatchReleasesThePanelThroughTheObserver`.
  A test name says what is true, not which function is under test.
- **Comments carry the reason, at the place it would otherwise be lost.** This
  is deliberate and it is why `panel:ensure-consuming` explains what an empty
  panel looks like, and why the braces in `board.html` say what htmx does to an
  expression without them. Both cost somebody an afternoon. Do not trim a
  comment that answers "why is it like this" — write one when the answer is not
  obvious from the code.
- **Errors are wrapped** (`fmt.Errorf("…: %w", err)`), not swallowed.

## Where things are

`internal/scorer` is the rules. `internal/match` holds running matches and fans
transitions out to observers. `internal/api` is the JSON API, `internal/ui` the
browser page, `internal/panel` the way to the matrix, `internal/live` the SSE
hub. `cmd/zaehlwerk-api` wires them from the environment and is the only place
that reads it.

`README.md` covers running it, the API and the panel; this file does not repeat
it.
