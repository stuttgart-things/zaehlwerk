// Package scorer owns the state of a running table tennis match: set logic,
// service rotation, undo and deduplication of incoming events.
//
// It is pure logic and does no I/O. The homerun pitcher and the SSE hub both
// consume it through [Scorer.Observe] rather than the other way round, so that
// the running score has exactly one writer — see docs/adr/0001 and 0002.
package scorer

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Defaults applied by [New] for the zero value of a [Config] field.
const (
	DefaultPointsPerSet = 11
	DefaultBestOf       = 5

	// DefaultRestartQuietPeriod is how long a source must have been silent
	// before a counter that jumps backwards is read as a firmware restart
	// rather than a duplicate. Retries arrive within seconds; a reflash or a
	// power cycle takes considerably longer.
	DefaultRestartQuietPeriod = time.Minute

	// DefaultRestartMaxEventID is the highest event id that a restarted source
	// is assumed to send first. A counter starts at zero after a reflash; the
	// margin covers the first few events being lost on the way.
	DefaultRestartMaxEventID = 8
)

// winBy is the lead required to take a set once PointsPerSet has been reached
// (ITTF law 2.11.1).
const winBy = 2

// Player identifies one side of the match. The values are the ones used on the
// wire in ADR-0002.
type Player string

const (
	PlayerA Player = "a"
	PlayerB Player = "b"
)

func (p Player) valid() bool { return p == PlayerA || p == PlayerB }

// Opponent returns the other player.
func (p Player) Opponent() Player {
	if p == PlayerA {
		return PlayerB
	}
	return PlayerA
}

// index returns 0 for a and 1 for b, and is only meaningful for a valid Player.
func (p Player) index() int {
	if p == PlayerB {
		return 1
	}
	return 0
}

// playerAt is the inverse of index. Only the low bit is read, so it composes
// with the parity arithmetic in serving.
func playerAt(i int) Player {
	if i&1 == 1 {
		return PlayerB
	}
	return PlayerA
}

// ScoreEvent is a single point as it reaches the scorer. Every ingest adapter
// normalises its own wire format into this shape (ADR-0002).
type ScoreEvent struct {
	MatchID string `json:"match_id"`
	Player  Player `json:"player"`
	Delta   int    `json:"delta"`
	// EventID is a counter, monotonic per Source. Anything at or below the
	// highest id already seen from that source is discarded.
	EventID uint64 `json:"event_id"`
	// Source names the sending device or browser session, not the transport.
	// One ESP-NOW hub posts on behalf of several buttons, and each button
	// counts for itself.
	Source string `json:"source"`
}

// State is a snapshot of the match. It is a value: nothing in it aliases the
// scorer's own state, so a consumer may hold on to it or hand it to a
// marshaller without a copy.
type State struct {
	MatchID string    `json:"match_id"`
	Players [2]string `json:"players"`
	// Points is the score of the set in progress, or of the final set once the
	// match is complete.
	Points [2]int `json:"points"`
	// Sets is the number of sets each player has won.
	Sets [2]int `json:"sets"`
	// SetNumber counts from 1.
	SetNumber int `json:"set_number"`
	// Serving is the player due to serve the next point.
	Serving  Player `json:"serving"`
	Complete bool   `json:"complete"`
	// Winner is only set once Complete is true.
	Winner Player `json:"winner,omitempty"`
	// CompletedSets holds the final score of every set played so far, in order,
	// for the result handed to Schmetterpause at match end.
	CompletedSets [][2]int `json:"completed_sets"`
}

// Outcome reports what [Scorer.Apply] did with an event.
type Outcome int

const (
	// Rejected is the zero value and the outcome whenever Apply returns an
	// error.
	Rejected Outcome = iota
	// Applied means the score changed.
	Applied
	// Ignored means the event was accepted and recorded but changed nothing.
	// The piezo adapter sends Delta 0 for a hit it cannot attribute.
	Ignored
	// Duplicate means the event id was at or below the watermark for its
	// source and the event was discarded. The caller answers with the current
	// state and a 200, so that a retrying sender stops retrying (ADR-0002).
	Duplicate
)

