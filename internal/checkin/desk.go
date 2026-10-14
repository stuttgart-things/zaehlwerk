// Package checkin holds who has claimed which half of the table, and starts
// the match when both halves are claimed (ADR-0010).
//
// A player holds a fob to the reader at the hub and presses the button of
// their half; the hub sends the UID and the half once. The desk asks
// Schmetterpause who the fob is, which also records a check-in over there,
// and keeps the player id and that check-in id against the half. The second
// half claimed starts the match, with both check-ins on its handover, so the
// result goes to Schmetterpause without an operator.
//
// Everything here is in memory (invariant 2): a claim lasts minutes and a
// restart forgets it. The UID is not kept anywhere — not in a claim, not in
// the record that makes a retried request safe, not in a log line.
package checkin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// DefaultTimeout is how long a lone claim holds its half. Long enough to walk
// round the table and find a fob, short enough that a claim left behind at
// lunch is gone before the next pair arrives.
const DefaultTimeout = 5 * time.Minute

// maxSources bounds the dedup records. A table has one hub, and the route is
// open on the office network, so this is a bound on what a caller can make
// the process remember rather than a number anybody should reach.
const maxSources = 64

// Schmetterpause is the part of the coupling the desk needs: who a fob is,
// and the names to put on the panel. [schmetterpause.Client] implements it.
type Schmetterpause interface {
	ResolveFob(ctx context.Context, uid, half string) (schmetterpause.FobCheckin, error)
	Players(ctx context.Context) ([]schmetterpause.Player, error)
}

// Table is the table the desk starts matches on. The API server implements
// it, because starting a match is also where the panel changes hands
// (ADR-0003), and that belongs in one place.
//
// The desk calls it with its own lock held, so that no claim can land between
// "both halves are taken" and the match that ends the claims. Neither method
// may call back into the desk.
type Table interface {
	// Occupied reports whether a real match is running. A demo does not
	// count: a checked-in match takes the table from it (ADR-0008).
	Occupied() bool
	StartMatch(cfg scorer.Config, handover match.Handover) (*match.Match, error)
}

// Outcome is what a check-in did.
type Outcome string

const (
	// Claimed is a free half taken.
	Claimed Outcome = "claimed"
	// Unchanged is the same player at the same half again. The claim keeps
	// the newer check-in and starts its timeout again; nothing else moves.
	Unchanged Outcome = "unchanged"
	// Replaced is another player at a half somebody had claimed.
	Replaced Outcome = "replaced"
	// Moved is a player who had claimed the other half and now claims this
	// one.
	Moved Outcome = "moved"
	// Started is the second half claimed: the match was created.
	Started Outcome = "started"
	// Unknown is a fob Schmetterpause does not know. Nothing changed.
	Unknown Outcome = "unknown"
	// Refused is a check-in while a real match is running. Nothing changed
	// and Schmetterpause was not asked.
	Refused Outcome = "refused"
	// Unavailable is a check-in Schmetterpause could not answer. Nothing
	// changed; the player holds the fob up again.
	Unavailable Outcome = "unavailable"
	// Failed is a match that could not be created from two claims. The claims
	// stay.
	Failed Outcome = "failed"
	// Duplicate is a request whose event_id is older than the source's last
	// one, so there is no answer to repeat. Nothing changed.
	Duplicate Outcome = "duplicate"
)

// Request is one check-in from the hub: the ingest contract of ADR-0002, a
// half and a UID.
type Request struct {
	Source  string
	EventID uint64
	Half    scorer.Half
	UID     string
}

// Validate checks the request without quoting the UID in what it returns, and
// writes the UID in its one spelling.
func (r *Request) Validate() error {
	switch {
	case r.Source == "":
		return errors.New("source is required")
	case r.Half != scorer.HalfLeft && r.Half != scorer.HalfRight:
		return fmt.Errorf("half: must be %q or %q", scorer.HalfLeft, scorer.HalfRight)
	}
	uid, err := NormaliseUID(r.UID)
	if err != nil {
		return err
	}
	r.UID = uid
	return nil
}

// Answer is what a check-in did, and what a retry of it gets again. It holds
// no UID and no ids: it is repeated to whoever sends the same event_id.
type Answer struct {
	Outcome Outcome
	Half    scorer.Half
	// Name is the player's display name, when the fob was known and the
	// player list could be read.
	Name string
	// MatchID is the match a Started check-in created, or the running one a
	// Refused check-in ran into.
	MatchID string
	// Err says why, for the outcomes that are not a claim. Never the UID.
	Err error
	// Repeated is set on the answer to a retry: it is the first attempt's.
	Repeated bool
	// Halves is who holds each half after this check-in, left then right.
	Halves [2]string
}

