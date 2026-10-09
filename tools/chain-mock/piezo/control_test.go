package piezo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/api"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
)

func TestAPausedBoardScoresNothingUntilItIsResumed(t *testing.T) {
	zw := httptest.NewServer(api.New(match.NewRegistry(), api.WithLogger(quiet)))
	t.Cleanup(zw.Close)
	resp, err := http.Post(zw.URL+"/matches", "application/json", strings.NewReader(`{"players":["A","B"],"best_of":1}`))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	b := New(Config{API: zw.URL, Source: "piezo-test", Pace: 100 * time.Millisecond, Join: true, Matches: 1}, quiet)
	paused := b.Control()
	paused.Paused = true
	require.NoError(t, b.SetControl(paused))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()

	require.Eventually(t, func() bool { return b.Status().Doing == "paused" }, time.Second, 10*time.Millisecond)
	time.Sleep(300 * time.Millisecond)
	st, _, err := b.call(ctx, http.MethodGet, "/matches/current", nil)
	require.NoError(t, err)
	require.Equal(t, [2]int{0, 0}, st.Points, "nothing scored while paused")

	resumed := b.Control()
	resumed.Paused = false
	require.NoError(t, b.SetControl(resumed))
	require.Eventually(t, func() bool {
		st, _, _ := b.call(ctx, http.MethodGet, "/matches/current", nil)
		return st.Points != [2]int{0, 0}
	}, 2*time.Second, 20*time.Millisecond, "and scores again once resumed")
}

func TestTheControlRefusesKnobsOutOfRange(t *testing.T) {
	b := New(Config{Pace: time.Second}, quiet)

	require.Error(t, b.SetControl(Control{PaceMs: 50}))
	require.Error(t, b.SetControl(Control{PaceMs: 1000, Resend: 1.5}))
	require.Error(t, b.SetControl(Control{PaceMs: 1000, Undo: -0.1}))
	require.NoError(t, b.SetControl(Control{PaceMs: 1000, Ambiguous: 1}))
}

func TestTheControlEndpointTakesAWholeControl(t *testing.T) {
	b := New(Config{Pace: time.Second, Ambiguous: 0.1}, quiet)
	srv := httptest.NewServer(b.ControlHandler())
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/control", "application/json",
		strings.NewReader(`{"paused":true,"pace_ms":2500,"ambiguous":0,"resend":0.5,"undo":0}`))
	require.NoError(t, err)
	var st Status
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&st))
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, Control{Paused: true, PaceMs: 2500, Resend: 0.5}, st.Control)

	resp, err = http.Post(srv.URL+"/control", "application/json", strings.NewReader(`{"paused":false,"pase_ms":1}`))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "a misspelt knob is refused, not ignored")
	require.True(t, b.Control().Paused)
}
