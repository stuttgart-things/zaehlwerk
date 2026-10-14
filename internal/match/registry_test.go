package match

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// counter hands out predictable ids so the tests can name matches.
func counter() func() (string, error) {
	var n int
	return func() (string, error) {
		n++
		return fmt.Sprintf("m%d", n), nil
	}
}

func newTestRegistry(t *testing.T, opts ...Option) *Registry {
	t.Helper()
	return NewRegistry(append([]Option{WithIDs(counter())}, opts...)...)
}

// win plays a whole match out so it completes on court.
func win(t *testing.T, m *Match, p scorer.Player) {
	t.Helper()

	for i := 1; m.Scorer.State().Complete == false; i++ {
		_, err := m.Scorer.Apply(scorer.ScoreEvent{
			MatchID: m.ID, Player: p, Delta: 1, EventID: uint64(i), Source: "test",
		})
		require.NoError(t, err)
		require.Less(t, i, 500, "match did not finish")
	}
}

func TestCreateAssignsTheID(t *testing.T) {
	r := newTestRegistry(t)

	m, err := r.Create(scorer.Config{MatchID: "ignored", Players: [2]string{"Anna", "Bernd"}})
	require.NoError(t, err)
	require.Equal(t, "m1", m.ID)
	require.Equal(t, "m1", m.Scorer.State().MatchID)
	require.Equal(t, [2]string{"Anna", "Bernd"}, m.Scorer.State().Players)
}