// Claim is one half held by one player.
type Claim struct {
	PlayerID  string
	CheckinID string
	Name      string
	At        time.Time
}

// View is a half as GET /checkin shows it: a name and two times, no ids, the
// way a match's state carries names and not ids (ADR-0004).
type View struct {
	Name    string    `json:"name"`
	Since   time.Time `json:"since"`
	Expires time.Time `json:"expires"`
}

// Desk holds the claims. Use [NewDesk].
type Desk struct {
	sp      Schmetterpause
	log     *slog.Logger
	now     func() time.Time
	timeout time.Duration
	// match is the configuration a checked-in match starts with: the phase,
	// the change of ends. Players and handover are filled in from the claims.
	match scorer.Config

	mu      sync.Mutex
	claims  [2]*Claim // by half index: left, right
	sources map[string]*source
}

// source is one sender's place in the ingest contract, for check-ins only:
// a match's scorer keeps its own for points, and a check-in has no match.
type source struct {
	high     uint64
	lastSeen time.Time
	// last is the attempt for high, kept so that a retry of it gets the same
	// answer rather than a second resolve.
	last *attempt
}

// attempt is one check-in under way or done. done is closed once answer is
// set.
type attempt struct {
	done   chan struct{}
	answer Answer
}

// Option configures a Desk.
type Option func(*Desk)

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option {
	return func(d *Desk) {
		if l != nil {
			d.log = l
		}
	}
}

// WithClock sets the clock, for tests.
func WithClock(now func() time.Time) Option {
	return func(d *Desk) {
		if now != nil {
			d.now = now
		}
	}
}

// WithTimeout sets how long a lone claim holds its half.
func WithTimeout(t time.Duration) Option {
	return func(d *Desk) {
		if t > 0 {
			d.timeout = t
		}
	}
}

// WithMatch sets how a checked-in match starts: Start and ChangeEnds are
// taken from cfg, everything else is the scorer's default.
func WithMatch(cfg scorer.Config) Option {
	return func(d *Desk) { d.match = cfg }
}

