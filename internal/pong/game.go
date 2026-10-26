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
	// StatusChecked is a button check that asked for everything it set out
	// to; its findings say how the buttons answered.
	StatusChecked Status = "checked"
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
	// Driver is who decides the rallies: "bots" headless, "browser",
	// "server" for two bots the service plays on a court (ADR-0014), or
	// "check" for the buttons at the table.
	Driver string `json:"driver"`
	// Attract is a demo started by attract mode rather than by anybody.
	Attract bool `json:"attract,omitempty"`
	// Screensaver is a demo started by the screensaver page (ADR-0013).
	Screensaver bool `json:"screensaver,omitempty"`
	// Mode and Bots are a browser game's: "pvp", "pvc" or "cvc", and the bot
	// playing as player a and b, or none for a person.
	Mode string        `json:"mode,omitempty"`
	Bots [2]*BotParams `json:"bots,omitempty"`
	// Diagonal is a browser game served as in doubles: from the server's
	// right half court into the receiver's (web/serve.js).
	Diagonal bool `json:"diagonal,omitempty"`
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
	// Check is a button check's prompts and findings.
	Check *CheckView `json:"check,omitempty"`
	// Remote is a game on two devices: whether the guest has joined, and
	// whether the code still joins. Never the code itself.
	Remote *RemoteView `json:"remote,omitempty"`
}

// GameConfig is one match: headless between two bots, or played in a
// browser.
type GameConfig struct {
	BestOf int
	// PointsPerSet is what a set is played to; zero is zaehlwerk's default.
	// Only a tournament match sets it, to its tournament's (ADR-0017). The
	// game itself never applies it: it counts rallies, and the sets are
	// zaehlwerk's (see Truth).
	PointsPerSet int
	// Players are the names on the panel; BotNames when empty.
	Players [2]string
	// Driver is DriverBots, DriverBrowser, DriverServer or DriverCheck;
	// empty is bots.
	Driver string
	// Mode and Bots are what a browser game shows and plays with; the game
	// itself does not use them.
	Mode string
	Bots [2]*BotParams
	// Diagonal has the serve go corner to corner, as in doubles. The page
	// judges it, as it judges every rally; the service only passes it on.
	Diagonal bool
	// Idle ends a browser game nobody presses in; zero is browserIdle.
	Idle time.Duration
	// CheckTimeout is how long a button check's prompt waits for its press,
	// and CheckGap the pause between two prompts; zero is the default.
	CheckTimeout time.Duration
	CheckGap     time.Duration
	// Attract is a demo the service started by itself on a free table. It
	// gives the table to any game somebody asks for.
	Attract bool
	// Screensaver is a demo a screen at the table started by itself
	// (ADR-0013): a browser game between two bots that gives way as attract
	// mode's does.
	Screensaver bool
	Skills      [2]Skill
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
	// Tick paces a served game's court; zero runs it as fast as it goes,
	// which only the tests want.
	Tick time.Duration
	// Handover makes the game a league match (ADR-0015): a real match
	// between two Schmetterpause players, reported by zaehlwerk when it is
	// won. Nil for every other game, which is a demo and reported nowhere.
	Handover *Handover
}

