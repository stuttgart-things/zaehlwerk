package panel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	homerun "github.com/stuttgart-things/homerun-library/v4"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recorder is a pitcher that remembers what it was asked to publish.
type recorder struct {
	mu      sync.Mutex
	msgs    []homerun.Message
	streams []string
	closed  int
	err     error         // returned by Enqueue when set
	block   chan struct{} // Enqueue waits on this when set
	arrived chan struct{} // signalled on every Enqueue entry
}

func newRecorder() *recorder {
	return &recorder{arrived: make(chan struct{}, 1024)}
}

func (r *recorder) Enqueue(ctx context.Context, msg homerun.Message, streamOverride ...string) (string, string, error) {
	select {
	case r.arrived <- struct{}{}:
	default:
	}

	if r.block != nil {
		select {
		case <-r.block:
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
	if r.err != nil {
		return "", "", r.err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, msg)
	if len(streamOverride) > 0 {
		r.streams = append(r.streams, streamOverride[0])
	}
	return "obj", "stream", nil
}

func (r *recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed++
	return nil
}

func (r *recorder) titles() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.msgs))
	for i, m := range r.msgs {
		out[i] = m.Title
	}
	return out
}

func (r *recorder) closeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// waitFor polls until cond holds or the test times out, so the tests do not
// depend on a sleep long enough to be slow and short enough to be flaky.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// newScorer builds a real scorer, because the property under test — that the
// sink does not block the match — is a property of the two together. The sink's
// Observe runs inside the scorer's lock, and a test that called Observe
// directly would not exercise that at all.
func newScorer(t *testing.T, s *Sink) *scorer.Scorer {
	t.Helper()
	sc, err := scorer.New(scorer.Config{MatchID: "m1", Players: [2]string{"Anna", "Bernd"}})
	require.NoError(t, err)
	sc.Observe(func(tr scorer.Transition) { s.Observe(nil, tr) })
	return sc
}

func point(t *testing.T, sc *scorer.Scorer, p scorer.Player, id uint64) {
	t.Helper()
	_, err := sc.Apply(scorer.ScoreEvent{MatchID: "m1", Player: p, Delta: 1, EventID: id, Source: "test"})
	require.NoError(t, err)
}

func TestEveryTransitionReachesThePanel(t *testing.T) {
	rec := newRecorder()
	sink := New(rec, Config{Logger: quiet()})
	defer func() { require.NoError(t, sink.Close()) }()

	sc := newScorer(t, sink)
	for i := range uint64(3) {
		point(t, sc, scorer.PlayerA, i+1)
	}
	_, err := sc.Undo()
	require.NoError(t, err)

	waitFor(t, "four transitions", func() bool { return len(rec.titles()) == 4 })
	require.Equal(t, []string{"1:0", "2:0", "3:0", "2:0"}, rec.titles())
}

// A panel that gets 4:5 before 3:5 shows a score that never happened. The
// scorer emits under its lock, so order is guaranteed on the way in; this
// pins that the sink keeps it on the way out.
func TestTransitionsArriveInTheOrderTheyHappened(t *testing.T) {
	rec := newRecorder()
	sink := New(rec, Config{Logger: quiet()})
	defer func() { require.NoError(t, sink.Close()) }()

	sc := newScorer(t, sink)

	// A second observer is the oracle: it is told what happened by the scorer
	// rather than by this test reconstructing it. The first version of this
	// test built the expectation itself, assumed every transition was a point,
	// and was wrong the moment the twenty points ran into a set.
	var mu sync.Mutex
	var want []string
	sc.Observe(func(tr scorer.Transition) {
		mu.Lock()
		defer mu.Unlock()
		want = append(want, Title(tr))
	})

	sawSet := false
	for i := range uint64(20) {
		p := scorer.PlayerA
		if i%3 == 0 {
			p = scorer.PlayerB
		}
		point(t, sc, p, i+1)
		if len(sc.State().CompletedSets) > 0 {
			sawSet = true
		}
	}
	require.True(t, sawSet, "the run must cross a set boundary, or it never tests the interesting case")

	mu.Lock()
	defer mu.Unlock()

	waitFor(t, "all points", func() bool { return len(rec.titles()) == len(want) })
	require.Equal(t, want, rec.titles())
}

func TestThePitchGoesToTheConfiguredStream(t *testing.T) {
	rec := newRecorder()
	sink := New(rec, Config{Stream: "tabletennis", Logger: quiet()})
	defer func() { require.NoError(t, sink.Close()) }()

	point(t, newScorer(t, sink), scorer.PlayerA, 1)

	waitFor(t, "the pitch", func() bool { return len(rec.titles()) == 1 })
	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Equal(t, []string{"tabletennis"}, rec.streams)
}

