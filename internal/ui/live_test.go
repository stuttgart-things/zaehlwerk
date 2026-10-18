package ui

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// The big screen next to the table shows the match being played on it, like
// /ui/watch, and polls its own path for the next one.
func TestTheLiveScreenShowsTheRunningMatch(t *testing.T) {
	f := newFixture(t)
	m := f.match("Anna", "Bernd")
	f.point(m, "a")

	rec, body := f.do(http.MethodGet, "/ui/live", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	require.Contains(t, body, "<title>Zählwerk · live</title>")
	require.Contains(t, body, "Anna")
	require.Contains(t, body, `<div class="points">1</div>`)
	require.Contains(t, body, `sse-connect="/ui/matches/`+m.ID+`/stream?view=watch"`)
	require.Contains(t, body, `hx-get="/ui/live/next?match=`+m.ID+`"`)
	require.NotContains(t, body, "/ui/watch/", "every poll stays on the live screen")
	require.NotContains(t, body, "hx-post")
	require.NotContains(t, body, "<button")
}

func TestTheLiveScreenMovesOnToTheNextMatchByItsOwnPath(t *testing.T) {
	f := newFixture(t)
	first := f.match("Anna", "Bernd")
	f.do(http.MethodPost, "/ui/matches/"+first.ID+"/end", nil)
	f.match("Carla", "Dieter")

	rec, _ := f.do(http.MethodGet, "/ui/live/next?match="+first.ID, nil)

	require.Equal(t, "/ui/live", rec.Header().Get("HX-Redirect"))
}

// Even without check-in, where /ui/watch follows a demo, the live screen does
// not: a bot game is not the match at the table.
func TestTheLiveScreenNeverFollowsADemo(t *testing.T) {
	f := newFixture(t, WithScreensaver(pongBase, time.Minute))
	f.demo()

	_, body := f.do(http.MethodGet, "/ui/live", nil)
	require.NotContains(t, body, "Pong A")
	require.Contains(t, body, "No match is running")

	rec, _ := f.do(http.MethodGet, "/ui/live/next?match=", nil)
	require.Empty(t, rec.Header().Get("HX-Redirect"))

	_, body = f.do(http.MethodGet, "/ui/watch", nil)
	require.Contains(t, body, "Pong A", "the watch screen still does")
}

// The live screen keeps the side select up on an idle table rather than
// handing it to the screensaver, and its polls stay on its own path.
func TestTheLiveScreenHasNoScreensaver(t *testing.T) {
	c := &clock{now: time.Date(2026, 10, 15, 6, 0, 0, 0, time.UTC)}
	f := saverFixture(t, &desk{}, c)
	f.do(http.MethodGet, "/ui/live/sides", nil)
	c.add(time.Hour)

	_, body := f.do(http.MethodGet, "/ui/live", nil)
	require.Contains(t, body, "Pick your side")
	require.Contains(t, body, `hx-get="/ui/live/sides"`)
	require.NotContains(t, body, "<iframe")

	_, body = f.do(http.MethodGet, "/ui/live/sides", nil)
	require.NotContains(t, body, "<iframe")

	_, body = f.do(http.MethodGet, "/ui/watch/sides", nil)
	require.Contains(t, body, "<iframe", "the watch screen still takes it")
}

func TestTheLiveScreenMakesWayForTheMatchTheCheckInsStart(t *testing.T) {
	f := newFixture(t, WithSides(&desk{}, true, spBase))
	f.match("Anna", "Bernd")

	rec, _ := f.do(http.MethodGet, "/ui/live/sides", nil)

	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, "/ui/live", rec.Header().Get("HX-Redirect"))
}

// A spectator looking up mid-match reads what the next rally is worth from the
// card of the side that holds it.
func TestTheSpectatorViewSaysWhenASetOrTheMatchIsAPointAway(t *testing.T) {
	f := newFixture(t)
	m := f.match("Anna", "Bernd") // best of 3
	for range 10 {
		f.point(m, "a")
	}

	_, body := f.do(http.MethodGet, "/ui/live", nil)
	anna, bernd, _ := strings.Cut(body, "Bernd")
	require.Contains(t, anna, `<span class="state state-stake">set point</span>`)
	require.NotContains(t, bernd, `class="state state-stake"`)

	f.point(m, "a")
	for range 10 {
		f.point(m, "a")
	}
	_, body = f.do(http.MethodGet, "/ui/live", nil)
	require.Contains(t, body, `<span class="state state-stake">match point</span>`)

	f.point(m, "b")
	f.point(m, "b")
	_, body = f.do(http.MethodGet, "/ui/live", nil)
	require.Contains(t, body, "match point", "still a point away at 10:2")

	f.point(m, "a")
	_, body = f.do(http.MethodGet, "/ui/live?match="+m.ID, nil)
	require.Contains(t, body, "won")
	require.NotContains(t, body, `class="state state-stake"`, "nothing at stake once it is won")
}

// The sets so far, the winner's number bold, and the set in play after them.
func TestTheSpectatorViewShowsTheSetsSoFar(t *testing.T) {
	f := newFixture(t)
	m := f.match("Anna", "Bernd")
	for range 11 {
		f.point(m, "a")
	}
	for range 11 {
		f.point(m, "b")
	}
	f.point(m, "b")
	f.point(m, "a")
	f.point(m, "b")

	_, body := f.do(http.MethodGet, "/ui/live", nil)

	require.Contains(t, body, `<span class="chip"><b>11</b>:0</span><span class="chip">0:<b>11</b></span><span class="chip chip-now">1:2</span>`)
}

func TestTheSpectatorStreamCarriesTheStake(t *testing.T) {
	f := newStreamFixture(t)
	r, _ := f.connectTo("/ui/matches/" + f.match.ID + "/stream?view=watch")
	readEvent(t, r)

	var ev sseEvent
	for range 10 {
		f.point(scorer.PlayerB)
		ev = readEvent(t, r)
	}

	require.Contains(t, ev.data, "set point")
}
