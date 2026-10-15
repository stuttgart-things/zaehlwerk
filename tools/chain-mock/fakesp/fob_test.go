package fakesp

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
)

// The roster's fobs, which are the table mock's (fobuid.For).
var (
	annaFob  = DefaultRoster[0].Fob
	berndFob = DefaultRoster[1].Fob
	claraFob = DefaultRoster[2].Fob
)

func checkedIn(home, away, homeCheckin, awayCheckin string) schmetterpause.Result {
	r := result(home, away, "")
	r.HomeCheckinID, r.AwayCheckinID = homeCheckin, awayCheckin
	return r
}

func TestTheClientResolvesAFakeFobAsItResolvesARealOne(t *testing.T) {
	f, _, c := fake(t, ModeAccept)

	got, err := c.ResolveFob(context.Background(), strings.ReplaceAll(strings.ToLower(annaFob), ":", "-"), "left")
	require.NoError(t, err)
	require.Equal(t, anna, got.PlayerID)
	require.NotEmpty(t, got.CheckinID)

	again, err := c.ResolveFob(context.Background(), annaFob, "left")
	require.NoError(t, err)
	require.NotEqual(t, got.CheckinID, again.CheckinID, "every resolve is a new check-in")
	require.Len(t, f.Checkins(), 2)
}

func TestAnUnknownFobIsAnEmpty404(t *testing.T) {
	_, _, c := fake(t, ModeAccept)
	_, err := c.ResolveFob(context.Background(), "DE:AD:BE:EF", "left")
	require.ErrorIs(t, err, schmetterpause.ErrUnknownFob)
}

func TestWithoutTheFobKeyThereIsNoRoute(t *testing.T) {
	f, err := New("secret", ModeAccept, quiet)
	require.NoError(t, err)
	f.NoFobs = true
	srv := httptest.NewServer(f.Handler())
	t.Cleanup(srv.Close)
	c, err := schmetterpause.New(schmetterpause.Config{BaseURL: srv.URL, Token: "secret"})
	require.NoError(t, err)

	_, err = c.ResolveFob(context.Background(), annaFob, "left")
	require.ErrorIs(t, err, schmetterpause.ErrNoFobRoute)
}

func TestAnUnknownFobIsLinkedInsideTheWindow(t *testing.T) {
	_, srv, c := fake(t, ModeAccept)

	resp, err := http.PostForm(srv.URL+"/link", url.Values{"player_id": {bernd}})
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got, err := c.ResolveFob(context.Background(), "DE:AD:BE:EF", "right")
	require.NoError(t, err)
	require.Equal(t, bernd, got.PlayerID)

	// The window is used up by the link.
	_, err = c.ResolveFob(context.Background(), "CA:FE:BA:BE", "right")
	require.ErrorIs(t, err, schmetterpause.ErrUnknownFob)
}

func TestTwoCheckinsProveAResultWithoutAnOperator(t *testing.T) {
	f, _, c := fake(t, ModeAccept)
	ctx := context.Background()

	home, err := c.ResolveFob(ctx, annaFob, "left")
	require.NoError(t, err)
	away, err := c.ResolveFob(ctx, berndFob, "right")
	require.NoError(t, err)

	accepted, err := c.Report(ctx, checkedIn(anna, bernd, home.CheckinID, away.CheckinID))
	require.NoError(t, err)
	require.Equal(t, "pending", accepted.Status)

	stored := f.Results()
	require.Len(t, stored, 1)
	require.Equal(t, "checkin", stored[0].EnteredVia)
	require.Empty(t, stored[0].Operator)

	// The same result again: the check-ins carry one already.
	_, err = c.Report(ctx, checkedIn(anna, bernd, home.CheckinID, away.CheckinID))
	require.ErrorIs(t, err, schmetterpause.ErrAlreadyReported)
	require.Len(t, f.Results(), 1)
}

func TestTheFakeRefusesCheckinsTheRealOneRefuses(t *testing.T) {
	f, _, c := fake(t, ModeAccept)
	ctx := context.Background()
	resolve := func(uid, half string) string {
		got, err := c.ResolveFob(ctx, uid, half)
		require.NoError(t, err)
		return got.CheckinID
	}
	annaLeft := resolve(annaFob, "left")
	berndRight := resolve(berndFob, "right")
	berndLeft := resolve(berndFob, "left")
	claraRight := resolve(claraFob, "right")

	withOperator := checkedIn(anna, bernd, annaLeft, berndRight)
	withOperator.OperatorID = olga

	for name, tc := range map[string]struct {
		result schmetterpause.Result
		reason string
	}{
		"an operator next to check-ins":  {withOperator, "400 Bad Request: send operator_id or home_checkin_id and away_checkin_id, not both"},
		"one check-in":                   {checkedIn(anna, bernd, annaLeft, ""), "400 Bad Request: home_checkin_id and away_checkin_id are both required"},
		"the same check-in twice":        {checkedIn(anna, bernd, annaLeft, annaLeft), "must differ"},
		"a check-in that does not exist": {checkedIn(anna, bernd, annaLeft, "99999999-9999-4999-8999-999999999999"), "422 Unprocessable Entity: no check-in with id"},
		"somebody else's check-in":       {checkedIn(anna, bernd, annaLeft, claraRight), "belongs to the wrong player"},
		"swapped":                        {checkedIn(bernd, anna, annaLeft, berndRight), "belongs to the wrong player"},
		"both at the same half":          {checkedIn(anna, bernd, annaLeft, berndLeft), "both check-ins are at the same half"},
	} {
		_, err := c.Report(ctx, tc.result)
		require.ErrorContains(t, err, tc.reason, name)
	}

	f.mu.Lock()
	f.now = func() time.Time { return time.Now().Add(CheckinValidity + time.Minute) }
	f.mu.Unlock()
	_, err := c.Report(ctx, checkedIn(anna, bernd, annaLeft, berndRight))
	require.ErrorContains(t, err, "has expired")
	require.Empty(t, f.Results())
}

func TestTheFakeNeverLogsAUID(t *testing.T) {
	var logs bytes.Buffer
	f, err := New("secret", ModeAccept, slog.New(slog.NewTextHandler(&logs, nil)))
	require.NoError(t, err)
	srv := httptest.NewServer(f.Handler())
	t.Cleanup(srv.Close)
	c, err := schmetterpause.New(schmetterpause.Config{BaseURL: srv.URL, Token: "secret"})
	require.NoError(t, err)

	_, _ = c.ResolveFob(context.Background(), annaFob, "left")
	_, _ = c.ResolveFob(context.Background(), "DE:AD:BE:EF", "left")
	_, _ = c.ResolveFob(context.Background(), "DE:AD:BE", "left")

	got := strings.ToLower(logs.String())
	require.NotEmpty(t, got)
	for _, uid := range []string{strings.ToLower(annaFob), "de:ad:be:ef", "de:ad:be"} {
		require.NotContains(t, got, uid)
	}
}