// The failure behaviour the issue asks for: Redis unreachable must not stall or
// crash the match. A hung pitcher is the harsher case of the two — an error
// returns, a hang does not.
func TestAHungPitcherDoesNotStallTheMatch(t *testing.T) {
	rec := newRecorder()
	rec.block = make(chan struct{})
	defer close(rec.block)

	sink := New(rec, Config{QueueSize: 2, PitchTimeout: time.Hour, DrainTimeout: time.Millisecond, Logger: quiet()})
	sc := newScorer(t, sink)

	// Wait until the worker is actually stuck inside Enqueue, so the queue is
	// the only thing left absorbing points.
	point(t, sc, scorer.PlayerA, 1)
	<-rec.arrived

	// Far more points than the queue holds. Every one of them must return
	// immediately; the panel is allowed to lose them, the match is not.
	start := time.Now()
	for i := range uint64(200) {
		p := scorer.PlayerA
		if i%2 == 0 {
			p = scorer.PlayerB
		}
		point(t, sc, p, i+2)
	}
	elapsed := time.Since(start)

	require.Less(t, elapsed, time.Second, "applying points took %s with the panel hung", elapsed)
	require.Positive(t, sink.Dropped(), "the queue should have overflowed and dropped")

	// And the match itself is intact: 201 points went in, 201 are on the score.
	st := sc.State()
	require.Equal(t, 201, st.Points[0]+st.Points[1]+setPoints(st))
}

// setPoints counts the points banked in sets already completed, so a match that
// rolled over a set boundary still adds up.
func setPoints(st scorer.State) int {
	total := 0
	for _, s := range st.CompletedSets {
		total += s[0] + s[1]
	}
	return total
}

func TestAFailingPitchIsCountedAndTheMatchCarriesOn(t *testing.T) {
	rec := newRecorder()
	rec.err = errors.New("dial tcp: connection refused")

	sink := New(rec, Config{Logger: quiet()})
	sc := newScorer(t, sink)

	for i := range uint64(5) {
		point(t, sc, scorer.PlayerA, i+1)
	}

	waitFor(t, "five failed pitches", func() bool { return sink.Failed() == 5 })
	require.Zero(t, sink.Dropped())
	require.Equal(t, 5, sc.State().Points[0])
	require.NoError(t, sink.Close())
}

// The last transition of a match is the final score. Losing it on shutdown
// would leave the panel showing the second-to-last point of the match forever.
func TestCloseDeliversWhatIsStillQueued(t *testing.T) {
	rec := newRecorder()
	rec.block = make(chan struct{})

	sink := New(rec, Config{QueueSize: 16, Logger: quiet()})
	sc := newScorer(t, sink)

	for i := range uint64(4) {
		point(t, sc, scorer.PlayerA, i+1)
	}
	<-rec.arrived // the worker is inside the first Enqueue
	close(rec.block)

	require.NoError(t, sink.Close())
	require.Equal(t, []string{"1:0", "2:0", "3:0", "4:0"}, rec.titles())
	require.Zero(t, sink.Dropped())
}

// A drain that waited on a Redis which has stopped answering would hold
// shutdown open past the process's own timeout.
func TestCloseGivesUpOnADrainThatWillNotFinish(t *testing.T) {
	rec := newRecorder()
	rec.block = make(chan struct{})
	defer close(rec.block)

	sink := New(rec, Config{QueueSize: 16, PitchTimeout: 20 * time.Millisecond, DrainTimeout: 50 * time.Millisecond, Logger: quiet()})
	sc := newScorer(t, sink)
	for i := range uint64(8) {
		point(t, sc, scorer.PlayerA, i+1)
	}

	start := time.Now()
	require.NoError(t, sink.Close())
	require.Less(t, time.Since(start), 2*time.Second, "Close hung on an unresponsive pitcher")
}

func TestCloseClosesThePitcherExactlyOnce(t *testing.T) {
	rec := newRecorder()
	sink := New(rec, Config{Logger: quiet()})

	require.NoError(t, sink.Close())
	require.NoError(t, sink.Close())
	require.NoError(t, sink.Close())
	require.Equal(t, 1, rec.closeCount())
}

