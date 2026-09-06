// Package panel publishes the running score to the LED panel.
//
// It is the pitcher half of the panel path: scorer transitions in, homerun
// messages on a Redis stream out, where homerun2-led-catcher picks them up and
// renders them. Nothing here reads back from Redis and nothing here can fail a
// match — see the Sink documentation for why.
package panel

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	homerun "github.com/stuttgart-things/homerun-library/v4"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// Defaults for Config.
const (
	DefaultStream       = "tabletennis"
	DefaultSystem       = "tabletennis"
	DefaultAuthor       = "zaehlwerk"
	DefaultQueueSize    = 64
	DefaultPitchTimeout = 2 * time.Second
	DefaultDrainTimeout = 2 * time.Second
)

// Pitcher is the part of homerun.Pitcher the sink uses. *homerun.Pitcher
// satisfies it; a test double can too, which is what keeps the unit tests off
// Redis.
type Pitcher interface {
	Enqueue(ctx context.Context, msg homerun.Message, streamOverride ...string) (objectID, streamID string, err error)
	Close() error
}

// Config configures a Sink. The zero value is usable: every field falls back to
// its default.
type Config struct {
	// Stream is the Redis stream to publish to. ADR-0003 gives the panel its
	// own stream so the score is not interleaved with other notifications.
	Stream string
	// System and Author land in the homerun message. System is what the
	// catcher's display rules match on, so changing it needs a matching
	// profile change on the panel side.
	System string
	Author string
	// QueueSize bounds how far the panel may fall behind before points are
	// dropped rather than queued.
	QueueSize int
	// PitchTimeout bounds one publish. A hung Redis must not stall the queue
	// for longer than this.
	PitchTimeout time.Duration
	// DrainTimeout bounds how long Close spends publishing what is still
	// queued. The last transition of a match is usually the final score, and
	// discarding it on shutdown would leave the panel a set behind.
	DrainTimeout time.Duration

	Logger *slog.Logger
	// Now is the clock stamped onto messages. Overridable for tests.
	Now func() time.Time
}

func (c Config) withDefaults() Config {
	if c.Stream == "" {
		c.Stream = DefaultStream
	}
	if c.System == "" {
		c.System = DefaultSystem
	}
	if c.Author == "" {
		c.Author = DefaultAuthor
	}
	if c.QueueSize <= 0 {
		c.QueueSize = DefaultQueueSize
	}
	if c.PitchTimeout <= 0 {
		c.PitchTimeout = DefaultPitchTimeout
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = DefaultDrainTimeout
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Sink turns scorer transitions into homerun messages on the panel stream.
//
// It never blocks the scorer and never fails a point. Observe is called from
// inside the scorer's lock, so it does no more than hand the transition to a
// buffered channel; a full channel drops rather than waits. A publish that
// fails is logged and forgotten — no queue, no retry. The score on a phone
// matters more than the score on the panel, and a point re-sent thirty seconds
// late would be worse than one never sent.
type Sink struct {
	pitcher Pitcher
	cfg     Config

	queue chan scorer.Transition
	stop  chan struct{}
	done  chan struct{}

	dropped   atomic.Uint64
	failed    atomic.Uint64
	closeOnce sync.Once
	closeErr  error
}

// New starts a sink publishing through p. The sink owns p from here on and
// closes it in Close.
func New(p Pitcher, cfg Config) *Sink {
	cfg = cfg.withDefaults()
	s := &Sink{
		pitcher: p,
		cfg:     cfg,
		queue:   make(chan scorer.Transition, cfg.QueueSize),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go s.run()
	return s
}

// Observe is the scorer observer. Register it with (*scorer.Scorer).Observe.
//
// The queue holds transitions rather than finished messages so that formatting
// happens on the worker, not under the scorer's lock.
func (s *Sink) Observe(t scorer.Transition) {
	select {
	case <-s.stop:
		// Counted rather than quietly ignored. A transition arriving after
		// Close is undelivered like any other, and a Dropped() of zero has to
		// mean the panel saw everything.
		s.dropped.Add(1)
		return
	default:
	}

	select {
	case s.queue <- t:
	default:
		// Deliberately counted, not logged: this runs under the scorer's lock,
		// and a Redis outage would otherwise write a log line per point from
		// inside it. Close reports the total.
		s.dropped.Add(1)
	}
}

// Dropped is the number of transitions that never reached the queue because it
// was full.
func (s *Sink) Dropped() uint64 { return s.dropped.Load() }

// Failed is the number of transitions that reached the queue but whose publish
// returned an error.
func (s *Sink) Failed() uint64 { return s.failed.Load() }

// Close stops the sink, publishes what is still queued within DrainTimeout, and
// closes the pitcher. It is safe to call more than once.
func (s *Sink) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
		if dropped, failed := s.Dropped(), s.Failed(); dropped > 0 || failed > 0 {
			s.cfg.Logger.Warn("panel sink did not deliver every transition",
				"dropped", dropped, "failed", failed, "stream", s.cfg.Stream)
		}
		s.closeErr = s.pitcher.Close()
	})
	return s.closeErr
}

