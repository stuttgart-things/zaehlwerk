package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/api"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
	"github.com/stuttgart-things/zaehlwerk/internal/ui"
)

const (
	anna  = "11111111-1111-4111-8111-111111111111"
	bernd = "22222222-2222-4222-8222-222222222222"
	olga  = "44444444-4444-4444-8444-444444444444"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fake starts the fake Schmetterpause and a real client pointed at it. The
// client is the one the service uses, which is what makes this a contract
// test rather than a test of the fake against itself.
func fake(t *testing.T, mode string) (*fakeSchmetterpause, *httptest.Server, *schmetterpause.Client) {
	t.Helper()

	f, err := newFakeSchmetterpause("secret", mode, quiet)
	require.NoError(t, err)
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	c, err := schmetterpause.New(schmetterpause.Config{BaseURL: srv.URL, Token: "secret", Timeout: time.Second})
	require.NoError(t, err)
	return f, srv, c
}

func result(home, away, operator string) schmetterpause.Result {
	return schmetterpause.Result{HomeID: home, AwayID: away, OperatorID: operator,
		Sets: [][2]int{{11, 7}, {9, 11}, {12, 10}}, BestOf: 3, PointsToWin: 11}
}

func TestTheClientReadsTheFakeRosterAsItReadsTheRealOne(t *testing.T) {
	_, _, c := fake(t, modeAccept)

	players, err := c.Players(context.Background())
	require.NoError(t, err)
	require.Len(t, players, 3, "an observer is never offered as a side")

	operators, err := c.Operators(context.Background())
	require.NoError(t, err)
	require.Len(t, operators, 4)
	require.Contains(t, operators, schmetterpause.Operator{ID: olga, DisplayName: "Olga", Observer: true})
}

func TestAFinishedResultIsAcceptedPending(t *testing.T) {
	f, _, c := fake(t, modeAccept)

	accepted, err := c.Report(context.Background(), result(anna, bernd, olga))
	require.NoError(t, err)
	require.Equal(t, "pending", accepted.Status)
	require.NotEmpty(t, accepted.MatchID)

	stored := f.stored()
	require.Len(t, stored, 1)
	require.Equal(t, "Anna", stored[0].Home)
	require.Equal(t, "Olga", stored[0].Operator)
}

func TestTheFakeRefusesWhatTheRealOneRefuses(t *testing.T) {
	_, _, c := fake(t, modeAccept)

	cases := map[string]struct {
		result schmetterpause.Result
		reason string
	}{
		"an operator who is playing": {result(anna, bernd, anna), "the operator may not be one of the two players"},
		"an observer as a side":      {result(anna, olga, bernd), "an observer cannot be one of the two players"},
		"an unknown player":          {result(anna, "55555555-5555-4555-8555-555555555555", olga), "no player with id"},
		"the same player twice":      {result(anna, anna, olga), "must differ"},
		"an unfinished match": {schmetterpause.Result{HomeID: anna, AwayID: bernd, OperatorID: olga,
			Sets: [][2]int{{11, 7}}, BestOf: 3, PointsToWin: 11}, "nobody won"},
		"a set won by one": {schmetterpause.Result{HomeID: anna, AwayID: bernd, OperatorID: olga,
			Sets: [][2]int{{11, 10}}, BestOf: 1, PointsToWin: 11}, "less than two"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := c.Report(context.Background(), tc.result)
			require.ErrorContains(t, err, tc.reason)
		})
	}
}

func TestAWrongTokenIsUnauthorized(t *testing.T) {
	_, srv, _ := fake(t, modeAccept)
	c, err := schmetterpause.New(schmetterpause.Config{BaseURL: srv.URL, Token: "wrong"})
	require.NoError(t, err)

	_, err = c.Players(context.Background())
	require.ErrorContains(t, err, "401")
}

func TestWithoutATokenThereIsNoAPI(t *testing.T) {
	f, err := newFakeSchmetterpause("", modeAccept, quiet)
	require.NoError(t, err)
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/players")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestARefusingSchmetterpauseCanBeSwitchedBack(t *testing.T) {
	_, srv, c := fake(t, modeRefuse)

	_, err := c.Report(context.Background(), result(anna, bernd, olga))
	require.ErrorContains(t, err, "503")

	resp, err := http.PostForm(srv.URL+"/mode", url.Values{"mode": {modeAccept}})
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	_, err = c.Report(context.Background(), result(anna, bernd, olga))
	require.NoError(t, err)
}

func TestAHangingSchmetterpauseRunsIntoTheClientTimeout(t *testing.T) {
	_, _, c := fake(t, modeHang)

	_, err := c.Report(context.Background(), result(anna, bernd, olga))
	require.Error(t, err)
}

