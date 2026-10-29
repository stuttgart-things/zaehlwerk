# 18. The faces come from the game

Status: Accepted
Date: 2026-10-27

> Counterpart of Schmetterpause ADR-0029, which lets every player assemble
> their own figure and leaves its face to this service. Tracked in #195.

## Context

Schmetterpause v0.20.0 lets a player build a figure — fur, headdress, handle,
blade, bling — and shows it beside the running score it reads from
`GET /live/stream`. What a player cannot choose is the face. A face picked once
in a profile would grin through a 2:11, and the moment worth a face is a moment
of the match: the point just won, the third one lost in a row, the set point
against. This service is the one that knows the match, so Schmetterpause
ADR-0029 hands the face over to it and proposes a mapping. Until this service
sends one, the figures there stay neutral.

Three questions follow: where the mapping lives, what it remembers, and what
goes on the wire.

## Decision

**A pure function in its own package, `internal/face`.** It takes a moment of
the match — the kind of event, the side it went to, the state after it, who
holds a set or match point, the length of a set — and returns one face per
side. It is not in the scorer: a face is not a rule of table tennis, and the
scorer stays rule-only (invariant 5). It asks the scorer what the rules say
(`Stake`, `Rules`) rather than knowing them.

**The mapping:**

| Moment | Face |
| --- | --- |
| warm-up, change of ends, anything without a point | neutral, both |
| point won | happy |
| point lost | skeptical |
| third point lost in a row, and every one after | angry |
| deuce | surprised, both |
| set point or match point against | surprised, for the side facing it |
| set won / lost | happy / sad |
| match won / lost | happy / skeptical — never sad |
| undo, or a correction that lowered a score | as at rest |

"At rest" is what the score says by itself: neutral, surprised where it is
deuce or a side faces a set point, happy and skeptical once the match is
decided. It is also what a page sees when it joins mid-match.

Where two rows meet, angry wins over surprised (three lost in a row and a set
point against is not surprise), and surprised wins over the plain point faces.
The side that holds a set point keeps the face its point gave it.

The briefing in Schmetterpause rules out a sad face after a lost match. After a
lost set, with the match still open, a sad moment is what the table actually
has; what stays after the match is skeptical.

**A face lasts until the next event.** ADR-0029 proposed "until the next event
or a few seconds, whichever comes first". The few seconds are left out: they
would need the stream to send something that is not an event of the match, a
timer per connection, and a rally is a few seconds anyway. A won set or match
keeps its faces because nothing follows them until play goes on.

**The streak is counted per connection.** Three in a row needs memory, and
invariant 5 keeps it out of the scorer. The stream that sends the faces counts
it, from the side each transition went to, and starts afresh with each match,
each set and each undo. A page that joins mid-streak counts from where it
joined, which is also what the people in front of it saw. For that the hub's
event carries the transition's `side` — the state after a won set cannot say
who won the point that took it.

**On the wire**, every event of `GET /live/stream` that has a `state` carries
`faces`: the face keys of side a and side b, in the order of `state.players`.
The keys are exactly Schmetterpause's `avatar.Faces`. A key Schmetterpause does
not know shows neutral there, and a consumer that does not read the field
(zaehlwerk-pong's `TableEvent`) is unaffected.

## Consequences

- Schmetterpause's running score pulls faces without a release of its own:
  v0.20.0 already reads `faces` and falls back to neutral without it.
- `GET /matches/{id}/stream` gains `side` on its events, the hub's new field.
  It does not carry `faces`; the per-match stream is for clients that render
  for themselves, and none of them has asked.
- Two pages following the same table can disagree for a few points about
  whether a side is angry yet, if one of them joined mid-streak. That is
  accepted; the alternative is a stored streak per match, which is state
  derived from the transitions that undo can leave out of step.
- A new face is a change in both repositories, here in the mapping and in
  Schmetterpause in the parts it draws.
- The spectator view shows each side's figure with these faces, counted the
  same way per connection on its own stream. It fetches the figures once per
  match from Schmetterpause and takes each side's colour, and so the referee's
  cord, from the blade on the figure rather than recomputing it from the id.
- The scoring page shows the same figures and faces, smaller: whoever keeps
  score is there for the buttons.
- zaehlwerk-pong shows the figures on its board as well, from the same cache
  (`internal/figure`). Its stream sends the board, not the transitions, so it
  reads the moment back from the change of the score, and a change that is no
  point keeps the faces up until the next one.
- The table mock (zaehlwerk-buttons) shows them too: a player's fob carries
  that player's figure, and the running score shows the match's figures from
  zaehlwerk's own route, with the faces `/live/stream` sent. It decides no face
  itself.
