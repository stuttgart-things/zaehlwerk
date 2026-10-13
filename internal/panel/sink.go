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
	"strings"
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
	return homerun.Message{
		System:    system,
		Severity:  Severity(t.Kind),
		Title:     Title(t),
		Message:   Summary(t),
		Author:    author,
		Tags:      Tags(t),
		Timestamp: now.Format(time.RFC3339),
	}
}

// Tags is what a catcher filters on without learning any table tennis: which
// match and set, what happened, and which side it went to. A light at the
// table flashes in the colour of `side`; a Teams post waits for
// `transition=match_won` rather than for the wording of Summary.
//
// New tags go on the end. `match=` and `set=` come first and stay in that
// order, so a rule written against them before the others existed still
// matches. `side` is left out, not sent as `side=`, when the transition went
// to nobody — an undo, or a correction that lowered a score — so a rule that
// asks for a side cannot be satisfied by a transition that has none.
//
// After the routing tags comes the board: everything the catcher's `score`
// display needs to draw the whole match rather than the title alone — the
// points and sets, the names, who serves next and the sets already played. The
// catcher applies no rules, so who serves is said here, where the rules are;
// it is left out once the match is over, when nobody serves and `winner`
// says who won instead.
//
// Last come the phase and the ends (ADR-0005): `phase=warmup` and its siblings
// tell the panel to show an instruction instead of a score, and `ends=b:a`
// says who stands at the left and the right half, which is what a change of
// ends has to show. Both are left out of a state that has no phase, so a
// catcher that never heard of them sees the tags it always saw.
func Tags(t scorer.Transition) string {
	st := t.State
	tags := fmt.Sprintf("match=%s,set=%d,transition=%s", st.MatchID, st.SetNumber, t.Kind)
	if t.Side != "" {
		tags += ",side=" + string(t.Side)
	}

	tags += fmt.Sprintf(",points=%d:%d,sets=%d:%d", st.Points[0], st.Points[1], st.Sets[0], st.Sets[1])
	if st.Serving != "" && !st.Complete {
		tags += ",serve=" + string(st.Serving)
	}
	if st.Complete && st.Winner != "" {
		tags += ",winner=" + string(st.Winner)
	}
	tags += ",player_a=" + tagValue(st.Players[0]) + ",player_b=" + tagValue(st.Players[1])
	if len(st.CompletedSets) > 0 {
		played := make([]string, len(st.CompletedSets))
		for i, set := range st.CompletedSets {
			played[i] = fmt.Sprintf("%d-%d", set[0], set[1])
		}
		tags += ",played=" + strings.Join(played, " ")
	}
	if b := banner(t); b != "" {
		tags += ",banner=" + b
	}
	if st.Phase != "" {
		tags += ",phase=" + string(st.Phase)
	}
	if st.Ends[0] != "" && st.Ends[1] != "" {
		tags += ",ends=" + string(st.Ends[0]) + ":" + string(st.Ends[1])
	}
	return tags
}

// bannerName is as much of a name as the catcher's scoreboard shows: six
// characters, the same cut it makes for the names under the score.
const bannerName = 6

// banner is the status line the scoreboard draws under the names: who took
// the set, or the match. Only on the transition itself — the next point
// replaces it — so the board says what just happened without keeping it up.
// German, like the "SATZ 3" the catcher already draws above the score: this is
// what the table reads, not what a developer does.
//
// Outside play the banner is the instruction instead: what the players are
// to do next. A won set that starts a change of ends still says who won it;
// the instruction follows with the first press at the new end, and the
// `phase` tag carries it from the start for a catcher that draws phases.
func banner(t scorer.Transition) string {
	var prefix string
	switch t.Kind {
	case scorer.TransitionSetWon:
		prefix = "SATZ"
	case scorer.TransitionMatchWon:
		prefix = "SIEG"
	case scorer.TransitionServeDecided:
		prefix = "ANGABE"
	default:
		return phaseBanner(t.State.Phase)
	}

	var name string
	switch t.Side {
	case scorer.PlayerA:
		name = t.State.Players[0]
	case scorer.PlayerB:
		name = t.State.Players[1]
	default:
		return prefix
	}
	runes := []rune(tagValue(name))
	return prefix + " " + string(runes[:min(len(runes), bannerName)])
}

// phaseBanner is the instruction of a phase in which the players have
// something to do other than play a point.
func phaseBanner(p scorer.Phase) string {
	switch p {
	case scorer.PhaseWarmup:
		return "EINSPIELEN"
	case scorer.PhaseServeRally:
		return "UM DIE ANGABE"
	case scorer.PhaseChangeEnds:
		return "SEITENWECHSEL"
	default:
		return ""
	}
}

// tagValue keeps a player's name from splitting the tags: a comma would start
// a new tag, and the catcher's score payload is separated by semicolons. Both
// become spaces, which costs nothing on a panel that shows six characters.
func tagValue(name string) string {
	return strings.TrimSpace(strings.NewReplacer(",", " ", ";", " ").Replace(name))
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
