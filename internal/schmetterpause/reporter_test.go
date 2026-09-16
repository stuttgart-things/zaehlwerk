package schmetterpause

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

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

// reporterHarness wires a registry whose matches report to a stub.
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
	registry := match.NewRegistry(match.WithObserver(func(tr scorer.Transition) {
		// Counted here rather than inside the Reporter so the test can wait
		// for the goroutine the observer starts.
		if tr.Kind == scorer.TransitionMatchWon {
			wg.Add(1)
		}
		reporter.Observe(tr)
	}))
	reporter = NewReporter(c, registry, withDone(wg.Done))
	return registry, reporter, &wg
}

func TestAWonMatchIsReported(t *testing.T) {
	var got Result
	registry, _, wg := reporterHarness(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, decode(r, &got))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"match_id":"sp-1","status":"pending"}`))
	})

	m, err := registry.CreateFor(scorer.Config{BestOf: 3, PointsPerSet: 11},
		match.Handover{HomeID: "h", AwayID: "a", OperatorID: "o"})
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

	m, err := registry.CreateFor(scorer.Config{},
		match.Handover{HomeID: "h", AwayID: "a", OperatorID: "o"})
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

	m, err := registry.CreateFor(scorer.Config{BestOf: 3, PointsPerSet: 11},
		match.Handover{HomeID: "h", AwayID: "a", OperatorID: "o"})
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
