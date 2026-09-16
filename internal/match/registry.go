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
)

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
type Handover struct {
	HomeID     string
	AwayID     string
	OperatorID string
}

// Wanted reports whether this match is meant to be reported.
func (h Handover) Wanted() bool {
	return h.HomeID != "" && h.AwayID != "" && h.OperatorID != ""
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

// Reported records a successful handover. Idempotent: the first success wins,
// so a retry that races a late success does not undo it.
func (m *Match) Reported(schmetterpauseID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.report.Attempts++
	if m.report.Done {
		return
	}
	m.report.Done = true
	m.report.MatchID = schmetterpauseID
	m.report.Err = nil
}

// ReportFailed records a failed handover, unless one has already succeeded.
func (m *Match) ReportFailed(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.report.Attempts++
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
type Registry struct {
	newID       func() (string, error)
	maxRetained int
	observers   []func(scorer.Transition)

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
	return func(r *Registry) { r.observers = append(r.observers, fn) }
}

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

// Create starts a match. The id in cfg is ignored and replaced by a fresh one.
func (r *Registry) Create(cfg scorer.Config) (*Match, error) {
	return r.CreateFor(cfg, Handover{})
}

// CreateFor is Create with the players named as Schmetterpause knows them, so
// the finished result can belong to somebody. An empty Handover is exactly
// Create: a match nobody reports.
func (r *Registry) CreateFor(cfg scorer.Config, handover Handover) (*Match, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	id, err := r.freeID()
	if err != nil {
		return nil, err
	}
	cfg.MatchID = id

	s, err := scorer.New(cfg)
	if err != nil {
		return nil, err
	}

	for _, fn := range r.observers {
		s.Observe(fn)
	}

	m := &Match{ID: id, Scorer: s, Created: time.Now(), Handover: handover}
	r.byID[id] = m
	r.order = append(r.order, m)
	r.evict()

	return m, nil
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
