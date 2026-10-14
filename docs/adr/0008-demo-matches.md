# 8. Demo matches

Status: Proposed
Date: 2026-10-14

## Context

zaehlwerk does not know that a match is a demo. To zaehlwerk-pong, a demo is a
match like any other, so a demo has to give way by itself (ADR-0006): it is
displaced when another match becomes current, it ends itself when a game is
asked for, and since #96 it ends itself when any divergence shows that somebody
else is counting into it. Two cases are out of Pong's reach, because only
zaehlwerk sees them (ADR-0006 Consequences, #94, #98):

1. **A demo left behind.** A pod that is SIGKILLed or OOM-killed mid-demo
   leaves its match running and current. The restarted service cannot tell it
   from somebody's match, so it never ends it, and the match blocks every
   further demo until somebody starts a real match.
2. **The race between `no_match` and `POST /matches`.** Attract mode sees
   `no_match` and creates its match a few milliseconds later. A real match
   created in between leaves the demo's match the newest, and so the current
   one (`Registry.Current`). The buttons then score into the demo until the
   demo notices a divergence and ends itself (#96). A ready press in `warmup`
   or `change_ends` changes no score and causes no divergence, so the real
   match can lose its first presses to the demo without anybody noticing.

Both cases come from the same gap: zaehlwerk resolves the table to the newest
running match and cannot weigh one match against another. Whatever decides
between a real match and a demo therefore has to happen in zaehlwerk, under the
registry's lock. A client that reads the table and then acts always leaves a
window between the two.

The rules that bound the answer:

- Invariant 1: zaehlwerk owns the running match. Which match is current is its
  decision, not a client's.
- Invariant 4: what a run needs at the table does not change. A run without
  Pong and without the new field behaves exactly as today.
- Invariant 5: the scorer is rule-only. Whether a match is a demo has no
  bearing on set logic.
- Invariant 6: Pong scores through the browser's door, and this decision does
  not give it a door of its own.
- The lock order of #102: the registry lock may be taken before a scorer lock,
  never after, and nothing an observer does may reach back into the registry.

## Decision

**A demo is marked when it is created.** `POST /matches` takes an optional
`demo: true`. It is stored on `match.Match` next to `Handover`, fixed at
creation and never changed after, so it is read without a lock for the same
reason `Handover` is. It is not stored in the scorer and not in
`scorer.State`. A match without the field is a real match, so the page, the
buttons, the piezo board and an older Pong are unchanged.

**A demo is never reported.** A demo and a handover rule each other out:
`Registry.CreateFor` refuses a demo that carries any player or operator id, and
a door that can send both answers `400`. The JSON API takes no ids today, so
the rule holds for any door added later, not only for this one. The reporter
needs no check of its own, because a match without a handover is reported
nowhere (ADR-0004).

**A real match always wins the table, decided in `CreateFor` under the
registry lock.** Two rules apply, and together they cover both orders in which
a real match and a demo can be created. They keep the letters they were proposed
under; (b) is the option rejected below:

- **(a) Creating a real match ends every running demo.** The demo's match is
  ended, the way `POST /matches/{id}/end` ends it, before the new match is
  added. This closes the case where the demo is created first: whatever it
  held, the real match has the table, the buttons and the panel from its
  first press. It also ends a demo left behind (case 1) at the next real
  match.
- **(c) A demo is refused while a real match is running.** `POST /matches`
  answers `409` with the running match's state, and Pong reads that as a busy
  table, as it reads a current match on `/live/stream` today. This closes the
  race of case 2: the real match was created first, so the demo never comes
  into existence.

With (a) and (c), a running demo and a running real match never exist at the
same time. `Registry.Current` therefore stays as it is, the newest running
match, and the resolution of ingest, of `/live/stream` and of `GET
/matches/current` does not change. A demo created while another demo runs is
allowed and becomes current as before. Only a real match ends demos.

**The lock order holds.** Inside `CreateFor`, (a) and (c) ask each match
whether it is running and whether it is a demo. Asking whether it is running
takes the scorer lock after the registry lock, as `evict` already does. The
demo flag is immutable and takes no lock. Ending a demo sets `endedAt` under
the match's own mutex. That is not a scorer transition, so no observer runs and
nothing can reach back into the registry. What `EndMatch` does around an end —
announcing `ended` on the hub and releasing the panel — happens in
`api.StartMatch` after `CreateFor` has returned and the registry lock is free.
The ended demos are handed back for that purpose. `StartMatch` gives the panel
to the new match first. The release of a demo after that finds the panel held
by another match and leaves it alone (ADR-0003), so the idle streams never
flash in between.

**Every match zaehlwerk-pong creates is a demo**: attract mode, the browser
game and the button check. What the flag says is "this match is no result and
gives way to one", and that holds for all three. None of them is ever reported
(ADR-0006). A button check presses real hardware, but what it produces is a
verdict on the buttons, not a score anybody keeps. Each of the three already
yields to a real match by Pong's own rules. The flag moves that rule into
zaehlwerk, where the race cannot happen. Marking only attract mode would leave
the same race open for a browser game asked for in the instant a real match
starts, and that race has existed for every game start since #83.

**Pong ends a demo that is not its own. zaehlwerk sets no timer for this.** A
demo left behind blocks only Pong, and only until the next real match ends it
under (a). The remaining case is attract mode on a table nobody uses. On that
table, a current match marked as a demo that Pong does not own is a match from
a pod that no longer exists:

- In attract mode, Pong ends it with `POST /matches/{id}/end` once it has been
  quiet for the whole wait (`PONG_ATTRACT_AFTER`). This is the same quiet that
  today only produces a log line.
- A game asked for in the lobby ends it at once, as it ends Pong's own demo
  through `yield()`.

A demo is nobody's result, so ending one costs nobody anything. The rule
"never end a match you cannot prove is your own" stays in force for every match
that is not marked.

**The flag is visible where the table is.**

- `/live/stream` carries `demo: true` on the events of a demo match. It is
  added at the API layer, like `home_id`/`away_id`, and is absent for a real
  match. Pong needs it to tell a left-behind demo from somebody's match.
  Anybody else following the table may use it.
- The panel gets a `demo=1` tag on a demo match. The tag is appended after the
  existing tags and is absent otherwise, so a catcher rule that never heard of
  it sees the tags it always saw. The catcher can show "demo" from a display
  rule without learning anything about table tennis (invariant 7). To read the
  flag, the panel sink becomes a match observer through `WithMatchObserver`,
  which exists since #102 for exactly this purpose: it is handed the match and
  never looks it up.
- `scorer.State` does not change. `GET /matches/{id}` and the ingest answers do
  not carry the flag, because they are answers about the score.

## Options considered

- **(b) `Current()` prefers a running real match over a newer demo.** On its
  own this would close case 2 for ingest and for the stream. The demo would
  still be created and still be running, though. `StartMatch` would hand it the
  panel, and its points would be pitched to the panel and to the hub while a
  real match is under way. Pong would then score its demo into a match that is
  not the table's and end it only once it saw another match current. Next to
  (a) and (c), (b) would also be dead code: no state it decides between can
  arise. Rejected, in favour of not letting the demo into existence.
- **(a) alone.** (a) ends demos when a real match is created. In the race of
  case 2, though, the real match is created *first*, and the demo's creation
  has nothing to end. (a) alone therefore leaves exactly the order the issue
  describes open. That is why (c) is part of the decision.
- **The flag in the scorer or in `scorer.State`.** Rejected by invariant 5: it
  has no bearing on set logic, and a field in the state would travel through
  every answer that reports a score.
- **An atomic "create only if the table is free" for every match.** This would
  close the race as well, but a real match must never be refused because
  something else is running. At a table, the match somebody starts is the
  table's (ADR-0003, ADR-0006). Only a demo should wait, and only a demo can be
  told apart. The rule for any match would be wrong for the real one.
- **zaehlwerk ends a demo after an inactivity time.** This would be a new
  environment variable (invariant 3) and a timer in zaehlwerk for a case that
  arises only where Pong is deployed. It would also end a demo in the middle of
  a quiet stretch on a table where nobody wants the panel back. Pong already
  measures that quiet for attract mode, so the decision stays there.
- **Pong ends any quiet match, marked or not.** Rejected, as in ADR-0006: an
  unmarked match nobody scores any more looks exactly like one paused between
  two points.
- **A separate endpoint, e.g. `POST /demos`.** This would be a second way to
  create a match for one boolean. A field on the existing body keeps one door,
  and an older client simply does not send it.

## Consequences

- Case 1 is gone at the next real match. Without one, attract mode ends a
  left-behind demo after one quiet wait, and the normal wait starts again from
  the `no_match` that follows. A table with a left-behind demo therefore sees
  its next demo after two waits.
- Case 2 is gone. A demo either exists before the real match and is ended by
  it, or is refused because the real match exists. #96's divergence rule stays
  for what it was meant to catch: somebody pressing the buttons during a demo.
- A real match started at the table ends a running browser game or button check
  at once, inside zaehlwerk. Pong saw that only as a displacement until now,
  after the fact.
- Pong learns to read `409` on `POST /matches` as a busy table, and to read
  `demo` on the stream. Its rule "a current match is somebody's" gets one
  exception: a demo it does not own.
- `api.StartMatch` announces `ended` for each demo that `CreateFor` ended,
  before or after the new match's `started`. A stream on the demo then follows
  to the real match as it would after any end.
- An older Pong sends no `demo`, so its matches are real to a newer zaehlwerk,
  and it behaves as today. A newer Pong against an older zaehlwerk has its
  `demo` ignored, because unknown fields are not refused, and gets today's
  behaviour too. The two are released under one tag anyway.
- Schmetterpause's start page follows `/live/stream` and could hide demos, or
  label them, using the new field. That is a change in Schmetterpause and is
  not part of this decision.
- zaehlwerk's own scoring page could label a demo the same way. Until it does,
  a demo shows there as today, named `Pong A` and `Pong B`.
- This supplements ADR-0006 and does not replace it. The rule for which match
  is current, the newest running one in `Registry.Current`, is unchanged,
  because (a) and (c) keep the case it would have to decide from arising.
