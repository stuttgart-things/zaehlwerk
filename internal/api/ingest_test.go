package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

func TestIngestWeb(t *testing.T) {
	c := newClient(t)
	id := c.newMatch(`{"players":["Anna","Bernd"]}`)

	st := c.state(http.StatusOK, http.MethodPost, "/ingest/web",
		fmt.Sprintf(`{"match_id":%q,"source":"phone-3f9a","player":"a","event_id":1}`, id))
	require.Equal(t, [2]int{1, 0}, st.Points)
	require.Equal(t, id, st.MatchID)

	st = c.state(http.StatusOK, http.MethodPost, "/ingest/web",
		fmt.Sprintf(`{"match_id":%q,"source":"phone-3f9a","player":"b","event_id":2}`, id))
	require.Equal(t, [2]int{1, 1}, st.Points)
}

// TestIngestWebNeedsTheMatchID — a browser knows which match it is scoring. A
// tab left open from yesterday must not find its way into today's match.
func TestIngestWebNeedsTheMatchID(t *testing.T) {
	c := newClient(t)
	c.newMatch("")

	out := c.failure(http.StatusBadRequest, http.MethodPost, "/ingest/web",
		`{"source":"phone-3f9a","player":"a","event_id":1}`)
	require.Contains(t, out.Error, "match_id")
}

// TestIngestButtonFindsTheRunningMatch — the ESP-NOW payload carries a source,
// a player, a delta and a counter, and nothing else. There is nowhere for a
// match id to come from, so the API resolves it.
func TestIngestButtonFindsTheRunningMatch(t *testing.T) {
	c := newClient(t)
	id := c.newMatch("")

	st := c.state(http.StatusOK, http.MethodPost, "/ingest/button",
		`{"events":[{"source":"button-a","player":"a","event_id":1}]}`)
	require.Equal(t, id, st.MatchID)
	require.Equal(t, [2]int{1, 0}, st.Points)
}

func TestIngestButtonWithAnExplicitMatchID(t *testing.T) {
	c := newClient(t)
	first := c.newMatch("")
	second := c.newMatch("")

	// Without an id it would be the newer match; naming one wins.
	st := c.state(http.StatusOK, http.MethodPost, "/ingest/button",
		fmt.Sprintf(`{"match_id":%q,"events":[{"source":"button-a","player":"a","event_id":1}]}`, first))
	require.Equal(t, first, st.MatchID)

	st = c.state(http.StatusOK, http.MethodGet, "/matches/"+second, "")
	require.Equal(t, [2]int{0, 0}, st.Points)
}

func TestIngestWithNoMatchRunning(t *testing.T) {
	c := newClient(t)

	c.failure(http.StatusNotFound, http.MethodPost, "/ingest/button",
		`{"events":[{"source":"button-a","player":"a","event_id":1}]}`)

	// A match that has been ended does not catch hardware points either.
	id := c.newMatch("")
	c.state(http.StatusOK, http.MethodPost, "/matches/"+id+"/end", "")
	c.failure(http.StatusNotFound, http.MethodPost, "/ingest/button",
		`{"events":[{"source":"button-a","player":"a","event_id":1}]}`)
}

func TestButtonBatchIsAppliedInOrder(t *testing.T) {
	c := newClient(t)
	id := c.newMatch("")

	// A burst the hub queued while a request was in flight.
	st := c.state(http.StatusOK, http.MethodPost, "/ingest/button", fmt.Sprintf(`{
		"match_id": %q,
		"events": [
			{"source":"button-a","player":"a","event_id":1},
			{"source":"button-b","player":"b","event_id":1},
			{"source":"button-a","player":"a","event_id":2},
			{"source":"button-a","player":"a","event_id":3}
		]
	}`, id))

	require.Equal(t, [2]int{3, 1}, st.Points)
	// Four points played, so service has changed twice: back to a.
	require.Equal(t, scorer.PlayerA, st.Serving)
}

// TestABadEventRejectsTheWholeBatch — half a burst on the scoreboard is worse
// than none of it, and the hub will retry the whole request anyway.
func TestABadEventRejectsTheWholeBatch(t *testing.T) {
	c := newClient(t)
	id := c.newMatch("")

	out := c.failure(http.StatusBadRequest, http.MethodPost, "/ingest/button", fmt.Sprintf(`{
		"match_id": %q,
		"events": [
			{"source":"button-a","player":"a","event_id":1},
			{"source":"button-a","player":"z","event_id":2}
		]
	}`, id))
	require.Contains(t, out.Error, "event 1")

	st := c.state(http.StatusOK, http.MethodGet, "/matches/"+id, "")
	require.Equal(t, [2]int{0, 0}, st.Points, "the good event in front of it was not applied either")
}

func TestIngestRejectsMalformedEvents(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
		want string
	}{
		{
			name: "not json",
			path: "/ingest/web",
			body: `{"match_id":`,
			want: "decoding",
		},
		{
			name: "batch that is not json",
			path: "/ingest/button",
			body: `{"events":[`,
			want: "decoding",
		},
		{
			name: "no events in the batch",
			path: "/ingest/button",
			body: `{"match_id":"{{id}}","events":[]}`,
			want: "no events",
		},
		{
			name: "missing source",
			path: "/ingest/web",
			body: `{"match_id":"{{id}}","player":"a","event_id":1}`,
			want: "source is required",
		},
		{
			name: "unknown player",
			path: "/ingest/web",
			body: `{"match_id":"{{id}}","source":"phone-1","player":"z","event_id":1}`,
			want: `player "z"`,
		},
		{
			name: "no player",
			path: "/ingest/web",
			body: `{"match_id":"{{id}}","source":"phone-1","event_id":1}`,
			want: "player",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t)
			id := c.newMatch("")

			body := strings.Replace(tt.body, "{{id}}", id, 1)

			out := c.failure(http.StatusBadRequest, http.MethodPost, tt.path, body)
			require.Contains(t, out.Error, tt.want)

			st := c.state(http.StatusOK, http.MethodGet, "/matches/"+id, "")
			require.Equal(t, [2]int{0, 0}, st.Points)
		})
	}
}

