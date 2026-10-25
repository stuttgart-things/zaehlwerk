package schmetterpause

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// playOut scores a whole match so the scorer emits TransitionMatchWon.
func playOut(t *testing.T, m *match.Match) {
	t.Helper()

	var id uint64
	for set := 0; set < 2; set++ {
		for i := 0; i < 11; i++ {
			id++
			_, err := m.Scorer.Apply(scorer.ScoreEvent{
				MatchID: m.ID, Source: "test", EventID: id,
				Player: scorer.PlayerA, Delta: 1,
			})
			require.NoError(t, err)
		}
	}
	require.True(t, m.Scorer.State().Complete, "the match should be won")
}

// reporterHarness wires a registry whose matches report to a stub, with no
// grace period: the tests of what is sent do not need to wait for it.
func reporterHarness(t *testing.T, handler http.HandlerFunc) (*match.Registry, *Reporter, *sync.WaitGroup) {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := New(Config{BaseURL: srv.URL, Token: "tok"})
	require.NoError(t, err)

	var (
		wg       sync.WaitGroup
		reporter *Reporter
	)
	registry := match.NewRegistry(match.WithMatchObserver(func(m *match.Match, tr scorer.Transition) {
		// Counted here rather than inside the Reporter so the test can wait
		// for the goroutine the observer starts.
		if tr.Kind == scorer.TransitionMatchWon {
			wg.Add(1)
		}
		reporter.Observe(m, tr)
	}))
	reporter = NewReporter(c, WithDelay(0), withDone(wg.Done))
	return registry, reporter, &wg
}

func TestAWonMatchIsReported(t *testing.T) {
	var got Result
	registry, _, wg := reporterHarness(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, decode(r, &got))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"match_id":"sp-1","status":"pending"}`))
	})

	m, _, err := registry.CreateFor(scorer.Config{BestOf: 3, PointsPerSet: 11},
		match.Setup{Handover: match.Handover{HomeID: "h", AwayID: "a", OperatorID: "o"}})
	require.NoError(t, err)

	playOut(t, m)
	wg.Wait()

	report := m.Report()
	require.True(t, report.Done, "a won match must be reported")
	require.Equal(t, "sp-1", report.MatchID)
	require.NoError(t, report.Err)

	require.Equal(t, "h", got.HomeID)
	require.Equal(t, "o", got.OperatorID)
	require.Equal(t, 3, got.BestOf)
	require.Equal(t, 11, got.PointsToWin)
	require.Equal(t, [][2]int{{11, 0}, {11, 0}}, got.Sets)
}

func TestATournamentMatchIsReportedWithItsPlace(t *testing.T) {
	var raw map[string]any
	registry, _, wg := reporterHarness(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&raw))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"match_id":"sp-t","status":"confirmed"}`))
	})

	m, _, err := registry.CreateFor(scorer.Config{BestOf: 3, PointsPerSet: 11},
		match.Setup{Handover: match.Handover{HomeID: "h", AwayID: "a", OperatorID: "o", TournamentID: "t", Round: 4}})
	require.NoError(t, err)
	playOut(t, m)
	wg.Wait()

	require.True(t, m.Report().Done)
	require.Equal(t, "t", raw["tournament_id"])
	require.EqualValues(t, 4, raw["round"])
}

// Schmetterpause refuses fields it does not know, so a match without a place
// sends none: a Schmetterpause from before tournaments keeps taking it.
func TestAMatchWithoutAPlaceSendsNoTournamentFields(t *testing.T) {
	var raw map[string]any
	registry, _, wg := reporterHarness(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&raw))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"match_id":"sp-c","status":"pending"}`))
	})

	m, _, err := registry.CreateFor(scorer.Config{BestOf: 3, PointsPerSet: 11},
		match.Setup{Handover: match.Handover{HomeID: "h", AwayID: "a", OperatorID: "o"}})
	require.NoError(t, err)
	playOut(t, m)
	wg.Wait()

	require.NotContains(t, raw, "tournament_id")
	require.NotContains(t, raw, "round")
}

// A place filled meanwhile, at the kiosk or by another table, is a refusal
// like any other: recorded on the match with Schmetterpause's sentence, not
// taken for a delivery.
func TestATournamentPlaceFilledMeanwhileIsAFailedReport(t *testing.T) {
	registry, _, wg := reporterHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"this place already holds a result"}`))
	})

	m, _, err := registry.CreateFor(scorer.Config{BestOf: 3, PointsPerSet: 11},
		match.Setup{Handover: match.Handover{HomeID: "h", AwayID: "a", OperatorID: "o", TournamentID: "t", Round: 1}})
	require.NoError(t, err)
	playOut(t, m)
	wg.Wait()

	report := m.Report()
	require.False(t, report.Done)
	require.ErrorContains(t, report.Err, "this place already holds a result")
}

