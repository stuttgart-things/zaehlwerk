package pong

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"slices"
	"sync"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// BotNames are what the bots are called on the panel and the page. Free text,
// never an id (ADR-0006), and short enough that the panel's banner, which cuts
// a name at six characters, says all of it. Until zaehlwerk can mark a match
// as a demo, the name is what tells a passer-by that nobody is playing.
var BotNames = [2]string{"Pong A", "Pong B"}

// maxReports ends a game that stops moving. A best of seven to eleven is a few
// hundred reports; a game that has sent this many is pressing a half that
// zaehlwerk keeps ignoring, and would otherwise press it forever.
const maxReports = 5000

// Status is where a game stands.
type Status string

const (
	StatusPlaying Status = "playing"
	// StatusWon is the match played to its last point.
	StatusWon Status = "won"
	// StatusEnded is a match the game did not finish: ended on the page,
	// displaced by another match, left by its browser, or the service
	// shutting down.
	StatusEnded  Status = "ended"
	StatusFailed Status = "failed"
)

// EndsChange is a change of ends as zaehlwerk called it: the set and the score
// the players walked at. After a set, that is the next set at 0:0.
type EndsChange struct {
	Set    int    `json:"set"`
	Points [2]int `json:"points"`
}

// View is a game as GET /games/{id} shows it.
type View struct {
	ID      string    `json:"id"`
	Source  string    `json:"source"`
	MatchID string    `json:"match_id"`
	Status  Status    `json:"status"`
	Reason  string    `json:"reason,omitempty"`
	Seed    int64     `json:"seed"`
	Players [2]string `json:"players"`
	// Driver is who decides the rallies: "bots" headless, or "browser".
	Driver string `json:"driver"`
	// Mode and Bots are a browser game's: "pvp", "pvc" or "cvc", and the bot
	// playing as player a and b, or none for a person.
	Mode string        `json:"mode,omitempty"`
	Bots [2]*BotParams `json:"bots,omitempty"`
	// LastPress is the browser's last press taken, so a page that reloads
	// numbers on from there.
	LastPress uint64   `json:"last_press,omitempty"`
	Skills    [2]Skill `json:"skills"`
	// Rallies is the rallies played for points; the serve rally is not one.
	Rallies   int    `json:"rallies"`
	Truth     [2]int `json:"truth"`
	Zaehlwerk [2]int `json:"zaehlwerk"`
	// Explained is how far the injected faults should have moved zaehlwerk's
	// points from the truth.
	Explained   [2]int       `json:"explained"`
	Divergences []Divergence `json:"divergences"`
	// Unexplained counts the divergences no injected fault explains: the
	// findings. A run is good when it is 0, with faults or without.
	Unexplained int          `json:"unexplained"`
	FaultShares Faults       `json:"fault_shares"`
	Faults      []Fault      `json:"faults"`
	EndsChanges []EndsChange `json:"ends_changes"`
	State       scorer.State `json:"state"`
}

// GameConfig is one match: headless between two bots, or played in a
// browser.
type GameConfig struct {
	BestOf int
	// Players are the names on the panel; BotNames when empty.
	Players [2]string
	// Driver is DriverBots or DriverBrowser; empty is bots.
	Driver string
	// Mode and Bots are what a browser game shows and plays with; the game
	// itself does not use them.
	Mode string
	Bots [2]*BotParams
	// Idle ends a browser game nobody presses in; zero is browserIdle.
	Idle   time.Duration
	Skills [2]Skill
	// Pace is the pause between two reports: zero for a test, a second or so
	// for somebody watching the panel.
	Pace time.Duration
	Seed int64
	// Faults is what goes wrong between the truth and zaehlwerk, drawn from
	// the seed like the rallies.
	Faults Faults
	// ResetQuiet is the gap before a counter reset; zero is resetQuiet. A
	// field so the tests do not sit through the minute.
	ResetQuiet time.Duration
}

