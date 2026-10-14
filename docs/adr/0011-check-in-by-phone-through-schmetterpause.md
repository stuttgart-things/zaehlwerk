# 11. Check-in by phone through Schmetterpause

Status: Accepted
Date: 2026-10-14

## Context

ADR-0010 decided check-in per table half for the fob and left the phone open:
"When Schmetterpause has a route for it, it ends in the same claim, with
Schmetterpause calling in rather than the hub." Schmetterpause has now decided
its side (its ADR-0026, schmetterpause#329):

- A QR code hangs at each half and opens `/table/left` or `/table/right` over
  there. A signed-in player confirms with a button, and **Schmetterpause**
  records the check-in (`method = 'phone'`) and then tells this service which
  player, with which check-in, claims which half.
- It calls with the address it already has for this service
  (`SP_ZAEHLWERK_URL`) and the secret both sides already share
  (`SP_SCOREBOARD_TOKEN` there, `SCHMETTERPAUSE_TOKEN` here), in the other
  direction. It waits briefly, does not retry in a loop, and shows the answer
  to the player in German.
- It does not ask this service to check anything: the result is checked
  against the check-ins over there anyway (its ADR-0024).

So the phone never talks to this service, and this service never learns who a
phone is. What it needs is a door for Schmetterpause into the claim of
ADR-0010.

## Decision

**Schmetterpause claims a half with `POST /checkin/claim`:**

    Authorization: Bearer <SCHMETTERPAUSE_TOKEN>
    { "half": "left", "player_id": "<uuid>", "checkin_id": "<uuid>" }

The body carries what a fob resolve returns, so there is no resolve call. From
there on it is the fob's claim, through the same desk: the same two halves,
`CHECKIN_TIMEOUT`, the same rules for the same player again, another player,
or the other half, and both halves claimed starts the match. Its result
carries `home_checkin_id` and `away_checkin_id` and no `operator_id`. One half
by fob and the other by phone is the ordinary case: a claim is a player id and
a check-in id, and the desk does not care how they arrived.

**The answer is `/ingest/checkin`'s**, body and status, so the two paths read
alike and Schmetterpause can map one table of outcomes to German: 200 for
`claimed`, `unchanged`, `replaced`, `moved` and `started`, 409 `refused` while
a real match holds the table, 503 when check-in is off, 400 for a body that is
not a claim (a half that is not `left` or `right`, an id that is not a UUID),
401 for a missing or wrong token. `unknown` and `duplicate` cannot happen on
this path: there is no fob to be unknown and no counter to fall behind.

**The token is `SCHMETTERPAUSE_TOKEN`**, compared in constant time before the
body is read, with a `WWW-Authenticate` header, as the hub's is. A second
secret would have to be distributed, kept in the vault and rotated, and would
protect nothing the first does not: whoever holds it can already enter results.
`CHECKIN_TOKEN` stays the hub's and is not accepted here, and this token is not
accepted on `/ingest/checkin`: each door has one caller.

**The route is on only with `SCHMETTERPAUSE_URL` and `SCHMETTERPAUSE_TOKEN`**,
and does not need `CHECKIN_TOKEN`. Without them it answers 503, like
`/ingest/checkin` without its token: the route exists, the feature does not,
and a 404 would read as a wrong URL to whoever sets Schmetterpause up. The desk
is built whenever Schmetterpause is configured and either path is on, so a
table with no hub can still check in by phone, and `/ingest/checkin` answers
503 and 401 exactly as before. One line at start says, for each path, whether
it is on and why not.

**A repeated claim with the same `checkin_id` claims once** and gets the first
answer with `"repeated": true`, also while the first is still under way. It is
the idempotency of ADR-0002 with the key the caller already has: Schmetterpause
creates a check-in per press of the button, so its id names exactly one
attempt. The path has no `source` and counter because a counter would be a
second, weaker name for the same thing. The record is bounded, the last 256
check-in ids, which is far more than one table sees in the minutes a claim
lasts. A double tap over there that creates two check-ins is two claims, and
the second is `unchanged`: the same player at the same half keeps the newer
one.

**Nothing is checked against Schmetterpause.** A player id and a check-in id
that do not belong together, or do not exist, make a result Schmetterpause
refuses with a 422, from the one place that can tell. Asking first would be a
second call per claim to learn what the caller just said.

**The display name** for the panel and the answer comes from `/api/players`
after the claim is accepted, as for the fob. The body stays at three fields: a
name in it would be a second source for something the player list already
says. A list that does not load leaves the name empty, and the match is
labelled A and B.

**The ids stay where ADR-0010 put them**: on the claim and the match's
handover. Neither a log line nor an answer nor `GET /checkin` carries them.
`GET /checkin` says per path whether it is on (`fob`, `phone`), and `enabled`
is either.

## Consequences

- A table without a hub can check in. Two phones start a match that is
  reported without an operator, as two fobs do.
- The coupling now runs both ways, but on one shared secret and one address on
  each side. A deployment that sets `SCHMETTERPAUSE_TOKEN` here turns the phone
  path on; the README says so beside the variable.
- Schmetterpause leaves an unused check-in for every claim that is refused,
  replaced, moved or timed out, as the fob path already does. They expire there
  after three hours.
- Anybody who knows the static QR URL and has an account can claim a half from
  outside the room, for themselves only, and a match starts only when an
  opponent claims the other half. A rotating code on the panel would close
  that; it is deferred on both sides until somebody misuses the fixed one.
- One table, as in ADR-0010. Several tables need a table id in the path or the
  body here and in the check-in there (schmetterpause#280).
