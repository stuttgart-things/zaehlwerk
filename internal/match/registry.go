// Package match keeps the running matches in memory.
//
// In memory deliberately: a match lasts twenty minutes and the finished result
// goes to Schmetterpause, so there is nothing here worth surviving a restart.
package match

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// DefaultMaxRetained is how many finished matches the registry keeps before it
// starts dropping the oldest. A running match is never dropped. The point is
// only to stop a process that runs for months from growing without bound; at an
// office table this is weeks of history.
const DefaultMaxRetained = 200

var (
	// ErrNotFound is returned for a match id the registry does not know.
	ErrNotFound = errors.New("no such match")
	// ErrIDGeneration means no match id could be drawn. It is the registry's
	// fault, not the caller's, and the only error here that is not a bad
	// request.
	ErrIDGeneration = errors.New("could not generate a match id")
	// ErrDemoReported is a demo asked for with a player or operator id. A
	// demo is never reported (ADR-0008), so an id on one is a mistake rather
	// than something to drop quietly.
	ErrDemoReported = errors.New("a demo is never reported, so it takes no player or operator id")
	// ErrTableTaken is a demo asked for while a real match is running. The
	// real match has the table (ADR-0008); the error is a *TableTakenError,
	// which names it.
	ErrTableTaken = errors.New("a real match is running, a demo waits until the table is free")
)

// TableTakenError is [ErrTableTaken] with the match that holds the table, so
// a caller can answer with where that match stands.
type TableTakenError struct {
	Running *Match
}

func (e *TableTakenError) Error() string {
	return fmt.Sprintf("%v: match %s", ErrTableTaken, e.Running.ID)
}

func (e *TableTakenError) Unwrap() error { return ErrTableTaken }

// Handover is who a finished match belongs to over in Schmetterpause.
//
// Empty when this match is not being reported: free-text names, no ids, no
// operator. ADR-0004 keeps that a first-class case — a match without
// Schmetterpause is still a match, scored and shown and pitched exactly the
// same, reported nowhere.
//
// The ids live here for the length of the match and nowhere else. Not in the
// scorer, which stays rule-only (invariant 5) and knows players as two display
// names: an id is not something set logic can have an opinion about. Nothing is
// written to disk, nothing survives a restart, and no player is known here for
// a second longer than the match lasts.
//
// It carries one of two proofs that the two players played: OperatorID, the
// person who kept score on the page (ADR-0004), or the two check-ins the
// players made at the table themselves (ADR-0010). The check-in ids are what
// Schmetterpause handed out when it resolved each fob, passed back unchanged.
type Handover struct {
	HomeID     string
	AwayID     string
	OperatorID string
	// HomeCheckinID and AwayCheckinID are the check-ins at the left and the
	// right half, so home's is a's and away's is b's (ADR-0005).
	HomeCheckinID string
	AwayCheckinID string
}

// Wanted reports whether this match is meant to be reported.
func (h Handover) Wanted() bool {
	return h.HomeID != "" && h.AwayID != "" && (h.OperatorID != "" || h.CheckedIn())
}

// CheckedIn reports whether both players checked in, so the result goes
// without an operator.
func (h Handover) CheckedIn() bool {
	return h.HomeCheckinID != "" && h.AwayCheckinID != ""
}

// empty reports whether no id at all is set, which is what a demo needs: one
// id alone is not a handover, but it is somebody's id on a match that is not.
func (h Handover) empty() bool {
	return h == Handover{}
}

// Setup is what a match is created with besides its rules: who it belongs to,
// and whether it is a demo.
type Setup struct {
	Handover Handover
	// Demo marks a match that is no result and gives way to one, as
	// zaehlwerk-pong's are (ADR-0008).
	Demo bool
}

// Report is what became of the handover.
//
// It exists because ADR-0004 accepted that a result can be lost and asked for
// the loss to be visible instead of silent: the page says the result did not
// reach Schmetterpause, and the match stays in the registry long enough to try
// again. Without somewhere to record the outcome there would be nothing for
// the page to say.
type Report struct {
	// Done is true once Schmetterpause has accepted the result. A retry of an
	// already-reported match is a no-op rather than a second row.
	Done bool
	// MatchID is what Schmetterpause called it, for the log and the page.
	MatchID string
	// Err is the last failure, and nil once Done.
	Err error
	// Attempts counts tries, so a page can say "tried twice" rather than
	// implying nothing happened.
	Attempts int
	// Due is when the result goes out, while it waits out the grace period
	// after match_won (ADR-0007), and zero otherwise. The page says it, so
	// that whoever pressed the match point wrong knows an undo still takes it
	// back.
	Due time.Time
	// Sent is the sets as Schmetterpause got them. An undo after the report
	// is not reported again, and the page compares this with the score to say
	// that the result over there no longer holds.
	Sent [][2]int
}

