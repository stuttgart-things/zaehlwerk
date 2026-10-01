package piezo

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	raw, err := json.Marshal(ingestBody{MatchID: "m", Source: "s", Player: "a", Delta: 0, EventID: 3})
	require.NoError(t, err)
	require.JSONEq(t, `{"match_id":"m","source":"s","player":"a","delta":0,"event_id":3}`, string(raw))
}
