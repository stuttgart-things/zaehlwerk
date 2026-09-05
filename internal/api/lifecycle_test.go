package api

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// point scores one point through the web endpoint.
func (c *client) point(id string, p scorer.Player, eventID int) scorer.State {
	c.t.Helper()

	return c.state(http.StatusOK, http.MethodPost, "/ingest/web",
		fmt.Sprintf(`{"match_id":%q,"source":"phone-1","player":%q,"event_id":%d}`, id, p, eventID))
}

func TestUndo(t *testing.T) {
	c := newClient(t)
	id := c.newMatch("")

	c.point(id, scorer.PlayerA, 1)
	st := c.point(id, scorer.PlayerB, 2)
	require.Equal(t, [2]int{1, 1}, st.Points)

	st = c.state(http.StatusOK, http.MethodPost, "/matches/"+id+"/undo", "")
	require.Equal(t, [2]int{1, 0}, st.Points)

	st = c.state(http.StatusOK, http.MethodPost, "/matches/"+id+"/undo", "")
	require.Equal(t, [2]int{0, 0}, st.Points)
}

func TestUndoWithNothingToTakeBack(t *testing.T) {
	c := newClient(t)
	id := c.newMatch("")

	out := c.failure(http.StatusConflict, http.MethodPost, "/matches/"+id+"/undo", "")
	require.NotNil(t, out.State)
	require.Equal(t, [2]int{0, 0}, out.State.Points)

	c.failure(http.StatusNotFound, http.MethodPost, "/matches/nope/undo", "")
}

// TestUndoAfterTheMatchIsWon is the case undo exists for. A retry of the point
// that won it must not put it back, though — the watermark does not roll back.
func TestUndoAfterTheMatchIsWon(t *testing.T) {
	c := newClient(t)
	id := c.newMatch(`{"best_of":1}`)

	for i := 1; i <= 11; i++ {
		c.point(id, scorer.PlayerA, i)
	}
	require.True(t, c.state(http.StatusOK, http.MethodGet, "/matches/"+id, "").Complete)

	st := c.state(http.StatusOK, http.MethodPost, "/matches/"+id+"/undo", "")
	require.False(t, st.Complete)
	require.Equal(t, [2]int{10, 0}, st.Points)

	// The match point resent by a client that never saw the answer.
	st = c.point(id, scorer.PlayerA, 11)
	require.Equal(t, [2]int{10, 0}, st.Points)
	require.False(t, st.Complete)

	// A new event id is a new press, and that one counts.
	st = c.point(id, scorer.PlayerB, 12)
	require.Equal(t, [2]int{10, 1}, st.Points)
}

func TestEndMatch(t *testing.T) {
	c := newClient(t)
	id := c.newMatch("")

	c.point(id, scorer.PlayerA, 1)

	st := c.state(http.StatusOK, http.MethodPost, "/matches/"+id+"/end", "")
	require.Equal(t, [2]int{1, 0}, st.Points)
	require.False(t, st.Complete, "abandoned is not the same as won")

	// Idempotent.
	c.state(http.StatusOK, http.MethodPost, "/matches/"+id+"/end", "")

	// And no more points go in.
	out := c.failure(http.StatusConflict, http.MethodPost, "/ingest/web",
		fmt.Sprintf(`{"match_id":%q,"source":"phone-1","player":"a","event_id":2}`, id))
	require.NotNil(t, out.State)
	require.Equal(t, [2]int{1, 0}, out.State.Points)

	// The state is still readable afterwards — the result has to get to
	// Schmetterpause somehow.
	st = c.state(http.StatusOK, http.MethodGet, "/matches/"+id, "")
	require.Equal(t, [2]int{1, 0}, st.Points)

	c.failure(http.StatusNotFound, http.MethodPost, "/matches/nope/end", "")
}

// TestAWholeMatchThroughTheAPI walks the path the table actually takes: create,
// score to the end of a set, take a point back, and finish.
func TestAWholeMatchThroughTheAPI(t *testing.T) {
	c := newClient(t)
	id := c.newMatch(`{"players":["Anna","Bernd"],"best_of":3}`)

	event := 0
	score := func(p scorer.Player) scorer.State {
		event++
		return c.point(id, p, event)
	}

	// First set to Anna, 11:9, through a deuce-free finish.
	for range 9 {
		score(scorer.PlayerA)
		score(scorer.PlayerB)
	}
	st := score(scorer.PlayerA)
	require.Equal(t, [2]int{10, 9}, st.Points)
	require.Equal(t, scorer.PlayerB, st.Serving)

	st = score(scorer.PlayerA)
	require.Equal(t, [2]int{1, 0}, st.Sets)
	require.Equal(t, 2, st.SetNumber)
	require.Equal(t, [][2]int{{11, 9}}, st.CompletedSets)
	require.Equal(t, scorer.PlayerB, st.Serving, "b opens the second set")

	// That last one was the wrong button.
	st = c.state(http.StatusOK, http.MethodPost, "/matches/"+id+"/undo", "")
	require.Equal(t, [2]int{10, 9}, st.Points)
	require.Equal(t, 1, st.SetNumber)
	require.Empty(t, st.CompletedSets)
	require.Equal(t, scorer.PlayerB, st.Serving)

	// It really was Anna's point.
	score(scorer.PlayerA)
	require.Equal(t, 2, c.state(http.StatusOK, http.MethodGet, "/matches/"+id, "").SetNumber)

	// Second set to Anna, and the match with it.
	for range 11 {
		score(scorer.PlayerA)
	}

	st = c.state(http.StatusOK, http.MethodGet, "/matches/"+id, "")
	require.True(t, st.Complete)
	require.Equal(t, scorer.PlayerA, st.Winner)
	require.Equal(t, [2]int{2, 0}, st.Sets)
	require.Equal(t, [][2]int{{11, 9}, {11, 0}}, st.CompletedSets)
}
