package api

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// TestAButtonNamesItsHalfAndTheScorerFindsThePlayer — a button is mounted at a
// half of the table and knows nothing about who stands there (ADR-0005).
func TestAButtonNamesItsHalfAndTheScorerFindsThePlayer(t *testing.T) {
	c := newClient(t)
	c.newMatch(`{"players":["Anna","Bernd"]}`)

	st := c.state(http.StatusOK, http.MethodPost, "/ingest/button",
		`{"events":[{"source":"button-right","half":"right","event_id":1}]}`)
	require.Equal(t, [2]int{0, 1}, st.Points)
	require.Equal(t, scorer.PhasePlaying, st.Phase)
}

func TestAnIngestEventNamesAPlayerOrAHalfNotBoth(t *testing.T) {
	c := newClient(t)
	c.newMatch("")

	out := c.failure(http.StatusBadRequest, http.MethodPost, "/ingest/button",
		`{"events":[{"source":"button-left","player":"a","half":"left","event_id":1}]}`)
	require.Contains(t, out.Error, "not both")

	out = c.failure(http.StatusBadRequest, http.MethodPost, "/ingest/button",
		`{"events":[{"source":"button-left","half":"middle","event_id":1}]}`)
	require.Contains(t, out.Error, "half")
}

// TestARallyForTheFirstServeThroughTheAPI walks the phases the way a table
// does: warm-up, the serve rally, a set, the change of ends.
func TestARallyForTheFirstServeThroughTheAPI(t *testing.T) {
	c := newClient(t)
	st := c.state(http.StatusCreated, http.MethodPost, "/matches",
		`{"players":["Anna","Bernd"],"best_of":3,"start":"warmup","change_ends":true}`)
	require.Equal(t, scorer.PhaseWarmup, st.Phase)
	require.Equal(t, []scorer.Half{scorer.HalfLeft, scorer.HalfRight}, st.Waiting)

	press := func(half string, id int) scorer.State {
		return c.state(http.StatusOK, http.MethodPost, "/ingest/button",
			`{"events":[{"source":"button-`+half+`","half":"`+half+`","event_id":`+strconv.Itoa(id)+`}]}`)
	}

	press("left", 1)
	st = press("right", 1)
	require.Equal(t, scorer.PhaseServeRally, st.Phase)

	st = press("right", 2)
	require.Equal(t, scorer.PhasePlaying, st.Phase)
	require.Equal(t, scorer.PlayerB, st.Serving)

	for i := range 11 {
		st = press("left", 2+i)
	}
	require.Equal(t, scorer.PhaseChangeEnds, st.Phase)
	require.Equal(t, [2]scorer.Player{scorer.PlayerB, scorer.PlayerA}, st.Ends)
}

func TestFirstServerRallyAsksForAServeRally(t *testing.T) {
	c := newClient(t)

	st := c.state(http.StatusCreated, http.MethodPost, "/matches", `{"first_server":"rally"}`)
	require.Equal(t, scorer.PhaseServeRally, st.Phase)
	require.Empty(t, st.Serving)
}

// TestTheServerWideSetOpenerAppliesUnlessAMatchChooses — SET_OPENER is the
// table's house rule; a match may still ask for the other one.
func TestTheServerWideSetOpenerAppliesUnlessAMatchChooses(t *testing.T) {
	registry := match.NewRegistry()
	s := New(registry, WithSetOpener(scorer.OpenerLoser))

	m, err := s.StartMatch(scorer.Config{BestOf: 3}, match.Handover{})
	require.NoError(t, err)
	for i := range 11 {
		_, err := m.Scorer.Apply(scorer.ScoreEvent{MatchID: m.ID, Player: scorer.PlayerB, Delta: 1,
			Source: "web", EventID: uint64(i + 1)})
		require.NoError(t, err)
	}
	require.Equal(t, scorer.PlayerA, m.Scorer.State().Serving, "a lost set one and opens set two")

	m, err = s.StartMatch(scorer.Config{BestOf: 3, SetOpener: scorer.OpenerAlternate}, match.Handover{})
	require.NoError(t, err)
	for i := range 11 {
		_, err := m.Scorer.Apply(scorer.ScoreEvent{MatchID: m.ID, Player: scorer.PlayerB, Delta: 1,
			Source: "web", EventID: uint64(i + 1)})
		require.NoError(t, err)
	}
	require.Equal(t, scorer.PlayerB, m.Scorer.State().Serving, "the match chose the rulebook")
}
