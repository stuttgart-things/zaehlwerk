// Package live fans scorer transitions out to connected browsers.
//
// It is the second consumer of transitions alongside the panel sink, and it is
// fed from the scorer directly rather than by reading the Redis stream back.
// Going through Redis would add latency to the one view where latency is
// visible — the phone in someone's hand — and would tie the live score to the
// availability of a stream it does not otherwise need.
package live

import (
	"sync"
	"sync/atomic"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// DefaultBuffer is how many transitions a subscriber may fall behind by before
// the oldest are dropped. A match produces a transition every few seconds, so
// this is only ever reached by a client that has stopped reading — a phone
// that went to sleep, or a laptop that was suspended mid-set.
const DefaultBuffer = 16

// Hub tracks who is watching which match.
//
// Lock ordering: the scorer lock may be taken before the hub lock, never after.
// Observe runs inside the scorer's lock and takes the hub's; nothing here takes
// a scorer lock at all.
type Hub struct {
	buffer int

	mu   sync.Mutex
	subs map[string]map[*Subscription]struct{}
}

// Option configures a Hub.
type Option func(*Hub)

// WithBuffer sets how many transitions a subscriber may queue.
func WithBuffer(n int) Option {
	return func(h *Hub) {
		if n > 0 {
			h.buffer = n
		}
	}
}

func New(opts ...Option) *Hub {
	h := &Hub{
		buffer: DefaultBuffer,
		subs:   make(map[string]map[*Subscription]struct{}),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Subscription is one watcher of one match. Close it when done; a subscription
// that is not closed keeps the hub holding a reference to its channel.
type Subscription struct {
	hub     *Hub
	matchID string
	events  chan Event
	done    chan struct{}

	dropped   atomic.Uint64
	closeOnce sync.Once
}

// Event is what a watcher receives: the kind of change and the state after it.
//
// The state is complete rather than a delta, which is what makes dropping one
// safe — a client that misses a transition and receives the next one is not
// missing anything the score depends on.
type Event struct {
	Kind  string       `json:"kind"`
	State scorer.State `json:"state"`
}

// KindSnapshot is the kind of the event sent on connect. It is not a
// transition: nothing happened, this is where the match stands.
const KindSnapshot = "snapshot"

// Subscribe registers a watcher of one match.
func (h *Hub) Subscribe(matchID string) *Subscription {
	sub := &Subscription{
		hub:     h,
		matchID: matchID,
		events:  make(chan Event, h.buffer),
		done:    make(chan struct{}),
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[matchID] == nil {
		h.subs[matchID] = make(map[*Subscription]struct{})
	}
	h.subs[matchID][sub] = struct{}{}

	return sub
}

// Observe is the scorer observer. Register it with [match.WithObserver].
//
// It runs inside the scorer's lock, so it never blocks: a subscriber that is
// not keeping up loses its oldest queued transition rather than holding up the
// match, or the other watchers.
func (h *Hub) Observe(t scorer.Transition) {
	h.publish(Event{Kind: string(t.Kind), State: t.State})
}

func (h *Hub) publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for sub := range h.subs[ev.State.MatchID] {
		sub.send(ev)
	}
}

// send queues ev, making room by discarding the oldest if it has to. Newest
// wins: a watcher that has fallen behind wants the current score, not a replay
// of the points it missed.
func (s *Subscription) send(ev Event) {
	select {
	case s.events <- ev:
		return
	default:
	}

	select {
	case <-s.events:
		s.dropped.Add(1)
	default:
		// Drained by the reader between the two selects. Nothing was lost.
	}

	select {
	case s.events <- ev:
	default:
		// Refilled by another sender in between, which cannot happen while
		// Observe holds the hub lock. Counted rather than looped on, so this
		// can never spin.
		s.dropped.Add(1)
	}
}

// Events is the channel to read transitions from. It is never closed — a
// reader stops on its own request context, and Close releases the hub's side.
func (s *Subscription) Events() <-chan Event { return s.events }

// Done is closed when the subscription is closed.
func (s *Subscription) Done() <-chan struct{} { return s.done }

// Dropped counts transitions discarded because this subscriber was not reading.
func (s *Subscription) Dropped() uint64 { return s.dropped.Load() }

// Close releases the subscription. Safe to call more than once.
func (s *Subscription) Close() {
	s.closeOnce.Do(func() {
		s.hub.remove(s)
		close(s.done)
	})
}

func (h *Hub) remove(sub *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()

	subs := h.subs[sub.matchID]
	delete(subs, sub)
	if len(subs) == 0 {
		// Otherwise the map keeps an entry per match ever streamed, which
		// outlives the match itself.
		delete(h.subs, sub.matchID)
	}
}

// Subscribers reports how many watchers a match has.
func (h *Hub) Subscribers(matchID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[matchID])
}

// Matches reports how many matches have a watcher. Zero once everyone has
// disconnected — which is what a leak would show up as.
func (h *Hub) Matches() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}