// TestTheRulesReportedAreTheOnesPlayed covers the defaults: a match created
// without a format still played best of five to eleven, and reporting zeroes
// would describe a match nobody played.
func TestTheRulesReportedAreTheOnesPlayed(t *testing.T) {
	var got Result
	registry, _, wg := reporterHarness(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, decode(r, &got))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"match_id":"sp-2","status":"pending"}`))
	})

	m, _, err := registry.CreateFor(scorer.Config{},
		match.Setup{Handover: match.Handover{HomeID: "h", AwayID: "a", OperatorID: "o"}})
	require.NoError(t, err)

	// Best of five, so three sets rather than two.
	var id uint64
	for set := 0; set < 3; set++ {
		for i := 0; i < 11; i++ {
			id++
			_, err := m.Scorer.Apply(scorer.ScoreEvent{
				MatchID: m.ID, Source: "test", EventID: id,
				Player: scorer.PlayerA, Delta: 1,
			})
			require.NoError(t, err)
		}
	}
	wg.Wait()

	require.Equal(t, scorer.DefaultBestOf, got.BestOf)
	require.Equal(t, scorer.DefaultPointsPerSet, got.PointsToWin)
}

// TestAMatchWithoutPlayersIsNotReported is ADR-0004's first-class case: a
// match at a table with nothing else set up is scored, shown and reported
// nowhere.
func TestAMatchWithoutPlayersIsNotReported(t *testing.T) {
	var called bool
	registry, _, _ := reporterHarness(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	})

	m, err := registry.Create(scorer.Config{BestOf: 3, PointsPerSet: 11})
	require.NoError(t, err)
	playOut(t, m)

	require.False(t, called, "a match with no player ids must not be reported")
	require.False(t, m.Report().Done)
	require.Zero(t, m.Report().Attempts)
}

// TestAFailedHandoverIsRecordedAndRetryable is the whole reason the outcome
// lives on the match: ADR-0004 accepted a lost result but asked for the loss
// to be visible, and for the page to be able to try again.
func TestAFailedHandoverIsRecordedAndRetryable(t *testing.T) {
	var fail = true
	var attempts int
	registry, reporter, wg := reporterHarness(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"the result could not be stored"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"match_id":"sp-3","status":"pending"}`))
	})

	m, _, err := registry.CreateFor(scorer.Config{BestOf: 3, PointsPerSet: 11},
		match.Setup{Handover: match.Handover{HomeID: "h", AwayID: "a", OperatorID: "o"}})
	require.NoError(t, err)

	playOut(t, m)
	wg.Wait()

	report := m.Report()
	require.False(t, report.Done, "a refused result must not look reported")
	require.Error(t, report.Err)
	require.Contains(t, report.Err.Error(), "the result could not be stored")
	require.Equal(t, 1, report.Attempts)
	require.Equal(t, 1, attempts)

	// The retry from the page is the same call, and it succeeds once the other
	// side does.
	fail = false
	require.NoError(t, reporter.Send(context.Background(), m))

	report = m.Report()
	require.True(t, report.Done, "a retry that succeeds must clear the failure")
	require.Equal(t, "sp-3", report.MatchID)
	require.NoError(t, report.Err)
	require.Equal(t, 2, report.Attempts)

	// And a second retry after success does not enter the result twice.
	require.NoError(t, reporter.Send(context.Background(), m))
	require.Equal(t, 2, attempts, "an already-reported match must not be sent again")
}

func decode(r *http.Request, v any) error {
	defer func() { _ = r.Body.Close() }()
	return json.NewDecoder(r.Body).Decode(v)
}

// stub is a Schmetterpause that keeps the results it is sent.
type stub struct {
	mu      sync.Mutex
	results []Result
}

func (s *stub) sent() []Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Result(nil), s.results...)
}