func TestCreateRejectsAConfigTheScorerWillNotTake(t *testing.T) {
	r := newTestRegistry(t)

	_, err := r.Create(scorer.Config{BestOf: 4})
	require.Error(t, err)

	// And the failed match did not land in the registry.
	_, err = r.Get("m1")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestGetUnknownID(t *testing.T) {
	r := newTestRegistry(t)

	_, err := r.Get("nope")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestRandomIDsAreDistinctAndUsable(t *testing.T) {
	r := NewRegistry()

	seen := make(map[string]bool)
	for range 50 {
		m, err := r.Create(scorer.Config{})
		require.NoError(t, err)
		require.Len(t, m.ID, 8)
		require.False(t, seen[m.ID], "duplicate id %q", m.ID)
		seen[m.ID] = true
	}
}

func TestFreeIDRetriesPastACollision(t *testing.T) {
	ids := []string{"m1", "m1", "m2"}
	var i int
	r := NewRegistry(WithIDs(func() (string, error) {
		id := ids[i]
		i++
		return id, nil
	}))

	first, err := r.Create(scorer.Config{})
	require.NoError(t, err)
	require.Equal(t, "m1", first.ID)

	second, err := r.Create(scorer.Config{})
	require.NoError(t, err)
	require.Equal(t, "m2", second.ID, "the collision was drawn again, not handed out twice")
}

func TestIDGenerationFailures(t *testing.T) {
	t.Run("the generator fails", func(t *testing.T) {
		r := NewRegistry(WithIDs(func() (string, error) {
			return "", errors.New("no entropy")
		}))

		_, err := r.Create(scorer.Config{})
		require.ErrorIs(t, err, ErrIDGeneration)
	})

	t.Run("every draw collides", func(t *testing.T) {
		r := NewRegistry(WithIDs(func() (string, error) { return "same", nil }))

		_, err := r.Create(scorer.Config{})
		require.NoError(t, err)

		_, err = r.Create(scorer.Config{})
		require.ErrorIs(t, err, ErrIDGeneration)

		// And the match that is there was not replaced by the failed attempt.
		m, err := r.Get("same")
		require.NoError(t, err)
		require.True(t, m.Running())
	})
}

func TestCurrentIsTheMostRecentRunningMatch(t *testing.T) {
	r := newTestRegistry(t)

	_, err := r.Current()
	require.ErrorIs(t, err, ErrNotFound, "nothing is running yet")

	first, err := r.Create(scorer.Config{})
	require.NoError(t, err)

	current, err := r.Current()
	require.NoError(t, err)
	require.Equal(t, first, current)

	second, err := r.Create(scorer.Config{})
	require.NoError(t, err)

	current, err = r.Current()
	require.NoError(t, err)
	require.Equal(t, second, current, "the newer match takes over")

	// The newer one finishes on court, so hardware points belong to the older
	// one again rather than to nothing.
	win(t, second, scorer.PlayerA)
	current, err = r.Current()
	require.NoError(t, err)
	require.Equal(t, first, current)

	// And when that one is ended by hand there is nothing left to score into.
	first.End(time.Now())
	_, err = r.Current()
	require.ErrorIs(t, err, ErrNotFound)
}

func TestEndIsIdempotent(t *testing.T) {
	r := newTestRegistry(t)
	m, err := r.Create(scorer.Config{})
	require.NoError(t, err)

	require.True(t, m.Running())
	_, ended := m.EndedAt()
	require.False(t, ended)

	first := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	require.Equal(t, first, m.End(first))
	require.Equal(t, first, m.End(first.Add(time.Hour)), "a retry does not move the end time")

	at, ended := m.EndedAt()
	require.True(t, ended)
	require.Equal(t, first, at)
	require.False(t, m.Running())
}

func TestAMatchWonOnCourtIsNotRunning(t *testing.T) {
	r := newTestRegistry(t)
	m, err := r.Create(scorer.Config{BestOf: 1})
	require.NoError(t, err)

	win(t, m, scorer.PlayerB)
	require.False(t, m.Running())

	// It was never ended by hand, though — the two are different things, and
	// the stream switching in #5 will care about the difference.
	_, ended := m.EndedAt()
	require.False(t, ended)
}

func TestFinishedMatchesAreEvictedOldestFirst(t *testing.T) {
	r := newTestRegistry(t, WithMaxRetained(3))

	for range 3 {
		m, err := r.Create(scorer.Config{})
		require.NoError(t, err)
		m.End(time.Now())
	}

	_, err := r.Create(scorer.Config{})
	require.NoError(t, err)

	_, err = r.Get("m1")
	require.ErrorIs(t, err, ErrNotFound, "the oldest finished match was dropped")
	for _, id := range []string{"m2", "m3", "m4"} {
		_, err := r.Get(id)
		require.NoErrorf(t, err, "%s should still be there", id)
	}
}

func TestARunningMatchIsNeverEvicted(t *testing.T) {
	r := newTestRegistry(t, WithMaxRetained(2))

	running, err := r.Create(scorer.Config{}) // m1, left running
	require.NoError(t, err)

	for range 5 {
		m, err := r.Create(scorer.Config{})
		require.NoError(t, err)
		m.End(time.Now())
	}

	_, err = r.Get(running.ID)
	require.NoError(t, err, "the match in progress outlives the limit")

	current, err := r.Current()
	require.NoError(t, err)
	require.Equal(t, running, current)
}

// TestNothingIsEvictedWhileEverythingIsRunning — the limit exists to stop a
// process that runs for months from growing without bound, not to throw away a
// match that is being played.
func TestNothingIsEvictedWhileEverythingIsRunning(t *testing.T) {
	r := newTestRegistry(t, WithMaxRetained(1))

	for range 3 {
		_, err := r.Create(scorer.Config{})
		require.NoError(t, err)
	}

	for _, id := range []string{"m1", "m2", "m3"} {
		_, err := r.Get(id)
		require.NoErrorf(t, err, "%s should still be there", id)
	}

	// And once they finish, the limit catches up.
	for _, id := range []string{"m1", "m2"} {
		m, err := r.Get(id)
		require.NoError(t, err)
		m.End(time.Now())
	}

	_, err := r.Create(scorer.Config{})
	require.NoError(t, err)

	_, err = r.Get("m1")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = r.Get("m3")
	require.NoError(t, err, "still running")
}

func TestConcurrentCreateAndLookup(t *testing.T) {
	r := NewRegistry()

	var wg sync.WaitGroup
	ids := make(chan string, 50)

	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()

			m, err := r.Create(scorer.Config{})
			require.NoError(t, err)
			ids <- m.ID

			_, _ = r.Current()
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]bool)
	for id := range ids {
		require.False(t, seen[id])
		seen[id] = true

		_, err := r.Get(id)
		require.NoError(t, err)
	}
	require.Len(t, seen, 50)
}