func (o Outcome) String() string {
	switch o {
	case Applied:
		return "applied"
	case Ignored:
		return "ignored"
	case Duplicate:
		return "duplicate"
	default:
		return "rejected"
	}
}

// Result is what Apply reports back. State is always the current state of the
// match, including when the error is non-nil, so an ingest endpoint can answer
// with it either way.
type Result struct {
	Outcome Outcome
	State   State
}

// TransitionKind says what happened to the match.
type TransitionKind string

const (
	TransitionPoint    TransitionKind = "point"
	TransitionSetWon   TransitionKind = "set_won"
	TransitionMatchWon TransitionKind = "match_won"
	// TransitionUndo is emitted when a point is taken back. Observers need it
	// for the same reason they need the others: a panel that misses it keeps
	// showing a point that no longer exists.
	TransitionUndo TransitionKind = "undo"
)

// Transition is one observable change of the match state.
type Transition struct {
	Kind  TransitionKind
	State State
}

// Errors returned by Apply and Undo. Callers classify with errors.Is.
var (
	ErrWrongMatch    = errors.New("event belongs to a different match")
	ErrUnknownPlayer = errors.New("unknown player")
	ErrMissingSource = errors.New("event has no source")
	ErrMatchComplete = errors.New("match is complete")
	ErrNothingToUndo = errors.New("nothing to undo")
)

// Config describes a match. The zero value of each field takes the documented
// default, so only MatchID and the player names are usually worth setting.
type Config struct {
	MatchID string
	// Players are display names, used by the pitcher to build the panel
	// message. They default to "A" and "B".
	Players [2]string
	// BestOf must be odd (ITTF law 2.12.1). The match is won at BestOf/2+1
	// sets.
	BestOf int
	// PointsPerSet is the score at which a set is won given a lead of two.
	// Service rotation stays on the modern two-point rule regardless of this
	// value, and the deuce threshold moves with it.
	PointsPerSet int
	// FirstServer serves the first point of the first set. Defaults to PlayerA.
	FirstServer Player

	// RestartQuietPeriod and RestartMaxEventID govern when a counter that jumps
	// backwards is read as a restarted source rather than a duplicate.
	RestartQuietPeriod time.Duration
	RestartMaxEventID  uint64

	// Now is the clock used for restart detection. Defaults to time.Now.
	Now func() time.Time
}

// match is the mutable core of the state. Everything derivable — who serves,
// which set number this is — is left out on purpose: a field that is stored is
// a field that undo and set boundaries can leave out of step with the score.
type match struct {
	points    [2]int
	sets      [2]int
	setIndex  int // 0-based
	completed [][2]int
	complete  bool
	winner    Player
}

func (m match) clone() match {
	c := m
	c.completed = slices.Clone(m.completed)
	return c
}

// source is what the scorer remembers about one sender for deduplication.
type source struct {
	high uint64
	// lastSeen is updated for every event from the source, discarded ones
	// included: a sender that is spraying retries is not quiet.
	lastSeen time.Time
}

// Scorer is the only writer of one match's state. It is safe for concurrent
// use.
type Scorer struct {
	cfg       Config
	setsToWin int

	mu   sync.Mutex
	m    match
	undo []match
	seen map[string]source

	observers []func(Transition)
}