// A transition arriving after Close must be dropped, not panic on a closed
// channel and not wake a worker that has gone.
func TestObserveAfterCloseIsHarmless(t *testing.T) {
	rec := newRecorder()
	sink := New(rec, Config{Logger: quiet()})
	sc := newScorer(t, sink)

	require.NoError(t, sink.Close())

	require.NotPanics(t, func() {
		for i := range uint64(10) {
			point(t, sc, scorer.PlayerA, i+1)
		}
	})
	require.Equal(t, 10, sc.State().Points[0])

	// And they are reported as undelivered rather than silently absorbed: the
	// first version of this test only asserted the absence of a panic, which
	// held just as well with the post-close check removed entirely.
	require.Equal(t, uint64(10), sink.Dropped())
	require.Empty(t, rec.titles(), "nothing should have been published after Close")
}

// Close racing with a scorer that is still emitting is the shutdown case: the
// HTTP server is closing while a last point is in flight. Run with -race.
func TestCloseRacingWithAScorerStillPlaying(t *testing.T) {
	rec := newRecorder()
	sink := New(rec, Config{QueueSize: 4, Logger: quiet()})
	sc := newScorer(t, sink)

	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range uint64(50) {
				_, _ = sc.Apply(scorer.ScoreEvent{
					MatchID: "m1", Player: scorer.PlayerA, Delta: 1,
					EventID: i + 1, Source: string(rune('a' + w)),
				})
			}
		}()
	}

	time.Sleep(time.Millisecond)
	require.NoError(t, sink.Close())
	wg.Wait()
}

func TestConfigDefaultsAreApplied(t *testing.T) {
	cfg := Config{}.withDefaults()

	require.Equal(t, DefaultStream, cfg.Stream)
	require.Equal(t, DefaultSystem, cfg.System)
	require.Equal(t, DefaultAuthor, cfg.Author)
	require.Equal(t, DefaultQueueSize, cfg.QueueSize)
	require.Equal(t, DefaultPitchTimeout, cfg.PitchTimeout)
	require.Equal(t, DefaultDrainTimeout, cfg.DrainTimeout)
	require.NotNil(t, cfg.Logger)
	require.NotNil(t, cfg.Now)
}

func TestConfigKeepsWhatWasSet(t *testing.T) {
	cfg := Config{
		Stream: "other", System: "sys", Author: "me",
		QueueSize: 7, PitchTimeout: time.Second, DrainTimeout: 2 * time.Second,
	}.withDefaults()

	require.Equal(t, "other", cfg.Stream)
	require.Equal(t, "sys", cfg.System)
	require.Equal(t, "me", cfg.Author)
	require.Equal(t, 7, cfg.QueueSize)
	require.Equal(t, time.Second, cfg.PitchTimeout)
	require.Equal(t, 2*time.Second, cfg.DrainTimeout)
}

// The sink is a match observer so that it can tell a demo by its match
// (ADR-0008); the registry hands the match over, and the sink never looks it
// up.
func TestThePanelTagsADemo(t *testing.T) {
	rec := newRecorder()
	sink := New(rec, Config{Logger: quiet()})
	defer func() { require.NoError(t, sink.Close()) }()
	registry := match.NewRegistry(match.WithMatchObserver(sink.Observe))

	demo, _, err := registry.CreateFor(scorer.Config{Players: [2]string{"Pong A", "Pong B"}}, match.Setup{Demo: true})
	require.NoError(t, err)
	_, err = demo.Scorer.Apply(scorer.ScoreEvent{MatchID: demo.ID, Player: scorer.PlayerA, Delta: 1, EventID: 1, Source: "test"})
	require.NoError(t, err)

	real, err := registry.Create(scorer.Config{Players: [2]string{"Anna", "Bernd"}})
	require.NoError(t, err)
	_, err = real.Scorer.Apply(scorer.ScoreEvent{MatchID: real.ID, Player: scorer.PlayerA, Delta: 1, EventID: 1, Source: "test"})
	require.NoError(t, err)

	waitFor(t, "both points", func() bool { return len(rec.titles()) == 2 })
	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.True(t, strings.HasSuffix(rec.msgs[0].Tags, ",demo=1"), rec.msgs[0].Tags)
	require.NotContains(t, rec.msgs[1].Tags, "demo", rec.msgs[1].Tags)
}

// pointsFor scores n points for p in m, counting event ids from first.
func pointsFor(t *testing.T, m *match.Match, p scorer.Player, n int, first uint64) {
	t.Helper()
	for i := range n {
		_, err := m.Scorer.Apply(scorer.ScoreEvent{MatchID: m.ID, Player: p, Delta: 1, EventID: first + uint64(i), Source: "test"})
		require.NoError(t, err)
	}
}