// Game is one headless bot match: two bots, rallies drawn by skill, every
// result reported to zaehlwerk and checked against the truth.
type Game struct {
	ID     string
	Source string

	cfg    GameConfig
	client *Client
	log    *slog.Logger
	driver Driver
	// faultRnd draws the faults. Separate from the bots' draws, so a game
	// without faults draws exactly the rallies it drew before faults existed,
	// and the same seed is the same match in every run.
	faultRnd *rand.Rand
	done     chan struct{}
	// token is what a browser game's presses must carry. Handed out once,
	// to the page that started it; empty for a headless game.
	token string
	// stop ends the game from outside, with the reason as its cause.
	stop context.CancelCauseFunc

	mu          sync.Mutex
	matchID     string
	status      Status
	reason      string
	state       scorer.State
	truth       Truth
	rallies     int
	divergences []Divergence
	changes     []EndsChange
	faults      []Fault
	// wake is closed and replaced whenever the view changes, so a page's
	// stream sends a new one instead of polling.
	wake chan struct{}
	// parted is how truth and zaehlwerk stood at the last comparison: the
	// difference in points, and the part of it the faults explain. A points
	// divergence is recorded when that changes, so a lost press is one entry
	// and not one for every rally after it.
	parted [2][2]int

	// rally numbers the game's presses: every press takes the next one, a
	// lost one included, and it never resets. counter is the event id sent,
	// the same number until a counter reset starts it again at 1 (ADR-0006).
	// Only the goroutine that plays touches either.
	rally   uint64
	counter uint64
}

// faultSalt keeps the fault draws of one game from being the rally draws of
// the next, whose seed is one higher.
const faultSalt int64 = 0x0fa017

func newGame(id string, cfg GameConfig, client *Client, log *slog.Logger) *Game {
	source := "pong-" + id
	if cfg.Players == ([2]string{}) {
		cfg.Players = BotNames
	}
	if cfg.Driver == "" {
		cfg.Driver = DriverBots
	}
	var driver Driver = &bots{rnd: rand.New(rand.NewSource(cfg.Seed)), skills: cfg.Skills}
	if cfg.Driver == DriverBrowser {
		driver = newBrowser(cfg.Idle)
	}
	return &Game{
		ID:          id,
		Source:      source,
		cfg:         cfg,
		client:      client,
		log:         log.With("game", id, "source", source),
		driver:      driver,
		faultRnd:    rand.New(rand.NewSource(cfg.Seed ^ faultSalt)),
		done:        make(chan struct{}),
		stop:        func(error) {},
		status:      StatusPlaying,
		divergences: []Divergence{},
		changes:     []EndsChange{},
		faults:      []Fault{},
		wake:        make(chan struct{}),
	}
}

// changed wakes every stream waiting on the view. Called with g.mu held.
func (g *Game) changed() {
	close(g.wake)
	g.wake = make(chan struct{})
}

// Changes is closed at the next change of the view.
func (g *Game) Changes() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.wake
}

// start creates the match. It is separate from play so that a match zaehlwerk
// refuses to create is the caller's error, not a game that failed later.
func (g *Game) start(ctx context.Context) error {
	st, err := g.client.CreateMatch(ctx, g.cfg.Players, g.cfg.BestOf)
	if err != nil {
		return fmt.Errorf("starting the match: %w", err)
	}
	g.mu.Lock()
	g.matchID, g.state = st.MatchID, st
	g.changed()
	g.mu.Unlock()
	g.log.Info("match started", "match_id", st.MatchID, "players", st.Players, "driver", g.cfg.Driver,
		"best_of", g.cfg.BestOf, "skills", []string{g.cfg.Skills[0].Name, g.cfg.Skills[1].Name},
		"seed", g.cfg.Seed)
	return nil
}

// errDisplaced is another match becoming current at the table while the game
// plays: somebody started a real one.
var errDisplaced = errors.New("another match became current at the table")

// errTableLost is /live/stream closing while the game plays. Without it the
// game cannot see another match become current, and would play a match that is
// no longer the table's to the end, with its transitions on the panel. It is
// treated like a displacement; reconnecting is not done yet.
var errTableLost = errors.New("lost sight of the table")

// errStopped is a browser game ended by its own page.
var errStopped = errors.New("ended in the browser")