// New validates cfg, applies defaults and returns a scorer for a match at 0:0.
func New(cfg Config) (*Scorer, error) {
	if cfg.MatchID == "" {
		return nil, errors.New("scorer: match id is required")
	}
	if cfg.FirstServer == "" {
		cfg.FirstServer = PlayerA
	}
	if !cfg.FirstServer.valid() {
		return nil, fmt.Errorf("scorer: first server: %w: %q", ErrUnknownPlayer, cfg.FirstServer)
	}
	if cfg.BestOf == 0 {
		cfg.BestOf = DefaultBestOf
	}
	if cfg.BestOf < 1 || cfg.BestOf%2 == 0 {
		return nil, fmt.Errorf("scorer: best of %d: must be odd and positive", cfg.BestOf)
	}
	if cfg.PointsPerSet == 0 {
		cfg.PointsPerSet = DefaultPointsPerSet
	}
	if cfg.PointsPerSet < winBy {
		return nil, fmt.Errorf("scorer: points per set %d: must be at least %d", cfg.PointsPerSet, winBy)
	}
	for i, name := range cfg.Players {
		if name == "" {
			cfg.Players[i] = string(playerAt(i))
		}
	}
	if cfg.RestartQuietPeriod == 0 {
		cfg.RestartQuietPeriod = DefaultRestartQuietPeriod
	}
	if cfg.RestartMaxEventID == 0 {
		cfg.RestartMaxEventID = DefaultRestartMaxEventID
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	return &Scorer{
		cfg:       cfg,
		setsToWin: cfg.BestOf/2 + 1,
		seen:      make(map[string]source),
	}, nil
}

// Observe registers fn to be called on every transition.
//
// Observers are called in registration order, from inside the scorer's lock, so
// that they see transitions in the order they happened and cannot interleave
// with a concurrent point. An observer must therefore not block: hand off to a
// buffered channel and return, and drop rather than wait. The panel and the
// live view are both allowed to fall behind; the match is not.
func (s *Scorer) Observe(fn func(Transition)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observers = append(s.observers, fn)
}

// State returns the current state of the match.
func (s *Scorer) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state()
}

// Apply applies one event. Duplicates and events that change nothing are
// reported through Result.Outcome rather than as errors; only a malformed event
// or a match that is already over produces one.
func (s *Scorer) Apply(ev ScoreEvent) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case ev.MatchID != s.cfg.MatchID:
		return Result{State: s.state()}, fmt.Errorf("%w: %q", ErrWrongMatch, ev.MatchID)
	case !ev.Player.valid():
		return Result{State: s.state()}, fmt.Errorf("%w: %q", ErrUnknownPlayer, ev.Player)
	case ev.Source == "":
		return Result{State: s.state()}, ErrMissingSource
	case s.m.complete:
		return Result{State: s.state()}, ErrMatchComplete
	}

	if !s.accept(ev) {
		return Result{Outcome: Duplicate, State: s.state()}, nil
	}

	next := s.m.clone()
	i := ev.Player.index()
	next.points[i] = max(0, next.points[i]+ev.Delta)
	if next.points == s.m.points {
		// Recorded but without effect — a piezo hit that could not be
		// attributed. Nothing changed, so there is nothing to emit and nothing
		// for undo to take back.
		return Result{Outcome: Ignored, State: s.state()}, nil
	}

	kind := TransitionPoint
	// Who took the set is decided by the score, not by who sent the event. A
	// correction that lowers one side can complete a set for the other.
	if w, won := s.setWinner(next.points); won {
		next.completed = append(next.completed, next.points)
		next.sets[w]++
		if next.sets[w] >= s.setsToWin {
			next.complete = true
			next.winner = playerAt(w)
			kind = TransitionMatchWon
		} else {
			next.setIndex++
			next.points = [2]int{}
			kind = TransitionSetWon
		}
	}

	s.push()
	s.m = next

	st := s.state()
	s.emit(Transition{Kind: kind, State: st})
	return Result{Outcome: Applied, State: st}, nil
}