// The registry is where the panel sink and, later, the live view get attached
// to a match. Nothing else does it, so an observer that is registered but never
// wired up leaves the panel dark with every other test still green.
func TestObserversAreAttachedToEveryNewMatch(t *testing.T) {
	var mu sync.Mutex
	var kinds []scorer.TransitionKind

	r := newTestRegistry(t, WithObserver(func(tr scorer.Transition) {
		mu.Lock()
		defer mu.Unlock()
		kinds = append(kinds, tr.Kind)
	}))

	m, err := r.Create(scorer.Config{Players: [2]string{"Anna", "Bernd"}, BestOf: 1})
	require.NoError(t, err)

	_, err = m.Scorer.Apply(scorer.ScoreEvent{
		MatchID: m.ID, Player: scorer.PlayerA, Delta: 1, EventID: 1, Source: "test",
	})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []scorer.TransitionKind{scorer.TransitionPoint}, kinds)
}

// A second match created later must be wired up too — the observer belongs to
// the registry, not to the match that happened to be first.
func TestObserversAreAttachedToLaterMatchesAsWell(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}

	r := newTestRegistry(t, WithObserver(func(tr scorer.Transition) {
		mu.Lock()
		defer mu.Unlock()
		seen[tr.State.MatchID]++
	}))

	for range 3 {
		m, err := r.Create(scorer.Config{Players: [2]string{"Anna", "Bernd"}})
		require.NoError(t, err)
		_, err = m.Scorer.Apply(scorer.ScoreEvent{
			MatchID: m.ID, Player: scorer.PlayerB, Delta: 1, EventID: 1, Source: "test",
		})
		require.NoError(t, err)
	}

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, map[string]int{"m1": 1, "m2": 1, "m3": 1}, seen)
}

// Several observers on one registry — the panel and the live view will both be
// there — and each sees every transition.
func TestSeveralObserversAllSeeTheTransition(t *testing.T) {
	var mu sync.Mutex
	var order []string

	r := newTestRegistry(t,
		WithObserver(func(scorer.Transition) { mu.Lock(); defer mu.Unlock(); order = append(order, "first") }),
		WithObserver(func(scorer.Transition) { mu.Lock(); defer mu.Unlock(); order = append(order, "second") }),
	)

	m, err := r.Create(scorer.Config{Players: [2]string{"Anna", "Bernd"}})
	require.NoError(t, err)
	_, err = m.Scorer.Apply(scorer.ScoreEvent{
		MatchID: m.ID, Player: scorer.PlayerA, Delta: 1, EventID: 1, Source: "test",
	})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"first", "second"}, order)
}

// The reporter needs the match a transition belongs to, and asking the
// registry for it from inside the scorer's lock is what hung the service
// (#102). So it is handed over, and it is the match the point was scored in.
func TestAMatchObserverIsToldWhichMatch(t *testing.T) {
	var mu sync.Mutex
	told := map[string]*Match{}

	r := newTestRegistry(t, WithMatchObserver(func(m *Match, tr scorer.Transition) {
		mu.Lock()
		defer mu.Unlock()
		require.Equal(t, m.ID, tr.State.MatchID)
		told[m.ID] = m
	}))

	var created []*Match
	for range 2 {
		m, _, err := r.CreateFor(scorer.Config{Players: [2]string{"Anna", "Bernd"}},
			Setup{Handover: Handover{HomeID: "h", AwayID: "a", OperatorID: "o"}})
		require.NoError(t, err)
		_, err = m.Scorer.Apply(scorer.ScoreEvent{
			MatchID: m.ID, Player: scorer.PlayerA, Delta: 1, EventID: 1, Source: "test",
		})
		require.NoError(t, err)
		created = append(created, m)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, m := range created {
		require.Same(t, m, told[m.ID], "the observer must be handed the match itself")
		require.True(t, told[m.ID].Handover.Wanted(), "and all of it, the handover included")
	}
}

// The reporter is registered before the hub, so that the result card the hub
// renders at match_won already knows when the result goes (ADR-0007). The two
// kinds of observer share one order, the order the options were given in.
func TestObserversRunInTheOrderTheyWereGiven(t *testing.T) {
	var mu sync.Mutex
	var order []string
	plain := func(name string) Option {
		return WithObserver(func(scorer.Transition) { mu.Lock(); defer mu.Unlock(); order = append(order, name) })
	}
	withMatch := func(name string) Option {
		return WithMatchObserver(func(*Match, scorer.Transition) { mu.Lock(); defer mu.Unlock(); order = append(order, name) })
	}

	r := newTestRegistry(t, withMatch("reporter"), plain("sink"), plain("hub"), withMatch("late"), plain("switcher"))

	m, err := r.Create(scorer.Config{Players: [2]string{"Anna", "Bernd"}})
	require.NoError(t, err)
	_, err = m.Scorer.Apply(scorer.ScoreEvent{
		MatchID: m.ID, Player: scorer.PlayerA, Delta: 1, EventID: 1, Source: "test",
	})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"reporter", "sink", "hub", "late", "switcher"}, order)
}

