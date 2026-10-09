package buttons

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/api"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// zaehlwerk serves the JSON API with one match running, as somebody would have
// started it on the page.
func zaehlwerk(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(api.New(match.NewRegistry(), api.WithLogger(quiet)))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/matches", "application/json",
		strings.NewReader(`{"players":["Anna","Bernd"],"best_of":3}`))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	return srv
}

// rig is the buttons against zw, with a window short enough to wait out.
func rig(t *testing.T, zw *httptest.Server, change func(*Settings)) *Rig {
	t.Helper()
	s := DefaultSettings()
	s.BothWindow = 30 * time.Millisecond
	if change != nil {
		change(&s)
	}
	return New(zw.URL, "button-test", s, quiet)
}

func current(t *testing.T, r *Rig) scorer.State {
	t.Helper()
	st, ok, err := r.Current(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	return st
}

func points(t *testing.T, r *Rig) [2]int {
	t.Helper()
	return current(t, r).Points
}

func press(t *testing.T, r *Rig, side string, held time.Duration) {
	t.Helper()
	require.NoError(t, r.Press(context.Background(), side, held))
}

func TestAShortPressScoresForTheHalfItIsOn(t *testing.T) {
	r := rig(t, zaehlwerk(t), nil)

	press(t, r, "A", 100*time.Millisecond)
	press(t, r, "B", 100*time.Millisecond)
	press(t, r, "B", 100*time.Millisecond)

	require.Equal(t, [2]int{1, 2}, points(t, r))
}

func TestAPressJustUnderTheThresholdIsShortAndJustOverIsLong(t *testing.T) {
	r := rig(t, zaehlwerk(t), nil)

	press(t, r, "A", 999*time.Millisecond)
	press(t, r, "A", 999*time.Millisecond)
	require.Equal(t, [2]int{2, 0}, points(t, r))

	press(t, r, "A", time.Second)
	require.Eventually(t, func() bool { return points(t, r) == [2]int{1, 0} },
		time.Second, 5*time.Millisecond, "the long press takes one back once the window is over")
}

func TestALongPressWaitsOutTheWindowBeforeItTakesThePointBack(t *testing.T) {
	r := rig(t, zaehlwerk(t), func(s *Settings) { s.BothWindow = 200 * time.Millisecond })

	press(t, r, "A", 100*time.Millisecond)
	press(t, r, "A", 2*time.Second)

	require.Equal(t, [2]int{1, 0}, points(t, r), "nothing is taken back inside the window")
	require.Eventually(t, func() bool { return points(t, r) == [2]int{0, 0} }, time.Second, 5*time.Millisecond)
}

func TestBothLongIsNotAnUndo(t *testing.T) {
	r := rig(t, zaehlwerk(t), nil)

	press(t, r, "A", 100*time.Millisecond)
	require.NoError(t, r.PressBoth(context.Background(), 2*time.Second))

	time.Sleep(100 * time.Millisecond)
	require.Equal(t, [2]int{1, 0}, points(t, r))
	require.True(t, current(t, r).MatchID != "", "and with ending switched off the match runs on")
}

func TestBothLongEndsTheMatchWhenAskedTo(t *testing.T) {
	r := rig(t, zaehlwerk(t), func(s *Settings) { s.BothLongEndsMatch = true })

	require.NoError(t, r.PressBoth(context.Background(), 2*time.Second))

	_, ok, err := r.Current(context.Background())
	require.NoError(t, err)
	require.False(t, ok)
}

// A resend carries the same id, so it is safe whether the hub drops it or
// zaehlwerk does.
func TestALostRadioAckScoresOnceWithOrWithoutTheHub(t *testing.T) {
	for _, dedup := range []bool{true, false} {
		r := rig(t, zaehlwerk(t), func(s *Settings) { s.RadioAckLost, s.HubDedup = true, dedup })
		press(t, r, "A", 100*time.Millisecond)
		require.Equal(t, [2]int{1, 0}, points(t, r), "hub dedup %t", dedup)
	}
}

// Except for an undo: zaehlwerk cannot tell its retry from a second one, so
// only the hub stands between a lost ACK and two points taken back.
func TestALostRadioAckOnALongPressTakesTwoBackWithoutTheHub(t *testing.T) {
	r := rig(t, zaehlwerk(t), func(s *Settings) { s.RadioAckLost, s.HubDedup = true, false })

	press(t, r, "A", 100*time.Millisecond)
	press(t, r, "A", 100*time.Millisecond)
	press(t, r, "A", 2*time.Second)

	require.Eventually(t, func() bool { return points(t, r) == [2]int{0, 0} }, time.Second, 5*time.Millisecond)
}

// The gap the mock exists to show. When undo gets an event id in the ingest
// contract, this becomes a test that it takes back one.
func TestARetriedUndoTakesBackTwoPoints(t *testing.T) {
	r := rig(t, zaehlwerk(t), func(s *Settings) { s.APIResponseLost = true })

	press(t, r, "B", 100*time.Millisecond)
	press(t, r, "B", 100*time.Millisecond)
	require.Equal(t, [2]int{0, 2}, points(t, r), "a retried point is a duplicate and scores once")

	press(t, r, "B", 2*time.Second)
	require.Eventually(t, func() bool { return points(t, r) == [2]int{0, 0} }, time.Second, 5*time.Millisecond)
}

// No id downstream can tell a bounce from a second press.
func TestABounceWithoutDebounceScoresTwice(t *testing.T) {
	r := rig(t, zaehlwerk(t), func(s *Settings) { s.Debounce = false })

	press(t, r, "A", 100*time.Millisecond)

	require.Equal(t, [2]int{2, 0}, points(t, r))
}

func TestTheHalvesChangePlayersEverySet(t *testing.T) {
	require.Equal(t, scorer.PlayerA, playerOn("A", 1))
	require.Equal(t, scorer.PlayerB, playerOn("A", 2))
	require.Equal(t, scorer.PlayerB, playerOn("B", 1))
	require.Equal(t, scorer.PlayerA, playerOn("B", 2))
}

func TestThePageScoresAHeldPress(t *testing.T) {
	zw := zaehlwerk(t)
	r := rig(t, zw, nil)
	page := httptest.NewServer(NewServer(r, zw.URL+"/ui", quiet))
	t.Cleanup(page.Close)

	resp, err := http.Get(page.URL + "/")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(body), "Anna")

	resp, err = http.PostForm(page.URL+"/press", url.Values{"side": {"B"}, "held_ms": {"120"}})
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(body), "0:1")
	require.Contains(t, string(body), "POST /ingest/button")
}

func TestThePageTakesTheSettingsAsAWholeForm(t *testing.T) {
	zw := zaehlwerk(t)
	r := rig(t, zw, nil)
	page := httptest.NewServer(NewServer(r, zw.URL+"/ui", quiet))
	t.Cleanup(page.Close)

	resp, err := http.PostForm(page.URL+"/settings", url.Values{
		"long_ms": {"700"}, "window_ms": {"250"}, "radio_ack_lost": {"on"},
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got := r.Settings()
	require.Equal(t, 700*time.Millisecond, got.LongPress)
	require.Equal(t, 250*time.Millisecond, got.BothWindow)
	require.True(t, got.RadioAckLost)
	require.False(t, got.Debounce, "a box not ticked is not posted, and that means off")
}
