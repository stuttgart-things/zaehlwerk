package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// points sends n points for player a from source, ids from first on.
func (c *client) points(id, source string, first, n int) {
	c.t.Helper()
	for i := range n {
		c.state(http.StatusOK, http.MethodPost, "/ingest/web",
			fmt.Sprintf(`{"match_id":%q,"source":%q,"player":"a","event_id":%d}`, id, source, first+i))
	}
}

func TestAnUndoEventSentTwiceTakesBackOnePoint(t *testing.T) {
	c := newClient(t)
	c.newMatch("")

	c.state(http.StatusOK, http.MethodPost, "/ingest/button", `{"events":[
		{"source":"button-b","player":"b","event_id":1},
		{"source":"button-b","player":"b","event_id":2}]}`)

	undo := `{"events":[{"source":"button-b","player":"b","action":"undo","event_id":3}]}`
	st := c.state(http.StatusOK, http.MethodPost, "/ingest/button", undo)
	require.Equal(t, [2]int{0, 1}, st.Points)

	again := c.state(http.StatusOK, http.MethodPost, "/ingest/button", undo)
	require.Equal(t, st, again, "the retry is a duplicate and takes nothing back")
}

func TestAnUndoEventAfterASetPointReopensTheSet(t *testing.T) {
	c := newClient(t)
	id := c.newMatch("")
	c.points(id, "phone-1", 1, 11)
	require.Equal(t, 2, c.state(http.StatusOK, http.MethodGet, "/matches/"+id, "").SetNumber)

	st := c.state(http.StatusOK, http.MethodPost, "/ingest/web",
		fmt.Sprintf(`{"match_id":%q,"source":"phone-1","action":"undo","event_id":12}`, id))
	require.Equal(t, 1, st.SetNumber)
	require.Equal(t, [2]int{10, 0}, st.Points)
	require.Equal(t, [2]int{0, 0}, st.Sets)
}

func TestAnUndoInTheSameBurstTakesTheMatchPointBack(t *testing.T) {
	c := newClient(t)
	c.newMatch(`{"best_of":1}`)

	events := make([]string, 0, 13)
	for i := 1; i <= 12; i++ {
		events = append(events, fmt.Sprintf(`{"source":"button-a","player":"a","event_id":%d}`, i))
	}
	events = append(events, `{"source":"button-a","action":"undo","event_id":13}`)

	st := c.state(http.StatusOK, http.MethodPost, "/ingest/button",
		fmt.Sprintf(`{"events":[%s]}`, strings.Join(events, ",")))
	require.False(t, st.Complete, "the press after match point is dropped, the undo is not")
	require.Equal(t, [2]int{10, 0}, st.Points)
}

func TestAWonMatchTakesAnUndoEventThatNamesIt(t *testing.T) {
	c := newClient(t)
	id := c.newMatch(`{"best_of":1}`)
	c.points(id, "phone-1", 1, 11)

	out := c.failure(http.StatusConflict, http.MethodPost, "/ingest/web",
		fmt.Sprintf(`{"match_id":%q,"source":"phone-1","player":"a","event_id":12}`, id))
	require.True(t, out.State.Complete, "a point still finds the match over")

	st := c.state(http.StatusOK, http.MethodPost, "/ingest/web",
		fmt.Sprintf(`{"match_id":%q,"source":"phone-1","action":"undo","event_id":13}`, id))
	require.False(t, st.Complete)
	require.Equal(t, [2]int{10, 0}, st.Points)
}

func TestAMatchEndedByHandTakesNoUndoEvent(t *testing.T) {
	c := newClient(t)
	id := c.newMatch("")
	c.points(id, "phone-1", 1, 2)
	c.state(http.StatusOK, http.MethodPost, "/matches/"+id+"/end", "")

	out := c.failure(http.StatusConflict, http.MethodPost, "/ingest/web",
		fmt.Sprintf(`{"match_id":%q,"source":"phone-1","action":"undo","event_id":3}`, id))
	require.Equal(t, [2]int{2, 0}, out.State.Points)
}

// The piezo has to say its delta for a point; an undo has none to say.
func TestAPiezoUndoNeedsNoDelta(t *testing.T) {
	c := newClient(t)
	id := c.newMatch("")
	c.state(http.StatusOK, http.MethodPost, "/ingest/piezo",
		fmt.Sprintf(`{"match_id":%q,"source":"piezo-a","player":"a","delta":1,"event_id":1}`, id))

	st := c.state(http.StatusOK, http.MethodPost, "/ingest/piezo",
		fmt.Sprintf(`{"match_id":%q,"source":"piezo-a","action":"undo","event_id":2}`, id))
	require.Equal(t, [2]int{0, 0}, st.Points)
}

func TestIngestRefusesWhatIsNotAPointOrAnUndo(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"a negative delta", `"player":"a","delta":-1`, "delta -1"},
		{"a delta of two", `"player":"a","delta":2`, "delta 2"},
		{"an undo with a delta", `"action":"undo","delta":-1`, "an undo takes no delta"},
		{"an action that does not exist", `"action":"redo"`, `action "redo"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t)
			id := c.newMatch("")
			c.points(id, "phone-1", 1, 1)

			out := c.failure(http.StatusBadRequest, http.MethodPost, "/ingest/web",
				fmt.Sprintf(`{"match_id":%q,"source":"phone-1","event_id":2,%s}`, id, tt.body))
			require.Contains(t, out.Error, tt.want)

			st := c.state(http.StatusOK, http.MethodGet, "/matches/"+id, "")
			require.Equal(t, [2]int{1, 0}, st.Points)
		})
	}
}