// A registry without observers is the local-development case and must not grow
// a nil call on the way through Create.
func TestARegistryWithoutObserversStillCreatesMatches(t *testing.T) {
	r := newTestRegistry(t)

	m, err := r.Create(scorer.Config{Players: [2]string{"Anna", "Bernd"}})
	require.NoError(t, err)
	_, err = m.Scorer.Apply(scorer.ScoreEvent{
		MatchID: m.ID, Player: scorer.PlayerA, Delta: 1, EventID: 1, Source: "test",
	})
	require.NoError(t, err)
	require.Equal(t, 1, m.Scorer.State().Points[0])
}

func demo(t *testing.T, r *Registry) *Match {
	t.Helper()
	m, ended, err := r.CreateFor(scorer.Config{Players: [2]string{"Pong A", "Pong B"}}, Setup{Demo: true})
	require.NoError(t, err)
	require.Empty(t, ended, "a demo ends nothing")
	require.True(t, m.Demo)
	return m
}

// ADR-0008 (a): whatever a demo held, the real match has the table from its
// first press, and the caller is handed the demo to announce as ended.
func TestARealMatchEndsTheRunningDemo(t *testing.T) {
	r := newTestRegistry(t)
	d := demo(t, r)

	real, ended, err := r.CreateFor(scorer.Config{Players: [2]string{"Anna", "Bernd"}}, Setup{})
	require.NoError(t, err)
	require.False(t, real.Demo)
	require.Equal(t, []*Match{d}, ended)
	require.False(t, d.Running(), "ended the way POST /matches/{id}/end ends it")
	_, byHand := d.EndedAt()
	require.True(t, byHand)

	current, err := r.Current()
	require.NoError(t, err)
	require.Same(t, real, current)
}

// A demo that is already over is nothing to end, and nothing to announce
// twice.
func TestADemoThatIsOverIsNotEndedAgain(t *testing.T) {
	r := newTestRegistry(t)
	d := demo(t, r)
	win(t, d, scorer.PlayerA)

	_, ended, err := r.CreateFor(scorer.Config{}, Setup{})
	require.NoError(t, err)
	require.Empty(t, ended)
	_, byHand := d.EndedAt()
	require.False(t, byHand, "won on court stays won on court")
}

// ADR-0008 (c): the race where the real match was created first. The demo
// never comes into existence.
func TestADemoIsRefusedWhileARealMatchRuns(t *testing.T) {
	r := newTestRegistry(t)
	real, err := r.Create(scorer.Config{Players: [2]string{"Anna", "Bernd"}})
	require.NoError(t, err)

	_, _, err = r.CreateFor(scorer.Config{}, Setup{Demo: true})
	require.ErrorIs(t, err, ErrTableTaken)
	var taken *TableTakenError
	require.ErrorAs(t, err, &taken)
	require.Same(t, real, taken.Running, "the error names the match holding the table")

	current, err := r.Current()
	require.NoError(t, err)
	require.Same(t, real, current)

	// Once the real match is over, the table is free for a demo again.
	real.End(time.Now())
	demo(t, r)
}

