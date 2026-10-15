package piezo

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/api"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
	"github.com/stuttgart-things/zaehlwerk/internal/ui"
	"github.com/stuttgart-things/zaehlwerk/tools/chain-mock/fakesp"
)

const (
	anna  = "11111111-1111-4111-8111-111111111111"
	bernd = "22222222-2222-4222-8222-222222222222"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fake starts the fake Schmetterpause and the service's own client for it.
func fake(t *testing.T) (*fakesp.Server, *schmetterpause.Client) {
	t.Helper()

	f, err := fakesp.New("secret", fakesp.ModeAccept, quiet)
	require.NoError(t, err)
	srv := httptest.NewServer(f.Handler())
	t.Cleanup(srv.Close)

	c, err := schmetterpause.New(schmetterpause.Config{BaseURL: srv.URL, Token: "secret", Timeout: time.Second})
	require.NoError(t, err)
	return f, c
}

// chain wires zaehlwerk the way cmd/zaehlwerk-api does with SCHMETTERPAUSE_URL
// set: the reporter as a registry observer, the page offering the roster.
func chain(t *testing.T, sp *schmetterpause.Client) *httptest.Server {
	t.Helper()

	// No grace period: what is under test is the result arriving, not when;
	// the reporter's own tests cover the wait.
	reporter := schmetterpause.NewReporter(sp,
		schmetterpause.WithLogger(quiet), schmetterpause.WithDelay(0))
	registry := match.NewRegistry(match.WithMatchObserver(reporter.Observe))

	apiSrv := api.New(registry, api.WithLogger(quiet))
	uiSrv := ui.New(registry, apiSrv, ui.WithLogger(quiet), ui.WithSchmetterpause(sp, reporter))

	mux := http.NewServeMux()
	mux.Handle("/", apiSrv)
	mux.Handle("/ui/", uiSrv)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestABoardPlaysAMatchFromThePageIntoSchmetterpause(t *testing.T) {
	f, sp := fake(t)
	zw := chain(t, sp)

	b := New(Config{
		API: zw.URL, Source: "piezo-test", Matches: 1, Seed: 7, BestOf: 3,
		Ambiguous: 0.2, Resend: 0.3, Undo: 0.1, Roster: sp,
	}, quiet)
	require.NoError(t, b.Run(context.Background()))

	require.Eventually(t, func() bool { return len(f.Results()) == 1 },
		2*time.Second, 10*time.Millisecond, "the reporter posts the result from a goroutine")

	got := f.Results()[0]
	require.NotEqual(t, got.Home, got.Away)
	require.Equal(t, "Olga", got.Operator, "an observer keeps score when there is one")
	require.Equal(t, 3, got.BestOf)
	require.NotEmpty(t, got.Sets)
}

// Every correction goes out twice, and the board stops with an error if the
// second one takes back another point: the undo event is a duplicate the way
// a rally is (ADR-0012).
func TestABoardsResentCorrectionTakesBackOnePoint(t *testing.T) {
	f, sp := fake(t)
	zw := chain(t, sp)

	b := New(Config{
		API: zw.URL, Source: "piezo-test", Matches: 1, Seed: 11, BestOf: 1,
		Resend: 1, Undo: 0.3, Roster: sp,
	}, quiet)
	require.NoError(t, b.Run(context.Background()))

	require.Eventually(t, func() bool { return len(f.Results()) == 1 }, 2*time.Second, 10*time.Millisecond)
}

// The board reads the players rather than knowing them, so it runs unchanged
// against a Schmetterpause with other people in it.
func TestTheBoardPlaysWhoeverTheRosterHolds(t *testing.T) {
	f, sp := fake(t)
	f.Roster = []fakesp.Person{
		{ID: uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"), DisplayName: "Ada"},
		{ID: uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"), DisplayName: "Grace"},
		{ID: uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc"), DisplayName: "Linus"},
	}
	zw := chain(t, sp)

	b := New(Config{API: zw.URL, Source: "piezo-test", Matches: 1, Seed: 3, BestOf: 1, Roster: sp}, quiet)
	require.NoError(t, b.Run(context.Background()))

	require.Eventually(t, func() bool { return len(f.Results()) == 1 }, 2*time.Second, 10*time.Millisecond)
	got := f.Results()[0]
	names := []string{got.Home, got.Away, got.Operator}
	require.ElementsMatch(t, []string{"Ada", "Grace", "Linus"}, names,
		"two play and the third keeps score, with no observer to prefer")
}

func TestABoardWithOnlyTwoPeopleCannotStartAMatch(t *testing.T) {
	f, sp := fake(t)
	f.Roster = f.Roster[:2]
	zw := chain(t, sp)

	b := New(Config{API: zw.URL, Source: "piezo-test", Matches: 1, BestOf: 3, Roster: sp}, quiet)
	require.ErrorContains(t, b.Run(context.Background()), "may not")
}

// zaehlwerk reporting somewhere else would refuse the result at the very end;
// the board says so before the first rally instead.
func TestABoardNoticesZaehlwerkReadsAnotherSchmetterpause(t *testing.T) {
	_, ours := fake(t)
	theirs, other := fake(t)
	theirs.Roster = []fakesp.Person{
		{ID: uuid.MustParse(anna), DisplayName: "Anna Other"},
		{ID: uuid.MustParse(bernd), DisplayName: "Bernd Other"},
		{ID: uuid.MustParse("33333333-3333-4333-8333-333333333333"), DisplayName: "Clara Other"},
	}
	zw := chain(t, other)

	b := New(Config{API: zw.URL, Source: "piezo-test", Matches: 1, BestOf: 3, Roster: ours}, quiet)
	require.ErrorContains(t, b.Run(context.Background()), "same Schmetterpause")
}

// A sensor stays under its half; the players do not.
func TestTheHalvesChangePlayersEverySet(t *testing.T) {
	require.Equal(t, scorer.PlayerA, playerOn("A", 1))
	require.Equal(t, scorer.PlayerB, playerOn("A", 2))
	require.Equal(t, scorer.PlayerB, playerOn("B", 1))
	require.Equal(t, scorer.PlayerA, playerOn("B", 2))
}

func TestTheBoardSendsTheFourFieldsAndTheMatch(t *testing.T) {
	zero := 0
	raw, err := json.Marshal(ingestBody{MatchID: "m", Source: "s", Player: "a", Delta: &zero, EventID: 3})
	require.NoError(t, err)
	require.JSONEq(t, `{"match_id":"m","source":"s","player":"a","delta":0,"event_id":3}`, string(raw))

	// An undo has no player and must have no delta, or ingest refuses it.
	raw, err = json.Marshal(ingestBody{MatchID: "m", Source: "s", Action: "undo", EventID: 4})
	require.NoError(t, err)
	require.JSONEq(t, `{"match_id":"m","source":"s","action":"undo","event_id":4}`, string(raw))
}

// A demo is any match zaehlwerk-pong started. A board that only rolls rallies
// would count into it, which ends the game on the canvas within a few rallies; it
// waits instead, and joins the real match that takes the table.
func TestABoardWaitsThroughADemoAndJoinsTheRealMatch(t *testing.T) {
	registry := match.NewRegistry()
	apiSrv := api.New(registry, api.WithLogger(quiet))
	srv := httptest.NewServer(apiSrv)
	t.Cleanup(srv.Close)

	post := func(body string) string {
		t.Helper()
		resp, err := http.Post(srv.URL+"/matches", "application/json", strings.NewReader(body))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		var st scorer.State
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&st))
		return st.MatchID
	}
	demo := post(`{"players":["Pong A","Pong B"],"best_of":1,"demo":true}`)

	b := New(Config{API: srv.URL, Source: "piezo-test", Seed: 3, Join: true}, quiet)
	joined := make(chan scorer.State, 1)
	go func() {
		st, err := b.join(t.Context())
		if err == nil {
			joined <- st
		}
	}()

	require.Never(t, func() bool { return len(joined) > 0 }, 3*joinPoll, joinPoll/4,
		"the board joined the demo")
	require.Equal(t, "waiting, a demo is running", b.Status().Doing)
	m, err := registry.Get(demo)
	require.NoError(t, err)
	require.Zero(t, m.Scorer.State().Points, "nothing counted into the demo")

	real := post(`{"players":["Anna","Bernd"],"best_of":1}`)
	select {
	case st := <-joined:
		require.Equal(t, real, st.MatchID)
	case <-time.After(5 * joinPoll):
		t.Fatal("the board did not join the real match")
	}
}