// Undo takes back the last event that changed the score.
//
// A point that won a set can still be taken back until a point of the next set
// has been applied — that is the mis-press that actually happens at the table.
// Once the new set has started, the previous one is closed and undo stops at
// the start of the set in progress.
func (s *Scorer) Undo() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := len(s.undo)
	if n == 0 {
		return s.state(), ErrNothingToUndo
	}
	s.m = s.undo[n-1]
	s.undo = s.undo[:n-1]

	// Deduplication watermarks are deliberately not rolled back. Undo is a
	// decision by the scorekeeper; a retry is an accident of the transport. A
	// source that re-sends the event just undone must still be discarded, or
	// every undo would be undone by the next retry.

	st := s.state()
	s.emit(Transition{Kind: TransitionUndo, State: st})
	return st, nil
}

// push records the current state so that the event about to be applied can be
// taken back.
func (s *Scorer) push() {
	// The completed set's events stop being undoable as soon as a point of the
	// new set is applied. Until then the stack still belongs to the set that
	// just ended, which is what lets its final point be taken back.
	if n := len(s.undo); n > 0 && s.undo[n-1].setIndex != s.m.setIndex {
		s.undo = s.undo[:0]
	}
	s.undo = append(s.undo, s.m.clone())
}

// accept records ev against its source's watermark and reports whether it is
// new. See ADR-0002 for why this exists and why a duplicate is not an error.
func (s *Scorer) accept(ev ScoreEvent) bool {
	now := s.cfg.Now()
	prev, known := s.seen[ev.Source]

	switch {
	case !known, ev.EventID > prev.high:
	case s.restarted(prev, ev.EventID, now):
		// A counter that has fallen back to near zero after a long silence is a
		// source that was reflashed or lost power, not a duplicate. Locking it
		// out until the end of the match would be worse than the alternative:
		// a genuine duplicate that arrives this late is applied. Retries are a
		// matter of seconds.
	default:
		prev.lastSeen = now
		s.seen[ev.Source] = prev
		return false
	}

	s.seen[ev.Source] = source{high: ev.EventID, lastSeen: now}
	return true
}

func (s *Scorer) restarted(prev source, id uint64, now time.Time) bool {
	return id <= s.cfg.RestartMaxEventID && now.Sub(prev.lastSeen) >= s.cfg.RestartQuietPeriod
}

// setWinner reports who has taken the set at these points, if anyone. Both
// players cannot satisfy it at once (ITTF law 2.11.1).
func (s *Scorer) setWinner(points [2]int) (int, bool) {
	for i, p := range points {
		if p >= s.cfg.PointsPerSet && p-points[1-i] >= winBy {
			return i, true
		}
	}
	return 0, false
}

func (s *Scorer) state() State {
	return State{
		MatchID:       s.cfg.MatchID,
		Players:       s.cfg.Players,
		Points:        s.m.points,
		Sets:          s.m.sets,
		SetNumber:     s.m.setIndex + 1,
		Serving:       s.serving(),
		Complete:      s.m.complete,
		Winner:        s.m.winner,
		CompletedSets: slices.Clone(s.m.completed),
	}
}

// serving derives who serves the next point from the first server, the set
// number and the score of the set in progress. Nothing about service is stored,
// which is what makes undo and set boundaries correct without either of them
// knowing about service at all.
func (s *Scorer) serving() Player {
	// Whoever served first in a set receives first in the next one
	// (ITTF law 2.13.6).
	first := s.cfg.FirstServer.index() ^ (s.m.setIndex % 2)

	a, b := s.m.points[0], s.m.points[1]
	deuce := s.cfg.PointsPerSet - 1

	// Service changes after every two points, and after every point once both
	// players have reached deuce (ITTF law 2.13.3). The two branches agree at
	// exactly deuce:deuce, where both count deuce changes, so the switch to
	// single service is seamless — and since that count is even for the
	// standard eleven-point set, the player who opened the set also serves the
	// first point of the deuce.
	var changes int
	if a >= deuce && b >= deuce {
		changes = deuce + (a + b - 2*deuce)
	} else {
		changes = (a + b) / 2
	}

	return playerAt(first ^ (changes % 2))
}

func (s *Scorer) emit(t Transition) {
	for _, fn := range s.observers {
		fn(t)
	}
}