// lastTags waits for n messages and returns the tags of the last one.
func lastTags(t *testing.T, rec *recorder, n int) string {
	t.Helper()
	waitFor(t, "the points", func() bool { return len(rec.titles()) == n })
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.msgs[n-1].Tags
}

// A light pulses on the side that holds a set or match point, so the tag says
// what the next point decides and for whom.
func TestThePanelSaysWhatTheNextPointDecides(t *testing.T) {
	rec := newRecorder()
	sink := New(rec, Config{Logger: quiet()})
	defer func() { require.NoError(t, sink.Close()) }()
	registry := match.NewRegistry(match.WithMatchObserver(sink.Observe))

	m, err := registry.Create(scorer.Config{BestOf: 3, PointsPerSet: 11, Players: [2]string{"Anna", "Bernd"}})
	require.NoError(t, err)

	pointsFor(t, m, scorer.PlayerB, 9, 1)
	require.NotContains(t, lastTags(t, rec, 9), "stake", "0:9 decides nothing yet")

	pointsFor(t, m, scorer.PlayerB, 1, 10)
	require.Contains(t, lastTags(t, rec, 10), ",stake=set_point,stake_side=b")

	pointsFor(t, m, scorer.PlayerB, 1, 11) // the set
	pointsFor(t, m, scorer.PlayerB, 10, 12)
	require.Contains(t, lastTags(t, rec, 21), ",stake=match_point,stake_side=b")

	pointsFor(t, m, scorer.PlayerB, 1, 22) // the match
	require.NotContains(t, lastTags(t, rec, 22), "stake", "a won match has nothing left to decide")
}

func TestATournamentMatchIsTaggedWithItsPlace(t *testing.T) {
	rec := newRecorder()
	sink := New(rec, Config{Logger: quiet()})
	defer func() { require.NoError(t, sink.Close()) }()
	registry := match.NewRegistry(match.WithMatchObserver(sink.Observe))

	group, _, err := registry.CreateFor(scorer.Config{Players: [2]string{"Anna", "Bernd"}},
		match.Setup{Handover: match.Handover{HomeID: "h", AwayID: "a", OperatorID: "o", TournamentID: "t", Round: 3}})
	require.NoError(t, err)
	pointsFor(t, group, scorer.PlayerA, 1, 1)
	require.True(t, strings.HasSuffix(lastTags(t, rec, 1), ",tournament=1,round=3"), lastTags(t, rec, 1))

	// A table of its own: one registry runs one match at a time.
	other := match.NewRegistry(match.WithMatchObserver(sink.Observe))
	final, _, err := other.CreateFor(scorer.Config{Players: [2]string{"Anna", "Bernd"}},
		match.Setup{Handover: match.Handover{HomeID: "h", AwayID: "a", OperatorID: "o", TournamentID: "t", Round: 4, Final: true}})
	require.NoError(t, err)
	pointsFor(t, final, scorer.PlayerA, 1, 1)
	require.True(t, strings.HasSuffix(lastTags(t, rec, 2), ",tournament=1,round=4,final=1"), lastTags(t, rec, 2))
}

func TestAnOrdinaryMatchHasNoMatchTags(t *testing.T) {
	require.Empty(t, MatchTags(Match{}))
}

// A won set or match names the winner's figure, which the catcher plays before
// it holds the board (Schmetterpause ADR-0030). A point does not.
func TestAWonSetNamesTheWinnersFigure(t *testing.T) {
	rec := newRecorder()
	sink := New(rec, Config{Figures: "https://sp.example/", Logger: quiet()})
	defer func() { require.NoError(t, sink.Close()) }()
	registry := match.NewRegistry(match.WithMatchObserver(sink.Observe))

	m, _, err := registry.CreateFor(scorer.Config{BestOf: 3, PointsPerSet: 11, Players: [2]string{"Anna", "Bernd"}},
		match.Setup{Handover: match.Handover{HomeID: "id-anna", AwayID: "id-bernd", OperatorID: "o"}})
	require.NoError(t, err)

	pointsFor(t, m, scorer.PlayerB, 10, 1)
	require.NotContains(t, lastTags(t, rec, 10), "figure=", "a point is not a moment for a figure")

	pointsFor(t, m, scorer.PlayerB, 1, 11) // the set
	require.True(t, strings.HasSuffix(lastTags(t, rec, 11), ",figure=https://sp.example/avatars/id-bernd.gif"), lastTags(t, rec, 11))

	pointsFor(t, m, scorer.PlayerA, 11, 12) // the second set, to Anna
	require.True(t, strings.HasSuffix(lastTags(t, rec, 22), ",figure=https://sp.example/avatars/id-anna.gif"), lastTags(t, rec, 22))
}