// run plays the match to its end and watches the table while it does. table
// is /live/stream, already past the event that said the table was free.
func (g *Game) run(ctx context.Context, table <-chan TableEvent) {
	defer close(g.done)
	// After the status is set and before done closes, so a press waiting
	// for its answer gets the last state and finds the game over.
	defer func() { g.driver.Over(g.current()) }()

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go g.watch(ctx, cancel, table)

	err := g.play(ctx)
	if cause := context.Cause(ctx); err != nil && cause != nil && !errors.Is(cause, context.Canceled) {
		err = cause
	}

	switch {
	case err == nil:
		g.finish(StatusWon, "")
		return
	case errors.Is(err, ErrMatchOver):
		g.finish(StatusEnded, "zaehlwerk says the match is over")
		return
	case errors.Is(err, errDisplaced), errors.Is(err, errTableLost), errors.Is(err, errStopped),
		errors.Is(err, errIdle), ctx.Err() != nil:
		// The match is no longer the game's to play, the game can no longer
		// tell whether it is, nobody is playing it any more, or the service
		// is stopping. Either way it is ended rather than left running, so it
		// does not sit in the registry as current with nobody scoring it.
		reason := "the service stopped"
		for _, known := range []error{errDisplaced, errTableLost, errStopped, errIdle} {
			if errors.Is(err, known) {
				reason = err.Error()
			}
		}
		g.end(ctx)
		g.finish(StatusEnded, reason)
		return
	}
	g.end(ctx)
	g.finish(StatusFailed, err.Error())
}

// watch reads the table while the game plays. A snapshot of another match
// means that match is now current: somebody started it, and it is theirs. A
// stream that closes leaves the game blind to that, so it ends the game too.
func (g *Game) watch(ctx context.Context, cancel context.CancelCauseFunc, table <-chan TableEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-table:
			if !ok {
				// Also closed when ctx ends. Only the first cause counts, and
				// a game that won or is stopping is not one that got lost.
				if ctx.Err() == nil {
					g.log.Warn("/live/stream closed, ending our match")
					cancel(errTableLost)
				}
				return
			}
			if ev.Kind == kindSnapshot && ev.State != nil && ev.State.MatchID != g.matchIDNow() {
				g.log.Info("another match is current, ending ours",
					"other", ev.State.MatchID, "reported", ev.Reported())
				cancel(errDisplaced)
				return
			}
		}
	}
}

// play is the match from the warm-up to the last point. Every decision about
// where the match stands is read from zaehlwerk's last answer; the driver only
// decides rallies.
func (g *Game) play(ctx context.Context) error {
	st := g.current()

	// Where the bots stand. Taken from zaehlwerk at the start and at every
	// change of ends — they walk where they are sent — and otherwise not
	// re-read, so that ends moving without a change of ends show up as a
	// divergence instead of being followed.
	standing := st.Ends
	// acted is the phase the last report was sent in. A change of ends is
	// walked once, on the answer that called it, and not again for the
	// second player's press.
	acted := st.Phase

	for st.Phase != scorer.PhaseComplete {
		if g.rally >= maxReports {
			return fmt.Errorf("%d reports and the match has not ended, stuck in %s", g.rally, st.Phase)
		}
		if err := g.pause(ctx); err != nil {
			return err
		}

		if st.Phase == scorer.PhaseChangeEnds && acted != scorer.PhaseChangeEnds {
			standing = st.Ends
			g.walked(st)
		}
		acted = st.Phase

		var err error
		switch st.Phase {
		case scorer.PhaseWarmup, scorer.PhaseChangeEnds:
			st, err = g.ready(ctx, st)
		case scorer.PhaseServeRally:
			st, err = g.serveRally(ctx, st, standing)
		case scorer.PhasePlaying:
			st, err = g.point(ctx, st, standing)
		default:
			return fmt.Errorf("zaehlwerk is in a phase the game does not know: %q", st.Phase)
		}
		if err != nil {
			return err
		}
	}

	v := g.View()
	g.log.Info("match won", "match_id", st.MatchID,
		"winner", st.Players[index(st.Winner)], "sets", st.Sets,
		"completed_sets", st.CompletedSets, "rallies", v.Rallies,
		"faults", len(v.Faults), "divergences", len(v.Divergences), "unexplained", v.Unexplained)
	return nil
}

