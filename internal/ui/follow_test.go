package ui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
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
