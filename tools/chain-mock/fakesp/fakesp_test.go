package fakesp

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
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
func fake(t *testing.T, mode string) (*Server, *httptest.Server, *schmetterpause.Client) {
	t.Helper()

	f, err := New("secret", mode, quiet)
	require.NoError(t, err)
	srv := httptest.NewServer(f.Handler())
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
	_, _, c := fake(t, ModeAccept)

	players, err := c.Players(context.Background())
	require.NoError(t, err)
	require.Len(t, players, 3, "an observer is never offered as a side")

	operators, err := c.Operators(context.Background())
	require.NoError(t, err)
	require.Len(t, operators, 4)
	require.Contains(t, operators, schmetterpause.Operator{ID: olga, DisplayName: "Olga", Observer: true})
}

func TestAFinishedResultIsAcceptedPending(t *testing.T) {
	f, _, c := fake(t, ModeAccept)

	accepted, err := c.Report(context.Background(), result(anna, bernd, olga))
	require.NoError(t, err)
	require.Equal(t, "pending", accepted.Status)
	require.NotEmpty(t, accepted.MatchID)

	stored := f.Results()
	require.Len(t, stored, 1)
	require.Equal(t, "Anna", stored[0].Home)
	require.Equal(t, "Olga", stored[0].Operator)
}

func TestTheFakeRefusesWhatTheRealOneRefuses(t *testing.T) {
	_, _, c := fake(t, ModeAccept)

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
	_, srv, _ := fake(t, ModeAccept)
	c, err := schmetterpause.New(schmetterpause.Config{BaseURL: srv.URL, Token: "wrong"})
	require.NoError(t, err)

	_, err = c.Players(context.Background())
	require.ErrorContains(t, err, "401")
}

func TestWithoutATokenThereIsNoAPI(t *testing.T) {
	f, err := New("", ModeAccept, quiet)
	require.NoError(t, err)
	srv := httptest.NewServer(f.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/players")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestARefusingSchmetterpauseCanBeSwitchedBack(t *testing.T) {
	_, srv, c := fake(t, ModeRefuse)

	_, err := c.Report(context.Background(), result(anna, bernd, olga))
	require.ErrorContains(t, err, "503")

	resp, err := http.PostForm(srv.URL+"/mode", url.Values{"mode": {ModeAccept}})
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	_, err = c.Report(context.Background(), result(anna, bernd, olga))
	require.NoError(t, err)
}

func TestAHangingSchmetterpauseRunsIntoTheClientTimeout(t *testing.T) {
	_, _, c := fake(t, ModeHang)

	_, err := c.Report(context.Background(), result(anna, bernd, olga))
	require.Error(t, err)
}
