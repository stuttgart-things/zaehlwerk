# 17. Tournament matches through zaehlwerk

Status: Proposed
Date: 2026-10-24

> Supplements [ADR-0004](0004-reporting-results-to-schmetterpause.md) (the
> handover carries a tournament slot) and
> [ADR-0015](0015-pong-plays-reported-matches.md) (Pong's league mode plays
> tournaments first). Its counterpart is Schmetterpause ADR-0028, which owns
> the draw.

## Context

Pong's league mode plays one match after another between two of
Schmetterpause's players, drawn at random (ADR-0015). Single matches work, on
the sandbox and on the office panel. A tournament does not: a tournament match
has a place in the draw, a round and a pairing, and the final is the two the
table names once the group is played. Today zaehlwerk has no way to say which
place a match fills, so every result it reports is a casual match.

Schmetterpause ADR-0028 decides where the draw lives: there, computed and never
stored. It offers `GET /api/tournaments` (the open ones, with their mode and
side names) and `GET /api/tournaments/{id}/pairings` (the open places, final
included once it is known, `home` being side A), and `POST /api/results`
takes `tournament_id` and `round` and checks the place with the same check the
kiosk and the phone use.

What is left for this service is to carry the place from the start of a match
to its report, and for Pong to play the places.

## Decision

**A tournament slot is part of the handover. zaehlwerk carries it and checks
nothing about it, and Pong's league mode plays open tournament places before
it draws random pairs.**

### The handover

- **`match.Handover` gains `TournamentID` and `Round`**, both or neither, set
  when the match is created and never changed after, like the player ids. A
  slot without the two players is refused: it is a place for those two.
- **The reporter sends them** as `tournament_id` and `round` on
  `POST /api/results`, and only when set. Schmetterpause's decoder refuses
  unknown fields, so a zaehlwerk talking to a Schmetterpause from before
  ADR-0028 keeps working for every match that is not a tournament one.
- **Nothing is checked here.** Whether the tournament is open, the pairing in
  the draw, the place free, the final decided: Schmetterpause checks all of it
  when the result arrives, with one function for every path. A refused
  tournament result is logged and dropped like any refused report (ADR-0004):
  `409` means the place was filled meanwhile, at the kiosk or by another table.
- **The mode is the tournament's.** Whoever starts the match takes `best_of`
  and `points_to_win` from `GET /api/tournaments`, and the scorer counts by
  them. Schmetterpause refuses a result in another mode rather than rewriting
  it.

### The doors

- **`POST /matches`** takes `tournament_id` and `round` next to the ids of
  ADR-0015, behind the same `MATCHES_TOKEN`, and refuses them without
  `home_id` and `away_id`. Home is player `a`, at the left half (ADR-0005),
  which is side A of the tournament (Schmetterpause ADR-0013).
- **The scoring page** offers a tournament match later, from the same two
  lists: an open tournament, then one of its open places, with the players and
  the mode filled in. It is a change of its own and does not change the
  handover above.
- **Check-in** does not select a tournament yet. Two players checked in at the
  halves can be a pairing of more than one open tournament, and choosing one
  for them is a question for the table, not for the desk.

### Pong

- **League mode plays tournaments first**, with `PONG_TOURNAMENTS=on`
  (default off). Before each match it reads `GET /api/tournaments` and takes
  the oldest open tournament that has an open place; of its places, the first
  in the draw's order. It starts that match with the slot, the tournament's
  mode and the players' strengths from `PONG_PLAYER_SKILL` as before.
- **When no tournament has an open place**, it draws a random pair as today.
  So a tournament created in the sandbox's Schmetterpause is played to its
  final while the league waits, and the league goes on afterwards.
- **The final comes by itself.** Schmetterpause lists it once the table has
  decided it, which with `SP_SCOREBOARD_SETTLE=immediate` is right after the
  last group match is reported. Pong waits `PONG_LEAGUE_PAUSE` after every
  match, longer than the report's grace period, so the final is listed by the
  time it asks.
- **Against a Schmetterpause without ADR-0028** the tournament list is a 404:
  Pong logs it once and plays the league.

## Consequences

- One wire change in each direction, both additive: `POST /matches` takes two
  more optional fields, and the report sends two more only when they are set.
- The sandbox can play a whole tournament, group and final, between bots of
  known strength. Whether the table and the final come out right is then
  something to look at, not to set up by hand.
- A tournament match on the panel looks like any other. The tournament's name
  is not on the panel; the catcher would need a tag it has no rule for.
- A pod restart mid-tournament loses nothing: the next start reads the open
  places again, and the match that was running is ended on SIGTERM and its
  place stays open.
- Two Pongs against one tournament would play the same place twice, and the
  second report would be refused with `409`. There is one Pong per sandbox,
  and one game at a time per Pong.