// ready presses for the half the driver says: a player ready after the
// warm-up, or arrived at the new end.
func (g *Game) ready(ctx context.Context, st scorer.State) (scorer.State, error) {
	half, err := g.driver.Ready(ctx, st)
	if err != nil {
		return st, err
	}
	// A browser can press for a half that already has. zaehlwerk ignores
	// that, so it is not a press that must change the state.
	after, n, err := g.report(ctx, half, slices.Contains(st.Waiting, half))
	if err != nil {
		return after, err
	}
	g.log.Info("ready", "rally", n, "event_id", g.counter, "phase", st.Phase, "half", half,
		"next", after.Phase, "waiting", after.Waiting)

	// The same half pressing again while the other is still awaited. It is
	// recorded and changes nothing: zaehlwerk answers it Ignored. The answer
	// carries no outcome, so "changes nothing" is the state staying the same.
	waiting := after.Phase == scorer.PhaseWarmup || after.Phase == scorer.PhaseChangeEnds
	if waiting && g.roll(g.cfg.Faults.WrongPhase) {
		again, n, err := g.report(ctx, half, false)
		if err != nil {
			return again, err
		}
		g.injected(Fault{Rally: n, EventID: g.counter, Kind: FaultWrongPhase, Phase: after.Phase, Half: half})
		if !same(after, again) {
			g.diverged(Divergence{Rally: n, What: string(FaultWrongPhase),
				Truth: "ignored", Zaehlwerk: "applied: " + summary(again)})
		}
		after = again
	}
	return after, nil
}

// serveRally plays for the first serve. Its winner's half is sent once, and
// zaehlwerk decides from it who serves; the game checks that it decided for
// the right player.
func (g *Game) serveRally(ctx context.Context, st scorer.State, standing [2]scorer.Player) (scorer.State, error) {
	winner, err := g.driver.Rally(ctx, st, standing)
	if err != nil {
		return st, err
	}
	g.mu.Lock()
	g.truth.ServeRally, g.truth.ExplainedServe = winner, winner
	g.changed()
	g.mu.Unlock()

	half := halfOf(winner, standing)
	after, n, err := g.press(ctx, half, winner)
	if err != nil {
		return after, err
	}
	g.log.Info("serve rally", "rally", n, "event_id", g.counter, "winner", after.Players[index(winner)],
		"half", half, "serving", after.Serving)
	if d, off := g.truthNow().Serve(n, after); off {
		g.diverged(d)
	}
	return after, nil
}

