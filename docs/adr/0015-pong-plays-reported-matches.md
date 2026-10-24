# 15. Pong plays reported matches in a sandbox

Status: Proposed
Date: 2026-10-24

> Supplements [ADR-0006](0006-a-game-as-an-ingest-source.md) and
> [ADR-0008](0008-demo-matches.md). Both stay as they are for every
> deployment that does not turn this on. Its counterpart on the other side
> is Schmetterpause ADR-0027.

## Context

Pong tests zaehlwerk: its rallies are decided before anything is sent, so it
holds a truth to compare the score with (ADR-0006). Nothing tests the rest of
the chain the same way. A result that reaches Schmetterpause is today always
a match a person played, and the parts of Schmetterpause that a result moves —
TTR, standings, later tournament tables and finals — are tried by hand, one
match at a time.

The idea is a sandbox: a namespace with its own Schmetterpause, its own
zaehlwerk and Pong, where bot players with individual strengths play match
after match, every result goes to that Schmetterpause, and the office can
watch it on the panel. Tournaments played through zaehlwerk come later and are
needed at the real table too; the sandbox is meant to be their first user, so
what it needs here is built the way the real table will use it.

Two decisions stand in the way, and both are right where they apply:

- **Pong never reports** (ADR-0006). It starts its own match with free-text
  names, so the match has no handover. A game must never create a result in
  the office Schmetterpause: it would end up in TTR and rankings, and skew the
  measurement of Schmetterpause ADR-0015.
- **A demo and a handover rule each other out** (ADR-0008). Every match Pong
  creates is a demo, and `Registry.CreateFor` refuses a demo with any id.

And one door is missing. The JSON API names players as free text, so a match
started through it is reported nowhere; only the scoring page and check-in
can create a match with a handover. Pong is a pure HTTP client of zaehlwerk
and cannot use either. `POST /matches` is also unauthenticated, which is fine
for a match that is reported nowhere and is not for one that names two
players.

## Decision

**`POST /matches` takes a handover, behind a token of its own.** The body
gains `home_id`, `away_id` and `operator_id`, Schmetterpause player ids as the
scoring page sends them. A body without any of them is what it is today:
unauthenticated, free text, reported nowhere.

- **`MATCHES_TOKEN` turns the door on.** A body that carries an id needs
  `Authorization: Bearer <MATCHES_TOKEN>`, compared in constant time before
  the match is created; missing or wrong is `401` with a `WWW-Authenticate`
  header, as on `/checkin/claim`. A second secret rather than
  `SCHMETTERPAUSE_TOKEN`, because each door has one caller (ADR-0011): this
  one's caller is the sandbox's Pong, and the token is set nowhere else.
- **The door needs reporting to be on.** Without `MATCHES_TOKEN`, or without
  `SCHMETTERPAUSE_URL`, a body with an id answers `503`: the route exists, the
  feature does not, as `/checkin/claim` answers without its configuration. A
  handover nobody can report would otherwise be accepted and silently go
  nowhere. One line at start says whether the door is on and why not.
- **All three ids or none.** A handover through this door is the page's:
  home, away and an operator. Check-in ids stay check-in's, so this door
  cannot claim that two people stood at the table (Schmetterpause ADR-0024).
- **Nothing is checked against Schmetterpause.** As on `/checkin/claim`,
  Schmetterpause checks the ids when the result arrives: players that exist,
  an operator who does not play. A wrong id costs a refused report and its
  log line, not a refused match.
- **A demo with ids is still refused**, by `CreateFor`, as today. A match with
  a handover is a real match: it ends running demos and blocks new ones
  (ADR-0008), and it is reported after the grace period (ADR-0007) like any
  other.

**Pong plays such matches in league mode, and only there.** League mode is on
with `ZAEHLWERK_TOKEN` (the `MATCHES_TOKEN` of the zaehlwerk it plays
against) together with the `SCHMETTERPAUSE_URL` and `SCHMETTERPAUSE_TOKEN` it
already reads `/api/players` with. Unset, Pong is exactly what ADR-0006 and
ADR-0008 describe, and the log says league mode is off.

- **It plays one match after another**, drawing two of Schmetterpause's
  players for each, and starts it with their ids, without `demo`. The operator
  is the account `PONG_OPERATOR` names, by display name, resolved from
  `/api/operators` at every start: in the sandbox an observer that stands for
  the machine (Schmetterpause ADR-0027).
- **Strength belongs to the player, not to the side.** `PONG_PLAYER_SKILL`
  maps a display name to a preset or to a miss share of its own, e.g.
  `Ada=schwer,Bert=0.2,Cleo=leicht`: the `Skill.Miss` that `leicht`, `mittel`
  and `schwer` already stand for, so a strength means the same in a headless
  game and in a physics one. A player it does not name plays at
  `PONG_BOT_SKILL`. Schmetterpause learns
  nothing about strength: it knows players, and whether one is good is what
  its TTR is meant to find out.
- **Its rallies are played by the server driver** (ADR-0014), so the match
  goes on with no screen and can be watched from `/play/{id}`. The truth is
  compared after every rally as in any game, and faults stay off: a league
  match is a test of what happens after the score, not of the score.
- **It reports nothing itself.** The result goes the way every result goes,
  from zaehlwerk's reporter. Pong waits for the next match until the grace
  period of the last one is over, so the results arrive in the order they
  were played.

## Consequences

- In the office nothing changes. `MATCHES_TOKEN` is not set there and
  `ZAEHLWERK_TOKEN` not in its Pong, so `POST /matches` answers a body with
  an id with `503`, and Pong plays demos only. The guard is a secret that
  exists in one namespace, not a flag somebody has to remember to leave off.
- **Two repositories have to agree on names.** `PONG_PLAYER_SKILL` and
  `PONG_OPERATOR` name accounts that the sandbox's seed in Schmetterpause
  creates. A renamed bot plays at the default strength and says so in the log;
  it does not stop the sandbox.
- A league match is a real match to zaehlwerk. A pod killed mid-match leaves
  it running, and the restarted Pong does not end a match it cannot prove is
  its own: it waits, as it does for any match it finds. Somebody ends it on the
  scoring page. Pong ends its own match on SIGTERM, as every game does.
- The comment on `createMatch` that says the JSON API names players as free
  text becomes false and is rewritten in the same change.
- The live stream already carries `home_id` and `away_id` for a real match.
  Schmetterpause's start page in the sandbox shows the running league match as
  it shows any.
- This is the door tournaments will use. A tournament match at the real table
  is started by the page or by check-in, but the sandbox's tournament loop is
  this door plus a pairing, and a tournament id on the handover is a later
  field, not a new door.
- How the sandbox shows up on the office panel is
  [ADR-0016](0016-the-sandbox-on-the-office-panel.md).