// Match is one match: the scorer that owns its state, plus the lifecycle around
// it that the scorer has no opinion about.
type Match struct {
	ID      string
	Scorer  *scorer.Scorer
	Created time.Time
	// Handover is fixed when the match is created and never changes after.
	// Read without the lock for that reason.
	Handover Handover
	// Demo is fixed the same way, and read without the lock for the same
	// reason. It lives here and not in the scorer: whether a match is a demo
	// has no bearing on set logic (invariant 5).
	Demo bool

	mu      sync.Mutex
	endedAt time.Time
	report  Report
}

// Report returns what became of the handover so far.
func (m *Match) Report() Report {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.report
}

// Reported records a successful handover of these sets. Idempotent: the first
// success wins, so a retry that races a late success does not undo it.
func (m *Match) Reported(schmetterpauseID string, sets [][2]int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.report.Attempts++
	m.report.Due = time.Time{}
	if m.report.Done {
		return
	}
	m.report.Done = true
	m.report.MatchID = schmetterpauseID
	m.report.Err = nil
	m.report.Sent = slices.Clone(sets)
}

// ReportDue records when the result goes out, or with a zero time that it no
// longer waits: sent, withheld or sent early.
func (m *Match) ReportDue(at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.report.Due = at
}

// ReportFailed records a failed handover, unless one has already succeeded.
func (m *Match) ReportFailed(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.report.Attempts++
	m.report.Due = time.Time{}
	if m.report.Done {
		return
	}
	m.report.Err = err
}

// End marks the match finished and reports when it ended. It is idempotent: a
// match that has already ended keeps the time it ended the first time, so a
// retried request does not move it.
func (m *Match) End(now time.Time) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.endedAt.IsZero() {
		m.endedAt = now
	}
	return m.endedAt
}

// EndedAt reports when the match was explicitly ended, if it was.
func (m *Match) EndedAt() (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.endedAt, !m.endedAt.IsZero()
}

// Running reports whether points can still be scored: neither won on court nor
// ended by hand.
func (m *Match) Running() bool {
	if _, ended := m.EndedAt(); ended {
		return false
	}
	return !m.Scorer.State().Complete
}

// Registry hands out match ids and keeps the matches behind them. It is safe
// for concurrent use.
//
// Lock ordering: the registry lock may be taken before a scorer lock, never
// after. Nothing in the scorer reaches back into the registry, and an observer
// must not either — it is called while the scorer holds its own lock.
// CreateFor holds the registry lock while evict asks matches whether they are
// running, which takes their scorer locks; an observer calling Get or Current
// at the same moment holds one of those and waits for the registry, and the
// service hangs (#102). An observer that needs the match takes
// WithMatchObserver, which hands it over, and never looks it up.
// WithObserver stays for observers that do not need it.
type Registry struct {
	newID       func() (string, error)
	maxRetained int
	// observers are both kinds, in the order the options gave them: the
	// reporter has to run before the hub renders the result card (ADR-0007).
	observers []observer

	mu    sync.Mutex
	byID  map[string]*Match
	order []*Match // oldest first
}

// Option configures a Registry.
type Option func(*Registry)

// WithIDs replaces the id generator, for tests.
func WithIDs(newID func() (string, error)) Option {
	return func(r *Registry) { r.newID = newID }
}

// WithMaxRetained sets how many finished matches are kept.
func WithMaxRetained(n int) Option {
	return func(r *Registry) { r.maxRetained = n }
}

// WithObserver registers fn as a scorer observer on every match the registry
// creates. This is how the panel sink, and later the live view, get told about
// a point without either of them knowing when a match starts.
//
// The scorer calls observers from inside its lock, so fn must not block — see
// (*scorer.Scorer).Observe.
func WithObserver(fn func(scorer.Transition)) Option {
	return func(r *Registry) {
		r.observers = append(r.observers, func(*Match) func(scorer.Transition) { return fn })
	}
}

// WithMatchObserver is WithObserver for an observer that needs the match the
// transition belongs to, as the Schmetterpause reporter does: it is handed
// over, so the observer never has to ask the registry for it from inside the
// scorer's lock (see [Registry]).
func WithMatchObserver(fn func(*Match, scorer.Transition)) Option {
	return func(r *Registry) {
		r.observers = append(r.observers, func(m *Match) func(scorer.Transition) {
			return func(t scorer.Transition) { fn(m, t) }
		})
	}
}

// observer builds the scorer observer for one match.
type observer func(*Match) func(scorer.Transition)