// chain wires zaehlwerk the way cmd/zaehlwerk-api does with SCHMETTERPAUSE_URL
// set: the reporter as a registry observer, the page offering the roster.
func chain(t *testing.T, sp *schmetterpause.Client) *httptest.Server {
	t.Helper()

	var reporter *schmetterpause.Reporter
	registry := match.NewRegistry(match.WithObserver(func(tr scorer.Transition) {
		reporter.Observe(tr)
	}))
	reporter = schmetterpause.NewReporter(sp, registry, schmetterpause.WithLogger(quiet))

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
	f, _, sp := fake(t, modeAccept)
	zw := chain(t, sp)

	b := newBoard(boardConfig{
		API: zw.URL, Source: "piezo-test", Matches: 1, Seed: 7, BestOf: 3,
		Ambiguous: 0.2, Resend: 0.3, Undo: 0.1, Roster: sp,
	}, quiet)
	require.NoError(t, b.run(context.Background()))

	require.Eventually(t, func() bool { return len(f.stored()) == 1 },
		2*time.Second, 10*time.Millisecond, "the reporter posts the result from a goroutine")

	got := f.stored()[0]
	require.NotEqual(t, got.Home, got.Away)
	require.Equal(t, "Olga", got.Operator, "an observer keeps score when there is one")
	require.Equal(t, 3, got.BestOf)
	require.NotEmpty(t, got.Sets)
}

// The board reads the players rather than knowing them, so it runs unchanged
// against a Schmetterpause with other people in it.
func TestTheBoardPlaysWhoeverTheRosterHolds(t *testing.T) {
	f, _, sp := fake(t, modeAccept)
	f.roster = []rosterEntry{
		{ID: uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"), DisplayName: "Ada"},
		{ID: uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"), DisplayName: "Grace"},
		{ID: uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc"), DisplayName: "Linus"},
	}
	zw := chain(t, sp)

	b := newBoard(boardConfig{API: zw.URL, Source: "piezo-test", Matches: 1, Seed: 3, BestOf: 1, Roster: sp}, quiet)
	require.NoError(t, b.run(context.Background()))

	require.Eventually(t, func() bool { return len(f.stored()) == 1 }, 2*time.Second, 10*time.Millisecond)
	got := f.stored()[0]
	names := []string{got.Home, got.Away, got.Operator}
	require.ElementsMatch(t, []string{"Ada", "Grace", "Linus"}, names,
		"two play and the third keeps score, with no observer to prefer")
}

func TestABoardWithOnlyTwoPeopleCannotStartAMatch(t *testing.T) {
	f, _, sp := fake(t, modeAccept)
	f.roster = f.roster[:2]
	zw := chain(t, sp)

	b := newBoard(boardConfig{API: zw.URL, Source: "piezo-test", Matches: 1, BestOf: 3, Roster: sp}, quiet)
	require.ErrorContains(t, b.run(context.Background()), "may not")
}

// zaehlwerk reporting somewhere else would refuse the result at the very end;
// the board says so before the first rally instead.
func TestABoardNoticesZaehlwerkReadsAnotherSchmetterpause(t *testing.T) {
	_, _, ours := fake(t, modeAccept)
	theirs, _, other := fake(t, modeAccept)
	theirs.roster = []rosterEntry{
		{ID: uuid.MustParse(anna), DisplayName: "Anna Other"},
		{ID: uuid.MustParse(bernd), DisplayName: "Bernd Other"},
		{ID: uuid.MustParse("33333333-3333-4333-8333-333333333333"), DisplayName: "Clara Other"},
	}
	zw := chain(t, other)

	b := newBoard(boardConfig{API: zw.URL, Source: "piezo-test", Matches: 1, BestOf: 3, Roster: ours}, quiet)
	require.ErrorContains(t, b.run(context.Background()), "same Schmetterpause")
}

// A sensor stays under its half; the players do not.
func TestTheHalvesChangePlayersEverySet(t *testing.T) {
	require.Equal(t, scorer.PlayerA, playerOn("A", 1))
	require.Equal(t, scorer.PlayerB, playerOn("A", 2))
	require.Equal(t, scorer.PlayerB, playerOn("B", 1))
	require.Equal(t, scorer.PlayerA, playerOn("B", 2))
}

func TestTheBoardSendsTheFourFieldsAndTheMatch(t *testing.T) {
	raw, err := json.Marshal(ingestBody{MatchID: "m", Source: "s", Player: "a", Delta: 0, EventID: 3})
	require.NoError(t, err)
	require.JSONEq(t, `{"match_id":"m","source":"s","player":"a","delta":0,"event_id":3}`, string(raw))
}
