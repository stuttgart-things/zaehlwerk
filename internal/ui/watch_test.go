package ui

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// A screen on the wall is touched by whoever walks past it. Nothing on it may
// score, take a point back or end the match.
func TestTheSpectatorViewHasNothingToPress(t *testing.T) {
	f := newFixture(t)
	m := f.match("Anna", "Bernd")
	f.point(m, "a")

	rec, body := f.do(http.MethodGet, "/ui/watch", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	require.Contains(t, body, "Anna")
	require.Contains(t, body, `<div class="points">1</div>`)
	require.Contains(t, body, `sse-connect="/ui/matches/`+m.ID+`/stream?view=watch"`)
	require.NotContains(t, body, "hx-post")
	require.NotContains(t, body, "<button")
	require.NotContains(t, body, "<form")
}

func TestTheSpectatorViewWaitsWhenNothingIsRunning(t *testing.T) {
	f := newFixture(t)

	_, body := f.do(http.MethodGet, "/ui/watch", nil)

	require.Contains(t, body, "No match is running")
	require.Contains(t, body, "picks up the next one")
	require.NotContains(t, body, `sse-connect="/ui/matches/`, "no match to watch yet")
	require.Contains(t, body, `hx-get="/ui/watch/next?match="`)
}

// Set up once and left alone: the screen moves on when a different match
// starts, and leaves a finished one up until then.
func TestTheSpectatorViewMovesOnToTheNextMatch(t *testing.T) {
	f := newFixture(t)
	first := f.match("Anna", "Bernd")

	rec, _ := f.do(http.MethodGet, "/ui/watch/next?match="+first.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Empty(t, rec.Header().Get("HX-Redirect"), "still the match on the screen")

	f.do(http.MethodPost, "/ui/matches/"+first.ID+"/end", nil)
	rec, _ = f.do(http.MethodGet, "/ui/watch/next?match="+first.ID, nil)
	require.Empty(t, rec.Header().Get("HX-Redirect"), "a finished match stays up until the next starts")

	f.match("Carla", "Dieter")
	rec, _ = f.do(http.MethodGet, "/ui/watch/next?match="+first.ID, nil)
	require.Equal(t, "/ui/watch", rec.Header().Get("HX-Redirect"))

	rec, _ = f.do(http.MethodGet, "/ui/watch/next?match=", nil)
	require.Equal(t, "/ui/watch", rec.Header().Get("HX-Redirect"), "an empty screen picks up the first match")
}

func TestAPinnedSpectatorViewStaysOnItsMatch(t *testing.T) {
	f := newFixture(t)
	m := f.match("Anna", "Bernd")
	f.do(http.MethodPost, "/ui/matches/"+m.ID+"/end", nil)

	_, body := f.do(http.MethodGet, "/ui/watch?match="+m.ID, nil)

	require.Contains(t, body, "Anna")
	require.Contains(t, body, "ended")
	require.NotContains(t, body, "/ui/watch/next")
}

func TestTheSpectatorStreamCarriesTheScoreWithoutButtons(t *testing.T) {
	f := newStreamFixture(t)
	r, _ := f.connectTo("/ui/matches/" + f.match.ID + "/stream?view=watch")

	snapshot := readEvent(t, r)
	require.Equal(t, "spectator", snapshot.name)
	require.NotContains(t, snapshot.data, "<button")

	f.point(scorer.PlayerB)
	f.point(scorer.PlayerB)

	// No timeline in between: the next event after a point is the next score.
	require.Contains(t, readEvent(t, r).data, `<div class="points">1</div>`)
	ev := readEvent(t, r)
	require.Equal(t, "spectator", ev.name)
	require.Contains(t, ev.data, `<div class="points">2</div>`)
}

func TestTheSpectatorViewShowsTheMascotWhileItWaits(t *testing.T) {
	f := newFixture(t)

	_, body := f.do(http.MethodGet, "/ui/watch", nil)

	require.Contains(t, body, `<img class="brand-logo" src="/ui/static/mark-180.png"`)
	require.Regexp(t, `<div class="mascot" style="--paddle: #[0-9a-f]{6}"><svg id="mascot"`, body)
}