// demo is a game nobody asked for: attract mode's or the screensaver's. It
// gives the table to a game somebody asks for and to anybody counting into it
// (ADR-0008, ADR-0013).
func (c GameConfig) demo() bool { return c.Attract || c.Screensaver }

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
	// check is a button check's record, nil for every other game. Guarded
	// by mu.
	check *checkRun
	// remote is a game on two devices' code and guest (ADR-0009), nil for
	// every other game. Set before the game is shared, and guarded by its
	// own mutex.
	remote *remote
	// relay carries the table to the pages that draw it: from the host of a
	// game on two devices, or from the court of a served game (ADR-0014).
	// Nil for every other game.
	relay *relayBox

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
	var check *checkRun
	var relay *relayBox
	if cfg.Mode == ModeRemote {
		relay = newRelayBox()
	}
	switch cfg.Driver {
	case DriverBrowser:
		driver = newBrowser(cfg.Idle)
	case DriverServer:
		relay = newRelayBox()
		driver = newServed(cfg.Seed, cfg.Bots, relay, cfg.Tick)
	case DriverCheck:
		// Nobody decides a rally in a check: it says what to press and
		// listens. The driver is there for Over, which a check has nothing
		// to answer.
		driver = noDriver{}
		check = &checkRun{started: time.Now(), counts: map[CheckOutcome]int{}}
	}
	return &Game{
		check:       check,
		relay:       relay,
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

// touch wakes every stream for a change the game's own fields do not hold: a
// guest joining, a code locked or drawn again.
func (g *Game) touch() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.changed()
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
	var st scorer.State
	var err error
	if g.cfg.Handover != nil {
		st, err = g.client.CreateLeagueMatch(ctx, g.cfg.Players, g.cfg.BestOf, g.cfg.PointsPerSet, *g.cfg.Handover)
	} else {
		st, err = g.client.CreateMatch(ctx, g.cfg.Players, g.cfg.BestOf)
	}
	if err != nil {
		return fmt.Errorf("starting the match: %w", err)
	}
	g.mu.Lock()
	g.matchID, g.state = st.MatchID, st
	g.changed()
	g.mu.Unlock()
	g.log.Info("match started", "match_id", st.MatchID, "players", st.Players, "driver", g.cfg.Driver,
		"best_of", g.cfg.BestOf, "skills", []string{g.cfg.Skills[0].Name, g.cfg.Skills[1].Name},
		"seed", g.cfg.Seed, "league", g.cfg.Handover != nil)
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

// errMatchEnded is zaehlwerk ending the game's match while it plays: on the
// scoring page, or through POST /matches/{id}/end. Without it a browser game
// would play on until PONG_BROWSER_IDLE, because nothing it reports is refused
// while nobody presses.
var errMatchEnded = errors.New("zaehlwerk ended the match")

// errStopped is a browser game ended by its own page.
var errStopped = errors.New("ended in the browser")

// errHeld is a league match ended because league mode was stopped from the
// lobby.
var errHeld = errors.New("league mode was stopped in the lobby")

// run plays the match to its end and watches the table while it does. table
// is /live/stream, already past the event that said the table was free.
// stopped is the game's own context, which g.stop cancels with the reason the
// game is being stopped for, and which table is read under.
func (g *Game) run(stopped context.Context, table <-chan TableEvent) {
	defer close(g.done)
	// After the status is set and before done closes, so a press waiting
	// for its answer gets the last state and finds the game over.
	defer func() { g.driver.Over(g.current()) }()

	ctx, cancel := context.WithCancelCause(stopped)
	defer cancel(nil)
	var err error
	if g.cfg.Driver == DriverCheck {
		err = g.runCheck(ctx, table)
	} else {
		go g.watch(ctx, stopped, cancel, table)
		err = g.play(ctx)
	}
	err = endCause(err, stopped, ctx)

	switch {
	case err == nil && g.cfg.Driver == DriverCheck:
		// Done asking, not a match played out: it is ended, like any match
		// the game does not finish.
		g.end(ctx)
		g.finish(StatusChecked, "")
		return
	case err == nil:
		g.finish(StatusWon, "")
		return
	case errors.Is(err, ErrMatchOver):
		g.finish(StatusEnded, "zaehlwerk says the match is over")
		return
	case errors.Is(err, errMatchEnded):
		// Already ended, so there is nothing to end.
		g.finish(StatusEnded, err.Error())
		return
	case errors.Is(err, errDisplaced), errors.Is(err, errTableLost), errors.Is(err, errStopped),
		errors.Is(err, errHeld), errors.Is(err, errIdle), errors.Is(err, errYielded), errors.Is(err, errIntruded),
		errors.Is(err, errNobodyPresses), ctx.Err() != nil:
		// The match is no longer the game's to play, the game can no longer
		// tell whether it is, nobody is playing it any more, a demo made way
		// for a game or for somebody counting into it, or the service is
		// stopping. Either way it is ended rather than left running, so it
		// does not sit in the registry as current with nobody scoring it.
		reason := "the service stopped"
		for _, known := range []error{errDisplaced, errTableLost, errStopped, errHeld, errIdle, errYielded, errIntruded,
			errNobodyPresses} {
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

// endCause is why the game ended. Whoever stopped the game said why, and that
// reason wins over anything the game saw as a consequence: stopping closes the
// table's stream too, and a game that read the close first would otherwise end
// as having lost sight of the table. It can read it first, because a context
// closes its own Done before it cancels its children: the stream, read under
// stopped, can be gone while ctx still says nothing happened (#141). After
// that, the cause the game's own context carries, as before.
func endCause(err error, stopped, ctx context.Context) error {
	if err == nil {
		return nil
	}
	for _, c := range []context.Context{stopped, ctx} {
		if cause := context.Cause(c); cause != nil && !errors.Is(cause, context.Canceled) {
			return cause
		}
	}
	return err
}

// watch reads the table while the game plays. A snapshot of another match
// means that match is now current: somebody started it, and it is theirs. Its
// own match ending on zaehlwerk ends the game, and a stream that closes leaves
// the game blind to both, so it ends the game too.
//
// The end of its own match is a match_ended with its id, and zaehlwerk follows
// that at once with what is current next: no_match, or the snapshot of the
// real match that ended a demo. watch waits for that next event, so a demo
// made way for a real match ends displaced, as it always has. A won match
// sends no match_ended but also a no_match after its last point; watch stops
// at the point, which play has as its answer, so a won game stays won.
//
// stopped is the game's own context, which the stream is read under: a stream
// that closes because the game is being stopped is not one that got lost.
func (g *Game) watch(ctx, stopped context.Context, cancel context.CancelCauseFunc, table <-chan TableEvent) {
	ended := false
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-table:
			switch {
			case !ok:
				// Also closed when ctx ends. Only the first cause counts, and
				// a game that won or is stopping is not one that got lost.
				// stopped is asked too: it is done before ctx is (endCause).
				if ctx.Err() == nil && stopped.Err() == nil {
					if ended {
						cancel(errMatchEnded)
						return
					}
					g.log.Warn("/live/stream closed, ending our match")
					cancel(errTableLost)
				}
				return
			case ev.Kind == kindNoMatch:
				g.log.Info("zaehlwerk ended our match", "match_id", g.matchIDNow())
				cancel(errMatchEnded)
				return
			case ev.State == nil:
			case ev.Kind == kindSnapshot && ev.State.MatchID != g.matchIDNow():
				g.log.Info("another match is current, ending ours",
					"other", ev.State.MatchID, "reported", ev.Reported())
				cancel(errDisplaced)
				return
			case ev.State.MatchID != g.matchIDNow():
			case ev.State.Complete:
				return
			case ev.Kind == kindMatchEnded:
				ended = true
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
	if g.cfg.demo() {
		// Any divergence, not only an unexplained one: a demo has no faults
		// to explain it. Only a demo gives way like this; a game somebody
		// plays reports its divergences, which is what it is for.
		g.log.Warn("truth and zaehlwerk disagree in a demo, giving way", "rally", d.Rally,
			"what", d.What, "truth", d.Truth, "zaehlwerk", d.Zaehlwerk)
		g.stop(errIntruded)
		return
	}
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
		Attract:     g.cfg.Attract,
		Screensaver: g.cfg.Screensaver,
		Mode:        g.cfg.Mode,
		Bots:        g.cfg.Bots,
		Diagonal:    g.cfg.Diagonal,
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
		Check:       g.checkView(),
		Remote:      g.remoteView(),
	}
}

func (g *Game) remoteView() *RemoteView {
	if g.remote == nil {
		return nil
	}
	return g.remote.view()
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