func TestIngestUnknownMatchID(t *testing.T) {
	c := newClient(t)
	c.newMatch("")

	c.failure(http.StatusNotFound, http.MethodPost, "/ingest/web",
		`{"match_id":"nope","source":"phone-1","player":"a","event_id":1}`)
}

// TestPiezoNeedsAnExplicitDelta — the sensor knows which side of the table it
// sits under but not whether the hit was a point. A missing delta defaulting to
// one would turn every knock into a point.
func TestPiezoNeedsAnExplicitDelta(t *testing.T) {
	c := newClient(t)
	id := c.newMatch("")

	out := c.failure(http.StatusBadRequest, http.MethodPost, "/ingest/piezo",
		fmt.Sprintf(`{"match_id":%q,"source":"piezo-a","player":"a","event_id":1}`, id))
	require.Contains(t, out.Error, "delta is required")
}

func TestPiezoHitThatIsNotAPoint(t *testing.T) {
	c := newClient(t)
	id := c.newMatch("")

	st := c.state(http.StatusOK, http.MethodPost, "/ingest/piezo",
		fmt.Sprintf(`{"match_id":%q,"source":"piezo-a","player":"a","delta":0,"event_id":1}`, id))
	require.Equal(t, [2]int{0, 0}, st.Points, "recorded, changes nothing")

	// Recorded means its id is used up: the retry of it is a duplicate.
	st = c.state(http.StatusOK, http.MethodPost, "/ingest/piezo",
		fmt.Sprintf(`{"match_id":%q,"source":"piezo-a","player":"a","delta":1,"event_id":1}`, id))
	require.Equal(t, [2]int{0, 0}, st.Points)

	st = c.state(http.StatusOK, http.MethodPost, "/ingest/piezo",
		fmt.Sprintf(`{"match_id":%q,"source":"piezo-a","player":"a","delta":1,"event_id":2}`, id))
	require.Equal(t, [2]int{1, 0}, st.Points)
}

// TestADuplicateGetsTwoHundredAndTheState — a retrying sender must be told to
// stop, not told to try harder (ADR-0002).
func TestADuplicateGetsTwoHundredAndTheState(t *testing.T) {
	c := newClient(t)
	id := c.newMatch("")

	body := fmt.Sprintf(`{"match_id":%q,"source":"phone-1","player":"a","event_id":7}`, id)

	st := c.state(http.StatusOK, http.MethodPost, "/ingest/web", body)
	require.Equal(t, [2]int{1, 0}, st.Points)

	for range 3 {
		again := c.state(http.StatusOK, http.MethodPost, "/ingest/web", body)
		require.Equal(t, st, again)
	}
}

func TestSourcesKeepTheirOwnCounters(t *testing.T) {
	c := newClient(t)
	id := c.newMatch("")

	st := c.state(http.StatusOK, http.MethodPost, "/ingest/button", fmt.Sprintf(`{
		"match_id": %q,
		"events": [
			{"source":"button-a","player":"a","event_id":500},
			{"source":"button-b","player":"b","event_id":1}
		]
	}`, id))
	require.Equal(t, [2]int{1, 1}, st.Points, "button-b starts its own count")
}

func TestIngestIntoAFinishedMatch(t *testing.T) {
	c := newClient(t)
	id := c.newMatch(`{"best_of":1}`)

	for i := 1; i <= 11; i++ {
		c.state(http.StatusOK, http.MethodPost, "/ingest/web",
			fmt.Sprintf(`{"match_id":%q,"source":"phone-1","player":"a","event_id":%d}`, id, i))
	}

	st := c.state(http.StatusOK, http.MethodGet, "/matches/"+id, "")
	require.True(t, st.Complete)
	require.Equal(t, scorer.PlayerA, st.Winner)

	out := c.failure(http.StatusConflict, http.MethodPost, "/ingest/web",
		fmt.Sprintf(`{"match_id":%q,"source":"phone-1","player":"b","event_id":12}`, id))
	require.NotNil(t, out.State, "a rejected point still answers with the score")
	require.Equal(t, [2]int{11, 0}, out.State.Points)
}

// TestABurstThatOutlivesTheMatch — the last press of a rally can arrive in the
// same batch as the one that won the match. The request did something, so it
// is not a failure; the trailing presses have nowhere to go.
func TestABurstThatOutlivesTheMatch(t *testing.T) {
	c := newClient(t)
	id := c.newMatch(`{"best_of":1}`)

	events := ""
	for i := 1; i <= 13; i++ {
		if i > 1 {
			events += ","
		}
		events += fmt.Sprintf(`{"source":"button-a","player":"a","event_id":%d}`, i)
	}

	st := c.state(http.StatusOK, http.MethodPost, "/ingest/button",
		fmt.Sprintf(`{"match_id":%q,"events":[%s]}`, id, events))
	require.True(t, st.Complete)
	require.Equal(t, [2]int{11, 0}, st.Points, "the two presses after match point were dropped")
}

func TestBodyTooLarge(t *testing.T) {
	c := newClient(t)
	id := c.newMatch("")

	huge := make([]byte, maxBodyBytes+1)
	for i := range huge {
		huge[i] = 'x'
	}

	c.failure(http.StatusBadRequest, http.MethodPost, "/ingest/web",
		fmt.Sprintf(`{"match_id":%q,"source":%q,"player":"a","event_id":1}`, id, huge))
}