func NewRegistry(opts ...Option) *Registry {
	r := &Registry{
		newID:       randomID,
		maxRetained: DefaultMaxRetained,
		byID:        make(map[string]*Match),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Create starts a real match nobody reports. The id in cfg is ignored and
// replaced by a fresh one. Demos it ends are not handed back; a caller that
// announces them uses CreateFor.
func (r *Registry) Create(cfg scorer.Config) (*Match, error) {
	m, _, err := r.CreateFor(cfg, Setup{})
	return m, err
}

// CreateFor is Create with a setup: the players named as Schmetterpause knows
// them, so the finished result can belong to somebody, or the match marked as
// a demo. An empty Setup is exactly Create.
//
// A real match always wins the table (ADR-0008), and both halves of that are
// decided here, under the registry lock, because a client that reads the table
// and then acts always leaves a window between the two:
//
//   - a real match ends every running demo, and hands them back as ended, so
//     the caller can announce them and let go of their panel;
//   - a demo is refused with a *TableTakenError while a real match is running.
//
// So a running demo and a running real match never exist at once, and Current
// stays the newest running match.
func (r *Registry) CreateFor(cfg scorer.Config, setup Setup) (*Match, []*Match, error) {
	if setup.Demo && !setup.Handover.empty() {
		return nil, nil, ErrDemoReported
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Running takes each match's scorer lock while the registry lock is held:
	// the order evict already takes them in, and the only order allowed (see
	// Registry).
	if setup.Demo {
		for _, m := range r.order {
			if !m.Demo && m.Running() {
				return nil, nil, &TableTakenError{Running: m}
			}
		}
	}

	id, err := r.freeID()
	if err != nil {
		return nil, nil, err
	}
	cfg.MatchID = id

	s, err := scorer.New(cfg)
	if err != nil {
		return nil, nil, err
	}

	// The match is built whole before any observer is attached, and no
	// transition can come before it is returned: the scorer emits only from
	// Apply and Undo, never from New, and nobody holds it until CreateFor
	// returns or the registry lock is let go. So an observer handed m never
	// sees half of it.
	now := time.Now()
	m := &Match{ID: id, Scorer: s, Created: now, Handover: setup.Handover, Demo: setup.Demo}
	for _, build := range r.observers {
		s.Observe(build(m))
	}

	// Ended before the new match is added, so there is no moment in which the
	// demo is the newer of two running matches. End takes only the match's
	// own mutex and is no scorer transition: no observer runs, so nothing can
	// reach back into the registry from under this lock (#102). What an end
	// otherwise brings with it -- ended on the hub, the panel let go of -- is
	// the caller's, once this lock is free.
	var ended []*Match
	if !setup.Demo {
		for _, d := range r.order {
			if d.Demo && d.Running() {
				d.End(now)
				ended = append(ended, d)
			}
		}
	}

	r.byID[id] = m
	r.order = append(r.order, m)
	r.evict()

	return m, ended, nil
}

// Get returns the match with this id.
func (r *Registry) Get(id string) (*Match, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	m, ok := r.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return m, nil
}

// Current returns the match that hardware ingest belongs to: the most recently
// created one that is still running.
//
// Buttons and piezo units are bound to the table, not to a match — the ESP-NOW
// payload carries a source, a player and a counter, and there is nowhere for a
// match id to come from. So the API resolves it. There is one table and one
// panel; concurrent matches are out of scope in ADR-0003 for the same reason.
func (r *Registry) Current() (*Match, error) {
	r.mu.Lock()
	candidates := make([]*Match, len(r.order))
	copy(candidates, r.order)
	r.mu.Unlock()

	// Running() reaches into the scorer, so it is called without the registry
	// lock held.
	for i := len(candidates) - 1; i >= 0; i-- {
		if candidates[i].Running() {
			return candidates[i], nil
		}
	}
	return nil, fmt.Errorf("%w: no match is running", ErrNotFound)
}

// Occupied returns the newest real match that is still running, if there is
// one. A demo does not occupy the table: a real match takes it from a demo
// (ADR-0008), and a check-in is how a real match starts (ADR-0010).
func (r *Registry) Occupied() (*Match, bool) {
	r.mu.Lock()
	candidates := slices.Clone(r.order)
	r.mu.Unlock()

	// Running() reaches into the scorer, so it is called without the registry
	// lock held, as in Current.
	for i := len(candidates) - 1; i >= 0; i-- {
		if m := candidates[i]; !m.Demo && m.Running() {
			return m, true
		}
	}
	return nil, false
}

// freeID draws an id that is not in use. Must be called with the lock held.
func (r *Registry) freeID() (string, error) {
	for range 10 {
		id, err := r.newID()
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrIDGeneration, err)
		}
		if _, taken := r.byID[id]; !taken {
			return id, nil
		}
	}
	return "", fmt.Errorf("%w: every attempt collided with a match in the registry", ErrIDGeneration)
}

// evict drops the oldest finished matches once there are more than the limit.
// Must be called with the lock held.
func (r *Registry) evict() {
	for len(r.order) > r.maxRetained {
		var kept []*Match
		dropped := false

		for i, m := range r.order {
			if !dropped && !m.Running() {
				delete(r.byID, m.ID)
				dropped = true
				continue
			}
			kept = append(kept, r.order[i])
		}
		if !dropped {
			// Everything still running. Nothing to drop, and dropping a match
			// in progress would be worse than the memory.
			return
		}
		r.order = kept
	}
}

func randomID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
