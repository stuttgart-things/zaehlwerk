package ui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/live"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// A pong game started from the navigation is a match like any other, and the
// page that started it should not need a reload to show it.
func TestTheScoringPagePicksUpAMatchThatStartsElsewhere(t *testing.T) {
	f := newFixture(t)

	_, body := f.do(http.MethodGet, "/ui", nil)
	require.Contains(t, body, "No match is running")
	require.Contains(t, body, `hx-get="/ui/next?match="`)

	rec, _ := f.do(http.MethodGet, "/ui/next?match=", nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Empty(t, rec.Header().Get("HX-Redirect"), "still nothing to show")

	first := f.match("Anna", "Bernd")
	rec, _ = f.do(http.MethodGet, "/ui/next?match=", nil)
	require.Equal(t, "/ui", rec.Header().Get("HX-Redirect"))

	rec, _ = f.do(http.MethodGet, "/ui/next?match="+first.ID, nil)
	require.Empty(t, rec.Header().Get("HX-Redirect"), "the match on the page is the one running")

	f.do(http.MethodPost, "/ui/matches/"+first.ID+"/end", nil)
	f.match("Carla", "Dieter")
	rec, _ = f.do(http.MethodGet, "/ui/next?match="+first.ID, nil)
	require.Equal(t, "/ui", rec.Header().Get("HX-Redirect"), "a finished match gives way to the next")
}

func TestAScoringPagePinnedToAMatchStaysOnIt(t *testing.T) {
	f := newFixture(t)
	m := f.match("Anna", "Bernd")

	_, body := f.do(http.MethodGet, "/ui?match="+m.ID, nil)
	require.NotContains(t, body, "/ui/next")

	// Starting a match from the pinned page answers with the panel, which
	// carries the poll or not by the page's own address.
	req := httptest.NewRequest(http.MethodPost, "/ui/matches", nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Current-URL", "http://zaehlwerk.example.com/ui?match="+m.ID)
	require.False(t, following(req))
	req.Header.Set("HX-Current-URL", "http://zaehlwerk.example.com/ui")
	require.True(t, following(req))
}

// The grace period and a failed handover are when the page's undo and retry
// matter, and they are on this page only.
func TestAResultStillOnItsWayKeepsThePage(t *testing.T) {
	f := newFixture(t, WithSchmetterpause(roster(), &stubHandover{}))
	f.do(http.MethodPost, "/ui/matches", url.Values{
		"home_id": {"id-anna"}, "away_id": {"id-bernd"}, "operator_id": {"id-cem"},
	})
	reported, err := f.registry.Current()
	require.NoError(t, err)
	f.do(http.MethodPost, "/ui/matches/"+reported.ID+"/end", nil)
	f.match("Carla", "Dieter")

	rec, _ := f.do(http.MethodGet, "/ui/next?match="+reported.ID, nil)

	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Empty(t, rec.Header().Get("HX-Redirect"))
}

// The page asks for the next match when the table says one started, rather
// than finding out at its next poll; the poll stays behind it, slower.
func TestAFollowingPageReChecksWhenAMatchStarts(t *testing.T) {
	f := newFixture(t)

	for path, next := range map[string]string{
		"/ui":       "/ui/next",
		"/ui/watch": "/ui/watch/next",
		"/ui/live":  "/ui/live/next",
	} {
		_, body := f.do(http.MethodGet, path, nil)
		require.Contains(t, body, `<div hidden hx-ext="sse" sse-connect="/ui/table">`, path)
		require.Contains(t, body, `hx-get="`+next+`?match=" hx-trigger="sse:table, every 30s"`, path)
	}
}

func TestAPageOpenedWithAMatchDoesNotFollow(t *testing.T) {
	f := newFixture(t)
	m := f.match("Anna", "Bernd")

	for _, path := range []string{"/ui", "/ui/watch", "/ui/live"} {
		_, body := f.do(http.MethodGet, path+"?match="+m.ID, nil)
		require.NotContains(t, body, "/ui/table", path)
		require.NotContains(t, body, "sse:table", path)
	}
}

// The panel a match started from the page swaps in carries the same wiring,
// so the page keeps listening to the table after it.
func TestAMatchStartedOnTheFollowingPageKeepsItListening(t *testing.T) {
	f := newFixture(t)

	req := httptest.NewRequest(http.MethodPost, "/ui/matches", nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Current-URL", "http://zaehlwerk.example.com/ui")
	req.Form = url.Values{"player_a": {"Anna"}, "player_b": {"Bernd"}}
	rec := httptest.NewRecorder()
	f.server.ServeHTTP(rec, req)

	require.Contains(t, rec.Body.String(), `sse-connect="/ui/table"`)
	require.Contains(t, rec.Body.String(), `hx-trigger="sse:table, every 30s"`)
}

// With the side select on, the poll is the clock of the hold, and a check-in
// ends the hold on it; neither is on the table stream, so it stays quick.
func TestASideSelectScreenKeepsItsQuickPoll(t *testing.T) {
	f := newFixture(t, WithSides(&desk{}, true, ""))

	_, body := f.do(http.MethodGet, "/ui/watch", nil)

	require.Contains(t, body, `hx-trigger="sse:table, every 5s"`)
}

// Without a hub there is no stream to listen to, and the poll is all there is.
func TestWithoutLiveUpdatesTheFollowingPagePollsAsBefore(t *testing.T) {
	f := newFixture(t, WithHub(nil))

	_, body := f.do(http.MethodGet, "/ui", nil)

	require.NotContains(t, body, "/ui/table")
	require.Contains(t, body, `hx-get="/ui/next?match=" hx-trigger="every 5s"`)
}

// The stream says when to ask and nothing more, and only when a page might
// move: not for every point.
func TestTheTableTriggerSkipsPoints(t *testing.T) {
	f := newStreamFixture(t)
	r, cancel := f.connectTo("/ui/table")

	ev := readEvent(t, r)
	require.Equal(t, "table", ev.name)
	require.Equal(t, live.KindSnapshot, ev.data, "a match that started before the page connected is asked about at once")

	f.point(scorer.PlayerA)
	f.point(scorer.PlayerB)
	f.hub.Announce(live.Event{Kind: live.KindStarted, State: f.match.Scorer.State()})
	require.Equal(t, sseEvent{name: "table", data: live.KindStarted}, readEvent(t, r))

	// A win is where the side select's hold begins.
	f.hub.Observe(scorer.Transition{Kind: scorer.TransitionMatchWon, State: f.match.Scorer.State()})
	require.Equal(t, sseEvent{name: "table", data: string(scorer.TransitionMatchWon)}, readEvent(t, r))

	f.hub.Announce(live.Event{Kind: live.KindEnded, State: f.match.Scorer.State()})
	require.Equal(t, sseEvent{name: "table", data: live.KindEnded}, readEvent(t, r))

	cancel()
	require.Eventually(t, func() bool { return f.hub.TableSubscribers() == 0 },
		2*time.Second, 10*time.Millisecond, "a closed page leaves nothing in the hub")
}