// point plays one rally for a point, served by whoever zaehlwerk says serves,
// and reports it for the half its winner stands at. The faults that belong to
// a rally for a point happen here: a ghost before it, the press lost, or a
// bounce after it.
func (g *Game) point(ctx context.Context, st scorer.State, standing [2]scorer.Player) (scorer.State, error) {
	if d, off := Ends(g.rally+1, standing, st); off {
		g.diverged(d)
	}

	if g.roll(g.cfg.Faults.Ghost) {
		half := scorer.HalfLeft
		if g.faultRnd.Intn(2) == 1 {
			half = scorer.HalfRight
		}
		after, n, err := g.report(ctx, half, true)
		if err != nil {
			return after, err
		}
		g.injected(Fault{Rally: n, EventID: g.counter, Kind: FaultGhost, Phase: st.Phase,
			Half: half, Effect: effectAt(half, standing)})
		g.points(n, after)
		// A ghost can end a set. The rally is then played after the change
		// of ends, from the top of the loop, like any other.
		if after.Phase != scorer.PhasePlaying {
			return after, nil
		}
		st = after
	}

	winner, err := g.driver.Rally(ctx, st, standing)
	if err != nil {
		return st, err
	}
	g.mu.Lock()
	g.truth.Rally(winner)
	g.rallies++
	g.changed()
	g.mu.Unlock()
	half := halfOf(winner, standing)

	if g.roll(g.cfg.Faults.Lost) {
		// The press was made and took its number; it never arrived. The
		// match goes on from where zaehlwerk has it, one point short.
		g.rally++
		g.counter++
		var effect [2]int
		effect[index(winner)] = -1
		g.injected(Fault{Rally: g.rally, Kind: FaultLost, Phase: st.Phase, Half: half, Effect: effect})
		g.points(g.rally, st)
		return st, nil
	}

	after, n, err := g.press(ctx, half, winner)
	if err != nil {
		return after, err
	}
	truth := g.truthNow()
	g.log.Info("rally", "rally", n, "event_id", g.counter, "server", st.Players[index(st.Serving)],
		"winner", st.Players[index(winner)], "half", half,
		"truth", score(truth.Won), "zaehlwerk", score(Awarded(after)),
		"points", after.Points, "sets", after.Sets, "phase", after.Phase)
	g.points(n, after)

	// Only while points are still played: after a set or the match the same
	// press would be a confirmation or refused, and no longer a point.
	if after.Phase == scorer.PhasePlaying && g.roll(g.cfg.Faults.Bounce) {
		bounced, n, err := g.report(ctx, half, true)
		if err != nil {
			return bounced, err
		}
		g.injected(Fault{Rally: n, EventID: g.counter, Kind: FaultBounce, Phase: after.Phase,
			Half: half, Effect: effectAt(half, standing)})
		g.points(n, bounced)
		after = bounced
	}
	return after, nil
}

// press reports a rally for the half its winner stands at. As a wrong_half
// fault it sends the other half first and, with Correct, takes it back with an
// undo, wherever that left the match: the serve rally decided, a set won, the
// match won.
func (g *Game) press(ctx context.Context, half scorer.Half, winner scorer.Player) (scorer.State, uint64, error) {
	if !g.roll(g.cfg.Faults.WrongHalf) {
		return g.report(ctx, half, true)
	}

	before := g.current()
	wrong := other(half)
	after, n, err := g.report(ctx, wrong, true)
	if err != nil {
		return after, n, err
	}
	f := Fault{Rally: n, EventID: g.counter, Kind: FaultWrongHalf, Phase: before.Phase, Half: wrong}

	if !g.cfg.Faults.Correct {
		loser := playerAt(1 - index(winner))
		if before.Phase == scorer.PhaseServeRally {
			g.mu.Lock()
			g.truth.ExplainedServe = loser
			g.mu.Unlock()
		} else {
			f.Effect[index(winner)]--
			f.Effect[index(loser)]++
		}
		g.injected(f)
		return after, n, nil
	}

	f.UndoneIn = after.Phase
	g.injected(f)
	undone, err := g.client.Undo(ctx, g.matchIDNow())
	switch {
	case errors.Is(err, ErrNothingToUndo):
		g.diverged(Divergence{Rally: n, What: "undo", Truth: summary(before), Zaehlwerk: "nothing to undo"})
	case err != nil:
		return undone, n, err
	default:
		g.setState(undone)
		if !same(before, undone) {
			g.diverged(Divergence{Rally: n, What: "undo", Truth: summary(before), Zaehlwerk: summary(undone)})
		}
		g.log.Info("wrong half taken back", "rally", n, "undone_in", after.Phase, "phase", undone.Phase)
	}
	return g.report(ctx, half, true)
}

