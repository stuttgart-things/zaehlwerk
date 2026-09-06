package api

import (
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// recordingSwitcher stands in for the LED catcher so the wiring can be checked
// without one. The real switcher's own behaviour is tested in internal/panel.
type recordingSwitcher struct {
	mu       sync.Mutex
	started  []string
	released []string
}

func (r *recordingSwitcher) Start(matchID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = append(r.started, matchID)
}

func (r *recordingSwitcher) Release(matchID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.released = append(r.released, matchID)
}

func (r *recordingSwitcher) snapshot() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.started...), append([]string(nil), r.released...)
}

func newSwitchingClient(t *testing.T) (*client, *recordingSwitcher) {
	t.Helper()

	sw := &recordingSwitcher{}
	c := newClient(t)
	c.server = New(c.registry, WithLogger(c.server.log), WithClock(c.server.now), WithPanelSwitcher(sw))
	return c, sw
}

func TestCreatingAMatchTakesThePanel(t *testing.T) {
	c, sw := newSwitchingClient(t)

	st := c.state(http.StatusCreated, http.MethodPost, "/matches", `{"players":["Anna","Bernd"]}`)

	started, released := sw.snapshot()
	require.Equal(t, []string{st.MatchID}, started)
	require.Empty(t, released)
}

func TestEndingAMatchGivesThePanelBack(t *testing.T) {
	c, sw := newSwitchingClient(t)
	st := c.state(http.StatusCreated, http.MethodPost, "/matches", `{"players":["Anna","Bernd"]}`)

	c.state(http.StatusOK, http.MethodPost, "/matches/"+st.MatchID+"/end", "")

	started, released := sw.snapshot()
	require.Equal(t, []string{st.MatchID}, started)
	require.Equal(t, []string{st.MatchID}, released)
}

// Ending twice is allowed — the endpoint is idempotent — and must not confuse
// the panel either.
func TestEndingTwiceReleasesTwiceAndIsHarmless(t *testing.T) {
	c, sw := newSwitchingClient(t)
	st := c.state(http.StatusCreated, http.MethodPost, "/matches", `{"players":["Anna","Bernd"]}`)

	c.state(http.StatusOK, http.MethodPost, "/matches/"+st.MatchID+"/end", "")
	c.state(http.StatusOK, http.MethodPost, "/matches/"+st.MatchID+"/end", "")

	_, released := sw.snapshot()
	require.Equal(t, []string{st.MatchID, st.MatchID}, released)
}

// A create that the scorer rejects must not take the panel for a match that
// does not exist.
func TestAFailedCreateDoesNotTakeThePanel(t *testing.T) {
	c, sw := newSwitchingClient(t)

	rec, _ := c.do(http.MethodPost, "/matches", `{"players":["Anna","Bernd"],"best_of":4}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	started, _ := sw.snapshot()
	require.Empty(t, started)
}

func TestEndingAnUnknownMatchDoesNotTouchThePanel(t *testing.T) {
	c, sw := newSwitchingClient(t)

	rec, _ := c.do(http.MethodPost, "/matches/nope/end", "")
	require.Equal(t, http.StatusNotFound, rec.Code)

	started, released := sw.snapshot()
	require.Empty(t, started)
	require.Empty(t, released)
}

// Without a catcher configured the endpoints behave exactly as before.
func TestTheLifecycleWorksWithNoSwitcherConfigured(t *testing.T) {
	c := newClient(t)

	st := c.state(http.StatusCreated, http.MethodPost, "/matches", `{"players":["Anna","Bernd"]}`)
	c.state(http.StatusOK, http.MethodPost, "/matches/"+st.MatchID+"/end", "")
}

// The scorer observer is the other half: a match played to its final point
// releases the panel without anyone calling /end.
func TestAWonMatchReleasesThePanelThroughTheObserver(t *testing.T) {
	var mu sync.Mutex
	var released []string

	registry := match.NewRegistry(match.WithObserver(func(tr scorer.Transition) {
		if tr.Kind == scorer.TransitionMatchWon {
			mu.Lock()
			defer mu.Unlock()
			released = append(released, tr.State.MatchID)
		}
	}))

	m, err := registry.Create(scorer.Config{Players: [2]string{"Anna", "Bernd"}, BestOf: 1})
	require.NoError(t, err)

	for i := uint64(1); !m.Scorer.State().Complete; i++ {
		_, err := m.Scorer.Apply(scorer.ScoreEvent{
			MatchID: m.ID, Player: scorer.PlayerA, Delta: 1, EventID: i, Source: "test",
		})
		require.NoError(t, err)
		require.Less(t, i, uint64(50), "match did not finish")
	}

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{m.ID}, released)
}
