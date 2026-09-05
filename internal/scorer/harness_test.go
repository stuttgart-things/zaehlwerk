package scorer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// clock is a hand-wound time source for the restart-detection tests.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }

// harness wraps a scorer with the bookkeeping every test needs: a fixed match
// id, per-source event ids that count up on their own, and a record of the
// transitions emitted.
type harness struct {
	t     *testing.T
	s     *Scorer
	clock *clock
	ids   map[string]uint64
	seen  []Transition
}

const defaultSource = "web-1"

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()

	if cfg.MatchID == "" {
		cfg.MatchID = "m1"
	}
	c := &clock{now: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)}
	if cfg.Now == nil {
		cfg.Now = c.Now
	}

	s, err := New(cfg)
	require.NoError(t, err)

	h := &harness{t: t, s: s, clock: c, ids: make(map[string]uint64)}
	s.Observe(func(tr Transition) { h.seen = append(h.seen, tr) })
	return h
}

// send fills in the match id and the next event id for the event's source and
// requires the event to be accepted without error.
func (h *harness) send(ev ScoreEvent) Result {
	h.t.Helper()

	res, err := h.apply(ev)
	require.NoError(h.t, err)
	return res
}

// apply is send without the error assertion, for the tests that want the error.
func (h *harness) apply(ev ScoreEvent) (Result, error) {
	h.t.Helper()

	if ev.MatchID == "" {
		ev.MatchID = h.s.cfg.MatchID
	}
	if ev.Source == "" {
		ev.Source = defaultSource
	}
	if ev.EventID == 0 {
		h.ids[ev.Source]++
		ev.EventID = h.ids[ev.Source]
	}
	return h.s.Apply(ev)
}

// raw applies ev exactly as given, filling in only the match id. The
// deduplication tests need control over the event id, including zero.
func (h *harness) raw(ev ScoreEvent) (Result, error) {
	h.t.Helper()

	if ev.MatchID == "" {
		ev.MatchID = h.s.cfg.MatchID
	}
	return h.s.Apply(ev)
}

// point awards one point to p.
func (h *harness) point(p Player) Result {
	h.t.Helper()
	return h.send(ScoreEvent{Player: p, Delta: 1})
}

// play awards one point per character of seq, "a" or "b".
func (h *harness) play(seq string) {
	h.t.Helper()
	for _, r := range seq {
		h.point(Player(string(r)))
	}
}

func (h *harness) state() State {
	h.t.Helper()
	return h.s.State()
}

func (h *harness) undo() State {
	h.t.Helper()

	st, err := h.s.Undo()
	require.NoError(h.t, err)
	return st
}

// kinds is the sequence of transition kinds emitted so far.
func (h *harness) kinds() []TransitionKind {
	out := make([]TransitionKind, 0, len(h.seen))
	for _, tr := range h.seen {
		out = append(out, tr.Kind)
	}
	return out
}

// repeat builds a point sequence, e.g. repeat("ab", 10) for twenty points.
func repeat(seq string, n int) string {
	out := ""
	for range n {
		out += seq
	}
	return out
}