// report sends one press for a half and keeps zaehlwerk's answer as the state
// the game stands on. changes says the press must change that state; it is
// false only for a press that is itself a fault zaehlwerk must ignore. The
// faults that can happen to any press happen here: the counter starting again
// before it, and the same press sent twice.
func (g *Game) report(ctx context.Context, half scorer.Half, changes bool) (scorer.State, uint64, error) {
	before := g.current()

	// A press that must change the state, so that a reset zaehlwerk discards
	// shows. And not before the first, which would be no reset at all.
	reset := changes && g.counter > 0 && g.roll(g.cfg.Faults.CounterReset)
	if reset {
		quiet := g.cfg.ResetQuiet
		if quiet <= 0 {
			quiet = resetQuiet
		}
		g.log.Info("quiet before a counter reset", "for", quiet.String())
		if err := sleep(ctx, quiet); err != nil {
			return before, g.rally, err
		}
		g.counter = resetEventID - 1
	}

	g.rally++
	g.counter++
	ev := scorer.ScoreEvent{
		MatchID: g.matchIDNow(),
		Source:  g.Source,
		Half:    half,
		Delta:   1,
		EventID: g.counter,
	}
	st, err := g.client.Ingest(ctx, ev)
	if err != nil {
		return st, g.rally, err
	}
	g.setState(st)

	if reset {
		g.injected(Fault{Rally: g.rally, EventID: g.counter, Kind: FaultCounterReset, Phase: before.Phase, Half: half})
		if same(before, st) {
			g.diverged(Divergence{Rally: g.rally, What: string(FaultCounterReset),
				Truth: "accepted", Zaehlwerk: "discarded"})
		}
	}

	// Not once the match is over: zaehlwerk refuses any press to a finished
	// match before it looks at the event id, so a resend there would end the
	// game rather than test the deduplication.
	if st.Phase != scorer.PhaseComplete && g.roll(g.cfg.Faults.Resend) {
		again, err := g.client.Ingest(ctx, ev)
		if err != nil {
			return again, g.rally, err
		}
		g.injected(Fault{Rally: g.rally, EventID: g.counter, Kind: FaultResend, Phase: before.Phase, Half: half})
		if !same(st, again) {
			g.diverged(Divergence{Rally: g.rally, What: string(FaultResend),
				Truth: "discarded", Zaehlwerk: "applied: " + summary(again)})
		}
		g.setState(again)
		st = again
	}
	return st, g.rally, nil
}

// roll draws whether a fault happens now. A share of zero draws nothing, so a
// fault that is off does not move the draws of the ones that are on.
func (g *Game) roll(share float64) bool {
	return share > 0 && g.faultRnd.Float64() < share
}

// injected records a fault and what it should do to zaehlwerk's points.
func (g *Game) injected(f Fault) {
	g.mu.Lock()
	g.faults = append(g.faults, f)
	g.truth.Explain(f.Effect)
	g.changed()
	g.mu.Unlock()
	g.log.Info("fault injected", "rally", f.Rally, "event_id", f.EventID, "fault", f.Kind,
		"phase", f.Phase, "half", f.Half, "effect", f.Effect, "undone_in", f.UndoneIn)
}

// points compares the truth with zaehlwerk's points after a press, and records
// a divergence where they part, or where how far they are apart changes.
//
// A divergence is a finding when the part no fault explains changed at that
// press. Once something nobody accounted for has scored, truth and zaehlwerk
// stay that far apart, and every fault after it moves them again; those moves
// are explained, and the finding is the one press where it began.
func (g *Game) points(n uint64, st scorer.State) {
	truth := g.truthNow()
	awarded := Awarded(st)
	gap := [2]int{awarded[0] - truth.Won[0], awarded[1] - truth.Won[1]}
	now := [2][2]int{gap, truth.Explained}

	g.mu.Lock()
	before := g.parted
	g.parted = now
	g.mu.Unlock()
	if now == before {
		return
	}

	unexplained := func(p [2][2]int) [2]int { return [2]int{p[0][0] - p[1][0], p[0][1] - p[1][1]} }
	d := Divergence{Rally: n, What: "points", Truth: score(truth.Won), Zaehlwerk: score(awarded),
		Expected: unexplained(now) == unexplained(before)}
	if truth.Explained != ([2]int{}) {
		d.Explained = score([2]int{truth.Won[0] + truth.Explained[0], truth.Won[1] + truth.Explained[1]})
	}
	g.diverged(d)
}

func (g *Game) walked(st scorer.State) {
	g.mu.Lock()
	g.changes = append(g.changes, EndsChange{Set: st.SetNumber, Points: st.Points})
	g.changed()
	g.mu.Unlock()
	g.log.Info("change of ends", "set", st.SetNumber, "points", st.Points,
		"left", st.Players[index(st.Ends[0])], "right", st.Players[index(st.Ends[1])])
}

