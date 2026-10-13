package pong

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
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
	// displaced by another match, or the service shutting down.
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
	Skills  [2]Skill  `json:"skills"`
	// Rallies is the rallies played for points; the serve rally is not one.
	Rallies     int          `json:"rallies"`
	Truth       [2]int       `json:"truth"`
	Zaehlwerk   [2]int       `json:"zaehlwerk"`
	Divergences []Divergence `json:"divergences"`
	EndsChanges []EndsChange `json:"ends_changes"`
	State       scorer.State `json:"state"`
}

// GameConfig is one headless bot match.
type GameConfig struct {
	BestOf int
	Skills [2]Skill
	// Pace is the pause between two reports: zero for a test, a second or so
	// for somebody watching the panel.
	Pace time.Duration
	Seed int64
}

// Game is one headless bot match: two bots, rallies drawn by skill, every
// result reported to zaehlwerk and checked against the truth.
type Game struct {
	ID     string
	Source string

	cfg    GameConfig
	client *Client
	log    *slog.Logger
	rnd    *rand.Rand
	done   chan struct{}

	mu          sync.Mutex
	matchID     string
	status      Status
	reason      string
	state       scorer.State
	truth       Truth
	rallies     int
	divergences []Divergence
	changes     []EndsChange

	// seq is the game's counter: every report takes the next number, so a
	// rally's number and its event id are the same thing (ADR-0006). Only the
	// goroutine that plays touches it.
	seq uint64
}

func newGame(id string, cfg GameConfig, client *Client, log *slog.Logger) *Game {
	source := "pong-" + id
	return &Game{
		ID:          id,
		Source:      source,
		cfg:         cfg,
		client:      client,
		log:         log.With("game", id, "source", source),
		rnd:         rand.New(rand.NewSource(cfg.Seed)),
		done:        make(chan struct{}),
		status:      StatusPlaying,
		divergences: []Divergence{},
		changes:     []EndsChange{},
	}
}