func (s *Sink) run() {
	defer close(s.done)
	for {
		select {
		case t := <-s.queue:
			s.pitch(t)
		case <-s.stop:
			s.drain()
			return
		}
	}
}

// drain publishes what is queued at the moment Close was called. It is bounded
// twice over — by the queue length it saw on entry and by DrainTimeout — so a
// scorer still emitting, or a Redis that has stopped answering, cannot hold
// shutdown open.
func (s *Sink) drain() {
	deadline := s.cfg.Now().Add(s.cfg.DrainTimeout)
	for range len(s.queue) {
		select {
		case t := <-s.queue:
			if !s.cfg.Now().Before(deadline) {
				s.dropped.Add(1)
				continue
			}
			s.pitch(t)
		default:
			return
		}
	}
}

func (s *Sink) pitch(t scorer.Transition) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.PitchTimeout)
	defer cancel()

	msg := Message(t, s.cfg.System, s.cfg.Author, s.cfg.Now())
	if _, _, err := s.pitcher.Enqueue(ctx, msg, s.cfg.Stream); err != nil {
		s.failed.Add(1)
		s.cfg.Logger.Warn("panel pitch failed, point not shown",
			"error", err,
			"stream", s.cfg.Stream,
			"match", t.State.MatchID,
			"kind", string(t.Kind),
			"title", msg.Title,
		)
		return
	}
	s.cfg.Logger.Debug("pitched to panel",
		"stream", s.cfg.Stream, "match", t.State.MatchID, "title", msg.Title)
}

// Message renders a transition as a homerun message.
//
// It is exported and pure so that the panel wording can be checked, and
// changed, without a Redis anywhere near it.
func Message(t scorer.Transition, system, author string, now time.Time) homerun.Message {
	st := t.State
	return homerun.Message{
		System:    system,
		Severity:  Severity(t.Kind),
		Title:     Title(t),
		Message:   Summary(t),
		Author:    author,
		Tags:      fmt.Sprintf("match=%s,set=%d", st.MatchID, st.SetNumber),
		Timestamp: now.Format(time.RFC3339),
	}
}

// Severity maps a transition to a homerun severity, which is what the catcher
// picks the text colour from. Set and match wins stand out; everything else,
// a taken-back point included, is an ordinary update.
func Severity(kind scorer.TransitionKind) string {
	switch kind {
	case scorer.TransitionSetWon, scorer.TransitionMatchWon:
		return "SUCCESS"
	default:
		return "INFO"
	}
}

// Title is the whole of what the panel shows: the catcher's display rule
// renders `{{ title }}` with a 6x10 font on a 64x64 matrix, which is about ten
// characters. Anything longer is not shortened, it is cut off by the edge of
// the panel.
//
// The prefixes are what separates a set score from a point score at a glance —
// two players at 2:1 in sets and 2:1 in points are otherwise the same three
// characters. Whether this is the right thing to show is a decision for the
// table, not for this file; see the panel section of the README for the
// simulator that makes it decidable.
func Title(t scorer.Transition) string {
	st := t.State
	switch t.Kind {
	case scorer.TransitionSetWon:
		return fmt.Sprintf("SET %d:%d", st.Sets[0], st.Sets[1])
	case scorer.TransitionMatchWon:
		return fmt.Sprintf("WIN %d:%d", st.Sets[0], st.Sets[1])
	default:
		return fmt.Sprintf("%d:%d", st.Points[0], st.Points[1])
	}
}

// Summary is the long form, for the catcher's log and the simulator's event
// list. It never reaches the matrix itself.
func Summary(t scorer.Transition) string {
	st := t.State
	a, b := st.Players[0], st.Players[1]

	switch t.Kind {
	case scorer.TransitionMatchWon:
		return fmt.Sprintf("%s %d : %d %s — %s wins", a, st.Sets[0], st.Sets[1], b, winnerName(st))
	case scorer.TransitionSetWon:
		return fmt.Sprintf("%s %d : %d %s — set %d, sets %d:%d",
			a, st.Points[0], st.Points[1], b, len(st.CompletedSets), st.Sets[0], st.Sets[1])
	case scorer.TransitionUndo:
		return fmt.Sprintf("%s %d : %d %s — corrected", a, st.Points[0], st.Points[1], b)
	default:
		return fmt.Sprintf("%s %d : %d %s", a, st.Points[0], st.Points[1], b)
	}
}

// winnerName resolves the winning player to their name. Player.index is
// unexported and the scorer is not worth widening for one caller, so the
// mapping is repeated here rather than exported there.
func winnerName(st scorer.State) string {
	switch st.Winner {
	case scorer.PlayerA:
		return st.Players[0]
	case scorer.PlayerB:
		return st.Players[1]
	default:
		return string(st.Winner)
	}
}