// graceHarness is reporterHarness with a grace period, and a stub that keeps
// what it is sent rather than a hook to wait on: what matters here is when
// a result arrives, and whether one does.
func graceHarness(t *testing.T, delay time.Duration) (*match.Registry, *Reporter, *stub) {
	t.Helper()

	sp := &stub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got Result
		require.NoError(t, decode(r, &got))
		sp.mu.Lock()
		sp.results = append(sp.results, got)
		n := len(sp.results)
		sp.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"match_id":"sp-%d","status":"pending"}`, n)
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{BaseURL: srv.URL, Token: "tok"})
	require.NoError(t, err)

	var reporter *Reporter
	registry := match.NewRegistry(match.WithMatchObserver(func(m *match.Match, tr scorer.Transition) {
		reporter.Observe(m, tr)
	}))
	reporter = NewReporter(c, WithDelay(delay))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = reporter.Flush(ctx)
	})
	return registry, reporter, sp
}

// bestOfOne starts a reported best of one.
func bestOfOne(t *testing.T, registry *match.Registry) *match.Match {
	t.Helper()
	m, _, err := registry.CreateFor(scorer.Config{BestOf: 1, PointsPerSet: 11},
		match.Setup{Handover: match.Handover{HomeID: "h", AwayID: "a", OperatorID: "o"}})
	require.NoError(t, err)
	return m
}

// points scores n points for p, numbering on from *id.
func points(t *testing.T, m *match.Match, id *uint64, p scorer.Player, n int) {
	t.Helper()
	for range n {
		*id++
		_, err := m.Scorer.Apply(scorer.ScoreEvent{
			MatchID: m.ID, Source: "test", EventID: *id, Player: p, Delta: 1,
		})
		require.NoError(t, err)
	}
}

// misPressed plays a best of one to 10:9 and gives the match point to the
// wrong player: b wins it 9:11 on the scoresheet, where a was to level.
func misPressed(t *testing.T, m *match.Match) uint64 {
	t.Helper()
	var id uint64
	points(t, m, &id, scorer.PlayerA, 9)
	points(t, m, &id, scorer.PlayerB, 11)
	require.True(t, m.Scorer.State().Complete)
	return id
}

const grace = 100 * time.Millisecond

func TestAWonMatchIsReportedAfterTheGracePeriod(t *testing.T) {
	registry, _, sp := graceHarness(t, grace)
	m := bestOfOne(t, registry)

	won := time.Now()
	var id uint64
	points(t, m, &id, scorer.PlayerA, 11)

	require.False(t, m.Report().Due.IsZero(), "a won match must say when it is reported")
	require.Empty(t, sp.sent(), "nothing may go out before the grace period is over")

	require.Eventually(t, func() bool { return len(sp.sent()) == 1 }, 5*grace, 5*time.Millisecond)
	require.GreaterOrEqual(t, time.Since(won), grace)
	require.Equal(t, [][2]int{{11, 0}}, sp.sent()[0].Sets)

	// The stub keeps the result before it answers, and the reporter marks it
	// done only once it has the answer, so done comes a moment after sent
	// (#141).
	require.Eventually(t, func() bool { return m.Report().Done }, 5*grace, time.Millisecond)
	report := m.Report()
	require.True(t, report.Due.IsZero(), "a sent result no longer waits")
	require.Equal(t, [][2]int{{11, 0}}, report.Sent)
}

func TestAnUndoInTheGracePeriodWithholdsTheReport(t *testing.T) {
	registry, _, sp := graceHarness(t, grace)
	m := bestOfOne(t, registry)
	misPressed(t, m)

	_, err := m.Scorer.Undo()
	require.NoError(t, err)

	require.True(t, m.Report().Due.IsZero(), "a reopened match no longer waits to be reported")
	require.Never(t, func() bool { return len(sp.sent()) > 0 }, 3*grace, 5*time.Millisecond,
		"an undone match point must not be reported")
	require.False(t, m.Report().Done)
	require.Zero(t, m.Report().Attempts, "a withheld report is not a failed one")
	require.NoError(t, m.Report().Err)
}

func TestAMatchWonAgainAfterAnUndoReportsTheNewResult(t *testing.T) {
	registry, _, sp := graceHarness(t, grace)
	m := bestOfOne(t, registry)
	id := misPressed(t, m)

	_, err := m.Scorer.Undo()
	require.NoError(t, err)
	// The undo leaves 9:10; the point goes to a this time, and a wins 12:10.
	points(t, m, &id, scorer.PlayerA, 3)
	require.True(t, m.Scorer.State().Complete)

	require.Eventually(t, func() bool { return len(sp.sent()) == 1 }, 5*grace, 5*time.Millisecond)
	require.Never(t, func() bool { return len(sp.sent()) > 1 }, 2*grace, 5*time.Millisecond)
	require.Equal(t, [][2]int{{12, 10}}, sp.sent()[0].Sets)
}

// TestAMatchWonTwiceInTheGracePeriodWaitsFromTheSecondWin covers an undo whose
// stop the timer raced: the second win starts the clock again.
func TestAMatchWonTwiceInTheGracePeriodWaitsFromTheSecondWin(t *testing.T) {
	registry, reporter, sp := graceHarness(t, grace)
	m := bestOfOne(t, registry)
	misPressed(t, m)
	first := m.Report().Due

	time.Sleep(grace / 2)
	reporter.Observe(m, scorer.Transition{Kind: scorer.TransitionMatchWon, State: m.Scorer.State()})

	require.True(t, m.Report().Due.After(first), "a second win must start the grace period again")
	require.Eventually(t, func() bool { return len(sp.sent()) == 1 }, 5*grace, 5*time.Millisecond)
	require.Never(t, func() bool { return len(sp.sent()) > 1 }, 2*grace, 5*time.Millisecond)
}

func TestARetryReportsAtOnce(t *testing.T) {
	registry, reporter, sp := graceHarness(t, time.Hour)
	m := bestOfOne(t, registry)
	var id uint64
	points(t, m, &id, scorer.PlayerA, 11)
	require.False(t, m.Report().Due.IsZero())

	require.NoError(t, reporter.Send(context.Background(), m))

	require.Len(t, sp.sent(), 1, "the button sends now, not after the grace period")
	require.True(t, m.Report().Done)
	require.True(t, m.Report().Due.IsZero())
}

func TestShutdownSendsTheReportsStillWaiting(t *testing.T) {
	registry, reporter, sp := graceHarness(t, time.Hour)
	m := bestOfOne(t, registry)
	var id uint64
	points(t, m, &id, scorer.PlayerA, 11)
	require.Empty(t, sp.sent())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, reporter.Flush(ctx))

	require.Len(t, sp.sent(), 1, "a result waiting at SIGTERM must go out, not be lost")
	require.True(t, m.Report().Done)

	// From then on nothing waits.
	other := bestOfOne(t, registry)
	points(t, other, &id, scorer.PlayerB, 11)
	require.Eventually(t, func() bool { return len(sp.sent()) == 2 }, time.Second, 5*time.Millisecond)
}

func TestAZeroDelayReportsAtOnce(t *testing.T) {
	registry, _, sp := graceHarness(t, 0)
	m := bestOfOne(t, registry)
	var id uint64
	points(t, m, &id, scorer.PlayerA, 11)

	require.True(t, m.Report().Due.IsZero(), "with no grace period nothing waits")
	require.Eventually(t, func() bool { return len(sp.sent()) == 1 }, time.Second, time.Millisecond)
}

// TestAMatchUndoneAfterItsReportIsNotReportedAgain is the limit ADR-0007
// names: there is no route to take a result back, so the first one stays,
// and the page has the sets that were sent to say it no longer holds.
func TestAMatchUndoneAfterItsReportIsNotReportedAgain(t *testing.T) {
	registry, _, sp := graceHarness(t, grace)
	m := bestOfOne(t, registry)
	id := misPressed(t, m)
	require.Eventually(t, func() bool { return len(sp.sent()) == 1 }, 5*grace, 5*time.Millisecond)

	_, err := m.Scorer.Undo()
	require.NoError(t, err)
	points(t, m, &id, scorer.PlayerA, 3)

	require.Never(t, func() bool { return len(sp.sent()) > 1 }, 3*grace, 5*time.Millisecond)
	require.Equal(t, [][2]int{{9, 11}}, m.Report().Sent)
	require.True(t, m.Report().Due.IsZero())
}

// TestAMatchWonWhileAnotherIsCreatedDoesNotDeadlock is #102. An observer runs
// under its match's scorer lock; creating a match holds the registry lock and
// evict takes scorer locks. A reporter that looked the won match up in the
// registry from inside its observer took the two locks the other way round,
// and the whole service hung.
func TestAMatchWonWhileAnotherIsCreatedDoesNotDeadlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"match_id":"sp-1","status":"pending"}`))
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, Token: "tok"})
	require.NoError(t, err)

	reporter := NewReporter(c, WithDelay(time.Hour))
	t.Cleanup(func() { _ = reporter.Flush(context.Background()) })
	won := make(chan struct{})
	registry := match.NewRegistry(match.WithMaxRetained(1),
		// Ahead of the reporter: it holds the won match's scorer lock long
		// enough for the create below to reach evict and wait on that lock.
		match.WithObserver(func(tr scorer.Transition) {
			if tr.Kind == scorer.TransitionMatchWon {
				close(won)
				time.Sleep(100 * time.Millisecond)
			}
		}),
		match.WithMatchObserver(reporter.Observe),
	)

	// X runs, and is first in the order evict walks.
	x := bestOfOne(t, registry)

	// Errors are collected rather than required: FailNow ends only the
	// goroutine it is called from.
	errs := make(chan error, 12)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for id := uint64(1); id <= 11; id++ {
			_, err := x.Scorer.Apply(scorer.ScoreEvent{
				MatchID: x.ID, Source: "test", EventID: id, Player: scorer.PlayerA, Delta: 1,
			})
			errs <- err
		}
	}()
	created := make(chan struct{})
	go func() {
		defer close(created)
		<-won
		_, err := registry.Create(scorer.Config{})
		errs <- err
	}()

	for _, ch := range []chan struct{}{done, created} {
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatal("deadlock: the match_won observer and the new match wait on each other's lock")
		}
	}
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.False(t, x.Report().Due.IsZero(), "the reporter must still have seen the win")
}