func (g *Game) diverged(d Divergence) {
	g.mu.Lock()
	g.divergences = append(g.divergences, d)
	g.changed()
	g.mu.Unlock()
	if d.Expected {
		g.log.Info("truth and zaehlwerk disagree, as the faults explain", "rally", d.Rally,
			"what", d.What, "truth", d.Truth, "zaehlwerk", d.Zaehlwerk, "explained", d.Explained)
		return
	}
	g.log.Warn("truth and zaehlwerk disagree", "rally", d.Rally, "what", d.What,
		"truth", d.Truth, "zaehlwerk", d.Zaehlwerk, "explained", d.Explained)
}

func (g *Game) pause(ctx context.Context) error { return sleep(ctx, g.cfg.Pace) }

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func (g *Game) setState(st scorer.State) {
	g.mu.Lock()
	g.state = st
	g.changed()
	g.mu.Unlock()
}

// same says two answers describe the same match. A press zaehlwerk discarded
// or ignored is answered with the state as it was, and the answer carries no
// outcome, so this is how the game tells.
func same(a, b scorer.State) bool {
	return a.Points == b.Points && a.Sets == b.Sets && a.SetNumber == b.SetNumber &&
		a.Serving == b.Serving && a.Complete == b.Complete && a.Winner == b.Winner &&
		a.Phase == b.Phase && a.Ends == b.Ends &&
		slices.Equal(a.CompletedSets, b.CompletedSets) && slices.Equal(a.Waiting, b.Waiting)
}

// summary is a state as a divergence shows it.
func summary(st scorer.State) string {
	return fmt.Sprintf("%s in set %d, %s", score(st.Points), st.SetNumber, st.Phase)
}

// end ends the game's match on zaehlwerk with a context of its own, because
// the one the game played under is usually what just ended.
func (g *Game) end(ctx context.Context) {
	id := g.matchIDNow()
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	st, err := g.client.EndMatch(ctx, id)
	if err != nil {
		g.log.Warn("could not end the match", "match_id", id, "error", err)
		return
	}
	g.mu.Lock()
	g.state = st
	g.changed()
	g.mu.Unlock()
}

func (g *Game) finish(status Status, reason string) {
	g.mu.Lock()
	g.status, g.reason = status, reason
	g.changed()
	g.mu.Unlock()
	if status != StatusWon {
		g.log.Info("game over", "status", status, "reason", reason)
	}
}

// Done is closed once the game has stopped, however it stopped.
func (g *Game) Done() <-chan struct{} { return g.done }

// View is the game as it stands now.
func (g *Game) View() View {
	g.mu.Lock()
	defer g.mu.Unlock()
	var lastPress uint64
	if b, ok := g.driver.(*browser); ok {
		lastPress = b.last.Load()
	}
	unexplained := 0
	for _, d := range g.divergences {
		if !d.Expected {
			unexplained++
		}
	}
	return View{
		ID:          g.ID,
		Source:      g.Source,
		MatchID:     g.matchID,
		Status:      g.status,
		Reason:      g.reason,
		Seed:        g.cfg.Seed,
		Players:     g.cfg.Players,
		Driver:      g.cfg.Driver,
		Mode:        g.cfg.Mode,
		Bots:        g.cfg.Bots,
		LastPress:   lastPress,
		Skills:      g.cfg.Skills,
		Rallies:     g.rallies,
		Truth:       g.truth.Won,
		Zaehlwerk:   Awarded(g.state),
		Explained:   g.truth.Explained,
		Divergences: append([]Divergence{}, g.divergences...),
		Unexplained: unexplained,
		FaultShares: g.cfg.Faults,
		Faults:      append([]Fault{}, g.faults...),
		EndsChanges: append([]EndsChange{}, g.changes...),
		State:       g.state,
	}
}

func (g *Game) current() scorer.State {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

func (g *Game) truthNow() Truth {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.truth
}

func (g *Game) matchIDNow() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.matchID
}