// A match by names whose winner Schmetterpause knows by that name: the
// player's own figure (match.Setup.Figures), as on the spectator view.
func TestAMatchByNamesNamesTheFigureOfAPlayerSchmetterpauseKnows(t *testing.T) {
	rec := newRecorder()
	sink := New(rec, Config{Figures: "https://sp.example", Logger: quiet()})
	defer func() { require.NoError(t, sink.Close()) }()
	registry := match.NewRegistry(match.WithMatchObserver(sink.Observe))

	m, _, err := registry.CreateFor(scorer.Config{BestOf: 1, PointsPerSet: 11, Players: [2]string{"Anna", "Gast"}},
		match.Setup{Demo: true, Figures: [2]string{"id-anna", ""}})
	require.NoError(t, err)

	pointsFor(t, m, scorer.PlayerA, 11, 11)
	require.True(t, strings.HasSuffix(lastTags(t, rec, 11), ",figure=https://sp.example/avatars/id-anna.gif"), lastTags(t, rec, 11))
}

func TestNoFigureWithoutSomebodyBehindTheWinnerOrWithoutFigures(t *testing.T) {
	won := scorer.Transition{Kind: scorer.TransitionMatchWon, Side: scorer.PlayerA}

	require.Empty(t, FigureTag("", won, [2]string{"id-anna", "id-bernd"}), "figures not configured")
	require.Empty(t, FigureTag("https://sp.example", won, [2]string{"", "id-bernd"}),
		"a free-text name has a random figure on the screen, and a different random one here would be wrong")
	require.Empty(t, FigureTag("https://sp.example", scorer.Transition{Kind: scorer.TransitionPoint, Side: scorer.PlayerA},
		[2]string{"id-anna", "id-bernd"}))
	require.Equal(t, ",figure=https://sp.example/avatars/a%2Fb.gif",
		FigureTag("https://sp.example", won, [2]string{"a/b", ""}), "an id is a path segment, whatever it holds")
}

// The catcher draws the names in the players' colours, the ones the spectator
// view and the game's board give them, so the matrix does not contradict them.
func TestThePanelSaysThePlayersColours(t *testing.T) {
	rec := newRecorder()
	sink := New(rec, Config{Logger: quiet()})
	defer func() { require.NoError(t, sink.Close()) }()
	var asked []string
	sink.ColourFrom(func(_ context.Context, m *match.Match) [2]string {
		asked = append(asked, m.ID)
		return [2]string{"#c05fd0", "#4d7c4a"}
	})
	registry := match.NewRegistry(match.WithMatchObserver(sink.Observe))

	m, err := registry.Create(scorer.Config{Players: [2]string{"Anna", "Bernd"}})
	require.NoError(t, err)
	pointsFor(t, m, scorer.PlayerA, 1, 1)

	require.True(t, strings.HasSuffix(lastTags(t, rec, 1), ",color_a=#c05fd0,color_b=#4d7c4a"), lastTags(t, rec, 1))
	require.Equal(t, []string{m.ID}, asked)
}

func TestWithoutColoursThePanelSaysNone(t *testing.T) {
	rec := newRecorder()
	sink := New(rec, Config{Logger: quiet()})
	defer func() { require.NoError(t, sink.Close()) }()
	registry := match.NewRegistry(match.WithMatchObserver(sink.Observe))

	m, err := registry.Create(scorer.Config{Players: [2]string{"Anna", "Bernd"}})
	require.NoError(t, err)
	pointsFor(t, m, scorer.PlayerA, 1, 1)

	require.NotContains(t, lastTags(t, rec, 1), "color_")
}

func TestColoursGoOutBothOrNeither(t *testing.T) {
	require.Equal(t, ",color_a=#c05fd0,color_b=#4D7C4A", ColourTags([2]string{"#c05fd0", "#4D7C4A"}))
	require.Empty(t, ColourTags([2]string{"#c05fd0", ""}), "one name in a colour would look picked out")
	require.Empty(t, ColourTags([2]string{"#c05fd0", "red"}))
	require.Empty(t, ColourTags([2]string{"#c05fd0", "#4d7c4a,x=1"}), "a colour cannot add a tag")
}
