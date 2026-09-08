# 4. Reporting results to Schmetterpause

Status: Accepted
Date: 2026-09-08

## Context

ADR-0001 already decided this: "Schmetterpause keeps players, TTR, tournaments
and history, and receives a finished result at match end." `State.CompletedSets`
exists for it and says so. Nothing implements it, and until the browser scoring
page there was nowhere to choose a player from anyway — a match was two names
typed into a shell loop.

Two things are missing, and they are the two halves of the same coupling: the
list of real players, so that a result can belong to somebody, and the finished
result itself.

Neither may become a requirement. This service is meant to run next to a table
with a matrix and no other infrastructure — `task demo`, two names, a panel.
The catcher and the omni-pitcher are both wired that way already: an environment
variable turns them on, its absence turns them off with a log line and changes
nothing else.

There is also nothing here to store an identity in. This service has no
database — no `database/sql`, no driver, nothing. A match lives in the registry
while it is played and is forgotten once `MAX_RETAINED_MATCHES` pushes it out.

## Decision

`SCHMETTERPAUSE_URL` enables the coupling, in the shape `CATCHER_URL` and
`OMNI_PITCHER_URL` already have. Unset, this service behaves exactly as it does
today.

**Players are fetched, never held.** The new-match form asks Schmetterpause for
the list each time it is rendered. No copy, no cache, no reconciliation. A
player who is not there cannot be chosen, and one chosen here cannot be invented.

**The page names three people, not two.** The two playing, and whoever is
keeping score. Schmetterpause's ADR-0014 requires every write to name an
operator and refuses one who is playing; that is not friction to work around
but a description of what is already happening — somebody is standing at the
table watching, and that is who is typing.

**The chosen ids live on `match.Match` for the length of the match and nowhere
else.** Not in the scorer, which stays rule-only and knows players as two
display names: an id is not something set logic can have an opinion about.
Nothing is written to disk, nothing survives a restart, and no player is known
here for a second longer than the match lasts.

**The result is posted when the match is won**, from an observer on
`TransitionMatchWon` — the same seam the panel sink hangs on. `CompletedSets`
is the payload it was written for.

**A match without Schmetterpause is still a match.** Free-text names, no ids,
no operator: scored, displayed, pitched to the panel, reported nowhere.

## Consequences

- Schmetterpause is the only owner of player identity. This service cannot show
  a player it does not have and cannot create one.
- A rename over there mid-match changes nothing here: the id was taken when the
  match started, and the display name on the panel is a label rather than a key.
- **A result can be lost.** If Schmetterpause is unreachable when the match ends
  there is no queue to fall back on, because there is nowhere to queue it. The
  post fails loudly, the page says the result did not reach Schmetterpause, and
  the match stays in the registry until retention drops it — long enough to
  retry it from the page, and after that it is a hand entry like any other. A
  store-and-forward buffer would mean giving this service persistence, which is
  a larger decision than this one.
- The result waits for one of the two players to confirm before it reaches TTR
  (Schmetterpause ADR-0015). Points on the matrix are immediate as they always
  were; the two clocks are different and that is intended.
- One more outbound dependency to configure, and one more that a local run does
  not need.
