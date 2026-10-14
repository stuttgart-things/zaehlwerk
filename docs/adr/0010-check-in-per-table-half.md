# 10. Check-in per table half

Status: Accepted
Date: 2026-10-14

## Context

A match gets its players from the page: two names from the Schmetterpause
dropdown, and a third, whoever keeps score, because Schmetterpause's ADR-0014
refuses a result nobody vouched for (ADR-0004). At the table that is a phone
passed around before the first serve.

ADR-0005 left the next step open: "check-in per side attaches to the halves:
who checked in at the left half is `a`". Two ways in are planned (#92): a key
fob held to a reader at the hub and then a press on that half's button
(zaehlwerk-firmware#63), and a phone at a sticker by the table (#77). This
decision covers the fob. Schmetterpause has no route for the phone yet.

Schmetterpause decided its side first:

- **A fob is an identity** there, behind its auth-provider interface. Its
  ADR-0025 keeps an HMAC of the UID, never the UID. An unknown fob is linked
  to a player only inside a short window that player opened ("Anhänger
  koppeln"); outside it, an unknown UID is answered and forgotten.
- **`POST /api/identities/rfid/resolve`**, with the bearer token this service
  already sends to `/api/results`, takes `{"uid", "half"}` and answers
  `{"player_id", "checkin_id"}`. Every answer creates a new check-in over
  there. An unknown fob is a 404 with an empty body.
- **Two check-ins replace the operator** (its ADR-0024). A result that sends
  `home_checkin_id` and `away_checkin_id` instead of `operator_id` is
  accepted when each check-in belongs to its side, the two are at different
  halves, neither carries a result yet and neither is older than three hours.
  A check-in that already carries one is a 409.

So this service never decides who a fob is, and cannot vouch for two players
by itself: it hands back the check-ins it was given, and Schmetterpause
believes only its own.

A UID is not harmless. A company badge held against the reader by mistake
produces one, and so does any card in somebody's wallet.

## Decision

**A fob check-in comes in through ingest**, as `POST /ingest/checkin`:

    { "source": "hub-1", "event_id": 41, "half": "left", "uid": "04:A3:1B:22" }

The hub reads the fob, waits for the next press of a paired button to learn
the half, and sends this once (zaehlwerk-firmware#63). It uses the contract of
ADR-0002 like every other input path: `source` plus a counter that only goes
up, and a repeat is answered with what the first one got. Because every
resolve creates a check-in in Schmetterpause, a retry must not resolve again:
the attempt is recorded under its `event_id` before Schmetterpause is asked,
and a retry that arrives while it is under way waits for it. A counter that
falls back to a low value after a quiet minute is a restarted hub, as it is
for points.

**Claims are held per half, in memory** (invariant 2):

- Both halves claimed creates the match at once: `home_id` and the left
  half's check-in are `a`, `away_id` and the right half's are `b`. It starts
  in `CHECKIN_START`, `serve_rally` by default or `warmup`, with the first
  serve played out. `CHECKIN_CHANGE_ENDS=true` turns on the change of ends.
- A check-in while a real match is running is refused and changes nothing,
  and Schmetterpause is not asked. There is no lobby to join. A demo does not
  count as running; the checked-in match takes the table from it (ADR-0008).
- The same player at the same half again changes nothing but the check-in it
  keeps, which is the newer one, and its timeout, which starts again. Another player at that half replaces the
  claim. The same player at the other half moves there.
- A lone claim is released after `CHECKIN_TIMEOUT` (5 minutes by default).
- A real match started on the page or through the API releases every claim
  at once (#114). The table is that match's, and a claim made before it no
  longer says who stands at the table: without this, a lone claim could wait
  out a short match and pair with whoever checks in after it. A demo leaves
  the claims. The desk is told after the registry's lock is free, because it
  takes the registry's inside its own when it starts a match (#102).
- A restart forgets every claim. Something that expires in minutes is not
  worth an exception to invariant 2.

What a claim keeps is the player id, the check-in id, the display name for the
panel and when it was made. The ids reach the match's handover and nothing
else: `GET /checkin` shows halves and names only, like the match state.

**The resolve call** has the client's five-second timeout. An outage, a
timeout, a 5xx, a 401 or a missing route fails the check-in with a 503 to the
hub, and the half stays as it was. The hub shows it as not checked in, and the
player holds the fob up again. There is no queue and no retry here: a
check-in a minute late would claim a half somebody may have left.

**The UID never stays.** It is validated (hex, `:`, `-` or space between
bytes, 4, 7 or 10 bytes), passed to Schmetterpause in the one request, and
dropped. It is in no log line, no error message, no response body and no
retained state, here or in the dedup record of the attempt. A malformed one is
refused without being quoted. An unknown fob is a 404 to the hub with nothing
in it that names the card. A Schmetterpause without the route — no
`SP_RFID_UID_KEY` or no token over there — answers a plain 404 too; the log
says which of the two it was, by the body, without the UID.

**A checked-in match is reported without an operator.** The result carries
`home_checkin_id` and `away_checkin_id` and no `operator_id`. A 409 means a
check-in already carries a result, which for this service is a retry of a
result that did arrive, and is recorded as delivered. A match started from the
dropdown sends `operator_id` exactly as before, and the page asks for an
operator only there.

**Without Schmetterpause there is no check-in.** A fob cannot be resolved
without it, and a match from two bare claims would have nobody to report to.
The route answers 503, one line at start says so, and nothing else changes
(invariant 4).

## Consequences

- A match at the table can start without anybody touching the page: two fobs,
  two presses, a rally for the serve.
- The scorer is untouched. Claims are not set logic, and the halves they name
  are the ones ADR-0005 already resolves (invariant 5).
- Each resolve leaves a check-in in Schmetterpause, also for a claim that is
  then replaced, moved, timed out or refused because a match started in
  between. Those are never used and expire there after three hours.
- A UID never reaches the diagnostics. A fob that does not check in is
  debugged by the outcome in the log and on the hub's LED, not by its number.
- One table. A half belongs to the one table this service scores, as the panel
  and `Registry.Current` already do (ADR-0003). Several tables need a table id
  on the claim and on the check-in (schmetterpause#280, #60).
- **Only the hub checks in** (#113). A check-in names a player to
  Schmetterpause, and during "Fob koppeln" pairs a UID with one, so an open
  route would let anybody on the network check a colleague in without a chip
  at the table, or pair a UID of their own choice. The hub sends
  `CHECKIN_TOKEN` as a bearer, compared in constant time before the body is
  read; unset, check-in is off with a line at start, in the shape of the
  other couplings (invariants 3 and 4). The firmware's side is
  zaehlwerk-firmware#65. Points stay without a token: they name nobody.
- The phone path is still open. When Schmetterpause has a route for it, it ends
  in the same claim, with Schmetterpause calling in rather than the hub.