// start creates the match. It is separate from play so that a match zaehlwerk
// refuses to create is the caller's error, not a game that failed later.
func (g *Game) start(ctx context.Context) error {
	st, err := g.client.CreateMatch(ctx, BotNames, g.cfg.BestOf)
	if err != nil {
		return fmt.Errorf("starting the match: %w", err)
	}
	g.mu.Lock()
	g.matchID, g.state = st.MatchID, st
	g.mu.Unlock()
	g.log.Info("match started", "match_id", st.MatchID, "players", st.Players,
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

// run plays the match to its end and watches the table while it does. table
// is /live/stream, already past the event that said the table was free.
func (g *Game) run(ctx context.Context, table <-chan TableEvent) {
	defer close(g.done)

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
	case errors.Is(err, errDisplaced), errors.Is(err, errTableLost), ctx.Err() != nil:
		// The match is no longer the game's to play, the game can no longer
		// tell whether it is, or the service is stopping. Either way it is
		// ended rather than left running, so it does not sit in the registry
		// as current with nobody scoring it.
		reason := "the service stopped"
		switch {
		case errors.Is(err, errDisplaced):
			reason = errDisplaced.Error()
		case errors.Is(err, errTableLost):
			reason = errTableLost.Error()
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
// where the match stands is read from zaehlwerk's last answer; the game only
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
		if g.seq >= maxReports {
			return fmt.Errorf("%d reports and the match has not ended, stuck in %s", g.seq, st.Phase)
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
			st, err = g.serveRally(ctx, standing)
		case scorer.PhasePlaying:
			st, err = g.point(ctx, st, standing)
		default:
			return fmt.Errorf("zaehlwerk is in a phase the game does not know: %q", st.Phase)
		}
		if err != nil {
			return err
		}
	}

	g.log.Info("match won", "match_id", st.MatchID,
		"winner", st.Players[index(st.Winner)], "sets", st.Sets,
		"completed_sets", st.CompletedSets, "rallies", g.View().Rallies,
		"divergences", len(g.View().Divergences))
	return nil
}

// ready presses for the first half zaehlwerk is still waiting for: a player
// ready after the warm-up, or arrived at the new end.
func (g *Game) ready(ctx context.Context, st scorer.State) (scorer.State, error) {
	if len(st.Waiting) == 0 {
		return st, fmt.Errorf("zaehlwerk is in %s and waits for nobody", st.Phase)
	}
	half := st.Waiting[0]
	after, id, err := g.report(ctx, half)
	if err != nil {
		return after, err
	}
	g.log.Info("ready", "event_id", id, "phase", st.Phase, "half", half,
		"next", after.Phase, "waiting", after.Waiting)
	return after, nil
}

// serveRally plays for the first serve. Nobody serves it, so who hits first is
// drawn too. Its winner's half is sent once, and zaehlwerk decides from it who
// serves; the game checks that it decided for the right player.
func (g *Game) serveRally(ctx context.Context, standing [2]scorer.Player) (scorer.State, error) {
	winner := playerAt(Rally(g.rnd, g.cfg.Skills, g.rnd.Intn(2)))
	g.mu.Lock()
	g.truth.ServeRally = winner
	g.mu.Unlock()

	half := halfOf(winner, standing)
	after, id, err := g.report(ctx, half)
	if err != nil {
		return after, err
	}
	g.log.Info("serve rally", "event_id", id, "winner", after.Players[index(winner)],
		"half", half, "serving", after.Serving)
	if d, off := g.truthNow().Serve(id, after); off {
		g.diverged(d)
	}
	return after, nil
}

// point plays one rally for a point, served by whoever zaehlwerk says serves,
// and reports it for the half its winner stands at.
func (g *Game) point(ctx context.Context, st scorer.State, standing [2]scorer.Player) (scorer.State, error) {
	if d, off := Ends(g.seq+1, standing, st); off {
		g.diverged(d)
	}

	winner := playerAt(Rally(g.rnd, g.cfg.Skills, index(st.Serving)))
	g.mu.Lock()
	g.truth.Rally(winner)
	g.rallies++
	g.mu.Unlock()

	half := halfOf(winner, standing)
	after, id, err := g.report(ctx, half)
	if err != nil {
		return after, err
	}
	truth := g.truthNow()
	g.log.Info("rally", "event_id", id, "server", st.Players[index(st.Serving)],
		"winner", st.Players[index(winner)], "half", half,
		"truth", score(truth.Won), "zaehlwerk", score(Awarded(after)),
		"points", after.Points, "sets", after.Sets, "phase", after.Phase)
	if d, off := truth.Points(id, after); off {
		g.diverged(d)
	}
	return after, nil
}

// report sends one press for a half with the game's next event id, and keeps
// zaehlwerk's answer as the state the game stands on.
func (g *Game) report(ctx context.Context, half scorer.Half) (scorer.State, uint64, error) {
	g.seq++
	st, err := g.client.Ingest(ctx, scorer.ScoreEvent{
		MatchID: g.matchIDNow(),
		Source:  g.Source,
		Half:    half,
		Delta:   1,
		EventID: g.seq,
	})
	if err != nil {
		return st, g.seq, err
	}
	g.mu.Lock()
	g.state = st
	g.mu.Unlock()
	return st, g.seq, nil
}

func (g *Game) walked(st scorer.State) {
	g.mu.Lock()
	g.changes = append(g.changes, EndsChange{Set: st.SetNumber, Points: st.Points})
	g.mu.Unlock()
	g.log.Info("change of ends", "set", st.SetNumber, "points", st.Points,
		"left", st.Players[index(st.Ends[0])], "right", st.Players[index(st.Ends[1])])
}

func (g *Game) diverged(d Divergence) {
	g.mu.Lock()
	g.divergences = append(g.divergences, d)
	g.mu.Unlock()
	g.log.Warn("truth and zaehlwerk disagree", "event_id", d.EventID, "what", d.What,
		"truth", d.Truth, "zaehlwerk", d.Zaehlwerk)
}

func (g *Game) pause(ctx context.Context) error {
	if g.cfg.Pace <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(g.cfg.Pace):
		return nil
	}
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
	g.mu.Unlock()
}

func (g *Game) finish(status Status, reason string) {
	g.mu.Lock()
	g.status, g.reason = status, reason
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
	return View{
		ID:          g.ID,
		Source:      g.Source,
		MatchID:     g.matchID,
		Status:      g.status,
		Reason:      g.reason,
		Seed:        g.cfg.Seed,
		Players:     BotNames,
		Skills:      g.cfg.Skills,
		Rallies:     g.rallies,
		Truth:       g.truth.Won,
		Zaehlwerk:   Awarded(g.state),
		Divergences: append([]Divergence{}, g.divergences...),
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