// NewDesk builds a desk that resolves fobs through sp.
func NewDesk(sp Schmetterpause, opts ...Option) *Desk {
	d := &Desk{
		sp:      sp,
		log:     slog.Default(),
		now:     time.Now,
		timeout: DefaultTimeout,
		match:   scorer.Config{Start: scorer.StartServeRally},
		sources: make(map[string]*source),
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Timeout is how long a lone claim holds its half.
func (d *Desk) Timeout() time.Duration { return d.timeout }

// CheckIn handles one validated request against table.
//
// The resolve call runs without the desk's lock, because it is a network call
// of up to five seconds. Everything it decides is applied under the lock
// afterwards, against the claims as they are by then.
func (d *Desk) CheckIn(ctx context.Context, req Request, table Table) Answer {
	d.mu.Lock()
	now := d.now()
	d.expire(now)

	att, prev := d.begin(req.Source, req.EventID, now)
	if att == nil {
		halves := d.halves()
		d.mu.Unlock()
		if prev != nil {
			// The same event_id again: the answer the first one got, once it
			// has one. A retry that arrives while the first is still being
			// resolved waits for it rather than resolving a second time.
			select {
			case <-prev.done:
			case <-ctx.Done():
				return Answer{Outcome: Unavailable, Half: req.Half, Halves: halves,
					Err: fmt.Errorf("waiting for the first attempt: %w", ctx.Err())}
			}
			a := prev.answer
			a.Repeated = true
			d.log.Info("check-in repeated", "source", req.Source, "event_id", req.EventID,
				"half", req.Half, "outcome", a.Outcome)
			return a
		}
		d.log.Info("check-in discarded, an older event_id", "source", req.Source,
			"event_id", req.EventID, "half", req.Half)
		return Answer{Outcome: Duplicate, Half: req.Half, Halves: halves}
	}

	if table.Occupied() {
		a := d.finish(att, req, Answer{Outcome: Refused, Half: req.Half,
			Err: errors.New("a match is running, check in once it is over")})
		d.mu.Unlock()
		return a
	}
	d.mu.Unlock()

	// Not cancelled with the request: Schmetterpause records a check-in as
	// it answers, so a hub that gives up mid-call must not leave this
	// attempt without an answer for its retry. The client's own timeout
	// bounds it.
	resolveCtx := context.WithoutCancel(ctx)
	got, err := d.sp.ResolveFob(resolveCtx, req.UID, string(req.Half))
	var name string
	if err == nil {
		name = d.nameOf(resolveCtx, got.PlayerID)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	now = d.now()
	d.expire(now)

	switch {
	case errors.Is(err, schmetterpause.ErrUnknownFob):
		return d.finish(att, req, Answer{Outcome: Unknown, Half: req.Half,
			Err: errors.New("unknown fob")})
	case errors.Is(err, schmetterpause.ErrNoFobRoute):
		return d.finish(att, req, Answer{Outcome: Unavailable, Half: req.Half, Err: err})
	case err != nil:
		return d.finish(att, req, Answer{Outcome: Unavailable, Half: req.Half,
			Err: fmt.Errorf("resolving the fob: %w", err)})
	}

	// Asked again: a match may have started while Schmetterpause answered.
	// The check-in over there stays unused and expires.
	if table.Occupied() {
		return d.finish(att, req, Answer{Outcome: Refused, Half: req.Half, Name: name,
			Err: errors.New("a match is running, check in once it is over")})
	}

	outcome := d.claim(req.Half, Claim{PlayerID: got.PlayerID, CheckinID: got.CheckinID, Name: name, At: now})
	a := Answer{Outcome: outcome, Half: req.Half, Name: name}

	left, right := d.claims[0], d.claims[1]
	if left != nil && right != nil {
		m, err := table.StartMatch(d.config(left, right), match.Handover{
			HomeID: left.PlayerID, AwayID: right.PlayerID,
			HomeCheckinID: left.CheckinID, AwayCheckinID: right.CheckinID,
		})
		if err != nil {
			// Both claims stay: a match that could not be created is ours to
			// fix, and taking the halves away would make the players check in
			// again for it.
			return d.finish(att, req, Answer{Outcome: Failed, Half: req.Half, Name: name,
				Err: fmt.Errorf("starting the checked-in match: %w", err)})
		}
		d.claims = [2]*Claim{}
		a.Outcome = Started
		a.MatchID = m.ID
		d.log.Info("both halves checked in, match started", "match_id", m.ID,
			"left", left.Name, "right", right.Name)
	}
	return d.finish(att, req, a)
}

// Drop releases every claim, because a real match started somewhere else: on
// the page or through the API. The table is that match's now (ADR-0008), and
// a claim made before it no longer says who stands at the table once it is
// over (#114). The desk's own matches end their claims themselves.
//
// It takes the desk's lock, so it must not be called with the registry's
// held, or from inside [Table]: the desk holds its lock while it starts a
// match, and the registry's is taken inside that (#102).
func (d *Desk) Drop(matchID string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.claims == [2]*Claim{} {
		return
	}
	h := d.halves()
	d.claims = [2]*Claim{}
	d.log.Info("check-ins dropped, a match started elsewhere took the table", "match_id", matchID,
		"left", h[0], "right", h[1])
}

// Claims shows the halves, left then right, nil where a half is free.
func (d *Desk) Claims() [2]*View {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.expire(d.now())
	var out [2]*View
	for i, c := range d.claims {
		if c != nil {
			out[i] = &View{Name: c.Name, Since: c.At, Expires: c.At.Add(d.timeout)}
		}
	}
	return out
}

// begin records an attempt for this event_id and returns it, or returns nil
// and the earlier attempt when the event_id has been seen. Must be called
// with the lock held.
func (d *Desk) begin(src string, id uint64, now time.Time) (*attempt, *attempt) {
	s, known := d.sources[src]
	switch {
	case !known, id > s.high:
	case restarted(s, id, now):
		// A hub that was reflashed or lost power counts from zero again, as a
		// button does (ADR-0002). Without this it would be locked out until
		// this service restarts, since check-ins have no match to reset with.
	case id == s.high && s.last != nil:
		s.lastSeen = now
		return nil, s.last
	default:
		s.lastSeen = now
		return nil, nil
	}

	if !known {
		d.forgetQuietestSource()
		s = &source{}
		d.sources[src] = s
	}
	att := &attempt{done: make(chan struct{})}
	s.high, s.lastSeen, s.last = id, now, att
	return att, nil
}

func restarted(s *source, id uint64, now time.Time) bool {
	return id <= scorer.DefaultRestartMaxEventID && now.Sub(s.lastSeen) >= scorer.DefaultRestartQuietPeriod
}

// forgetQuietestSource makes room for a new source. Must be called with the
// lock held.
func (d *Desk) forgetQuietestSource() {
	if len(d.sources) < maxSources {
		return
	}
	var quietest string
	var at time.Time
	for name, s := range d.sources {
		if quietest == "" || s.lastSeen.Before(at) {
			quietest, at = name, s.lastSeen
		}
	}
	delete(d.sources, quietest)
}

// finish records the answer for retries, logs it and returns it. Must be
// called with the lock held.
func (d *Desk) finish(att *attempt, req Request, a Answer) Answer {
	a.Halves = d.halves()
	att.answer = a
	close(att.done)

	// The UID is not in this line or any other: the outcome, the half and
	// who it turned out to be are what a fob that did not check in is
	// debugged by (ADR-0010).
	attrs := []any{"source", req.Source, "event_id", req.EventID, "half", req.Half, "outcome", a.Outcome}
	if a.Name != "" {
		attrs = append(attrs, "name", a.Name)
	}
	if a.MatchID != "" {
		attrs = append(attrs, "match_id", a.MatchID)
	}
	switch a.Outcome {
	case Unavailable, Failed:
		d.log.Warn("check-in failed", append(attrs, "error", a.Err)...)
	default:
		if a.Err != nil {
			attrs = append(attrs, "reason", a.Err)
		}
		d.log.Info("check-in", attrs...)
	}
	return a
}

// claim applies one resolved check-in to the halves. Must be called with the
// lock held.
func (d *Desk) claim(half scorer.Half, c Claim) Outcome {
	i := halfIndex(half)
	here, there := d.claims[i], d.claims[1-i]

	switch {
	case here != nil && here.PlayerID == c.PlayerID:
		d.claims[i] = &c
		d.arm()
		return Unchanged
	case there != nil && there.PlayerID == c.PlayerID:
		d.claims[1-i] = nil
		d.claims[i] = &c
		d.arm()
		return Moved
	case here != nil:
		d.claims[i] = &c
		d.arm()
		return Replaced
	default:
		d.claims[i] = &c
		d.arm()
		return Claimed
	}
}

// arm makes sure a lone claim is released when its time is up even when
// nobody asks: the timer only sweeps, and a sweep that finds nothing due does
// nothing, so a stale timer is harmless.
func (d *Desk) arm() {
	time.AfterFunc(d.timeout, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.expire(d.now())
	})
}

// expire releases claims older than the timeout. Must be called with the
// lock held.
func (d *Desk) expire(now time.Time) {
	for i, c := range d.claims {
		if c != nil && now.Sub(c.At) >= d.timeout {
			d.claims[i] = nil
			d.log.Info("check-in timed out, the half is free again",
				"half", halfAt(i), "name", c.Name, "after", d.timeout)
		}
	}
}

// halves is who holds each half by name. Must be called with the lock held.
func (d *Desk) halves() [2]string {
	var out [2]string
	for i, c := range d.claims {
		if c != nil {
			out[i] = c.Name
		}
	}
	return out
}

// config is the match two claims start: the table's phase and ends, and the
// names from the claims for the panel.
func (d *Desk) config(left, right *Claim) scorer.Config {
	cfg := scorer.Config{Start: d.match.Start, ChangeEnds: d.match.ChangeEnds}
	if cfg.Start == scorer.StartPlaying {
		// A checked-in match always plays for the serve: nobody chose a first
		// server, and the house rule is to play it out (#77).
		cfg.Start = scorer.StartServeRally
	}
	if left.Name != "" && right.Name != "" {
		cfg.Players = [2]string{left.Name, right.Name}
	}
	return cfg
}

// nameOf finds the player's display name for the panel. A failure leaves it
// empty and the match is labelled A and B: the claim does not depend on it.
func (d *Desk) nameOf(ctx context.Context, playerID string) string {
	players, err := d.sp.Players(ctx)
	if err != nil {
		d.log.Warn("no name for a checked-in player, the player list did not load", "error", err)
		return ""
	}
	for _, p := range players {
		if p.ID == playerID {
			return p.DisplayName
		}
	}
	return ""
}

func halfIndex(h scorer.Half) int {
	if h == scorer.HalfRight {
		return 1
	}
	return 0
}

func halfAt(i int) scorer.Half {
	if i == 1 {
		return scorer.HalfRight
	}
	return scorer.HalfLeft
}