func TestADemoWithPlayerIDsIsRefused(t *testing.T) {
	r := newTestRegistry(t)

	for _, h := range []Handover{
		{HomeID: "h", AwayID: "a", OperatorID: "o"},
		{HomeID: "h"},
		{OperatorID: "o"},
	} {
		_, _, err := r.CreateFor(scorer.Config{}, Setup{Handover: h, Demo: true})
		require.ErrorIs(t, err, ErrDemoReported, "%+v", h)
	}
	_, err := r.Current()
	require.ErrorIs(t, err, ErrNotFound, "nothing was created")
}

// Only a real match ends demos. A demo created next to another one takes over
// as the newer running match, as any match did before ADR-0008.
func TestAnotherDemoMayStartWhileADemoRuns(t *testing.T) {
	r := newTestRegistry(t)
	first := demo(t, r)
	second := demo(t, r)

	require.True(t, first.Running())
	current, err := r.Current()
	require.NoError(t, err)
	require.Same(t, second, current)
}

// #102 with ADR-0008 (a) in it: creating a real match asks every demo whether
// it is running, which takes the demo's scorer lock under the registry lock.
// A demo won at that moment holds its scorer lock while its observers run, and
// they touch the match, as the reporter does. Ending demos must not take the
// two the other way round.
func TestCreatingARealMatchWhileADemoIsWonDoesNotDeadlock(t *testing.T) {
	won := make(chan struct{})
	r := newTestRegistry(t, WithMatchObserver(func(m *Match, tr scorer.Transition) {
		if tr.Kind == scorer.TransitionMatchWon && m.Demo {
			close(won)
			// Long enough for the create below to reach the demo and wait
			// on its scorer lock, then touch the match under that lock.
			time.Sleep(100 * time.Millisecond)
			m.ReportDue(time.Time{})
			_, _ = m.EndedAt()
		}
	}))
	d, _, err := r.CreateFor(scorer.Config{BestOf: 1}, Setup{Demo: true})
	require.NoError(t, err)

	errs := make(chan error, 12)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for id := uint64(1); id <= 11; id++ {
			_, err := d.Scorer.Apply(scorer.ScoreEvent{
				MatchID: d.ID, Source: "test", EventID: id, Player: scorer.PlayerA, Delta: 1,
			})
			errs <- err
		}
	}()
	var ended []*Match
	created := make(chan struct{})
	go func() {
		defer close(created)
		<-won
		var err error
		_, ended, err = r.CreateFor(scorer.Config{}, Setup{})
		errs <- err
	}()

	for _, ch := range []chan struct{}{done, created} {
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatal("deadlock: the demo's match_won observer and the real match wait on each other's lock")
		}
	}
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Empty(t, ended, "the demo was won before it was asked, so there was nothing to end")
}

func TestADemoDoesNotOccupyTheTableAndARealMatchDoes(t *testing.T) {
	r := newTestRegistry(t)

	_, ok := r.Occupied()
	require.False(t, ok, "an empty table is not occupied")

	_, _, err := r.CreateFor(scorer.Config{}, Setup{Demo: true})
	require.NoError(t, err)
	_, ok = r.Occupied()
	require.False(t, ok, "a demo gives way, so it does not occupy the table")

	played, err := r.Create(scorer.Config{})
	require.NoError(t, err)
	got, ok := r.Occupied()
	require.True(t, ok)
	require.Equal(t, played.ID, got.ID)

	played.End(time.Now())
	_, ok = r.Occupied()
	require.False(t, ok, "an ended match frees the table")
}

func TestAHandoverIsWantedWithAnOperatorOrWithBothCheckins(t *testing.T) {
	require.True(t, Handover{HomeID: "h", AwayID: "a", OperatorID: "o"}.Wanted())
	require.True(t, Handover{HomeID: "h", AwayID: "a", HomeCheckinID: "c1", AwayCheckinID: "c2"}.Wanted())
	require.False(t, Handover{HomeID: "h", AwayID: "a", HomeCheckinID: "c1"}.Wanted(),
		"one check-in is no proof, and schmetterpause refuses it")
	require.False(t, Handover{HomeID: "h", AwayID: "a"}.Wanted())
}
