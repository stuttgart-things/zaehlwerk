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

// Match is one match: the scorer that owns its state, plus the lifecycle around
// it that the scorer has no opinion about.
type Match struct {
	ID      string
	Scorer  *scorer.Scorer
	Created time.Time

	mu      sync.Mutex
	endedAt time.Time
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

	m := &Match{ID: id, Scorer: s, Created: time.Now()}
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
