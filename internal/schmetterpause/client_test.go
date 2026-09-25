package schmetterpause

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewIsOffWithoutABaseURL(t *testing.T) {
	for _, raw := range []string{"", "   "} {
		_, err := New(Config{BaseURL: raw})
		require.ErrorIs(t, err, ErrNotConfigured,
			"an unset base URL must mean the coupling is off, not a broken client")
	}
}

func TestPlayersAreFetchedWithTheToken(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"p1","display_name":"Anna","ttr":1000}]`))
	}))
	defer srv.Close()

	// A trailing slash on the base URL must not produce //api/players.
	c, err := New(Config{BaseURL: srv.URL + "/", Token: "tok"})
	require.NoError(t, err)

	players, err := c.Players(context.Background())
	require.NoError(t, err)
	require.Equal(t, "/api/players", gotPath)
	require.Equal(t, "Bearer tok", gotAuth)
	require.Equal(t, []Player{{ID: "p1", DisplayName: "Anna", TTR: 1000}}, players)
}

func TestReportSendsTheResultAndReadsTheAnswer(t *testing.T) {
	var body Result
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/results", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"match_id":"m-9","status":"pending"}`))
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, Token: "tok"})
	require.NoError(t, err)

	played := time.Date(2026, 9, 16, 7, 0, 0, 0, time.UTC)
	accepted, err := c.Report(context.Background(), Result{
		HomeID: "h", AwayID: "a", OperatorID: "o",
		Sets:        [][2]int{{11, 9}, {11, 7}},
		BestOf:      3,
		PointsToWin: 11,
		PlayedAt:    &played,
	})
	require.NoError(t, err)
	require.Equal(t, Accepted{MatchID: "m-9", Status: "pending"}, accepted)

	// The sets must arrive in the order they were played, home first — the
	// one transformation in which this handover could silently swap a result.
	require.Equal(t, [][2]int{{11, 9}, {11, 7}}, body.Sets)
	require.Equal(t, "o", body.OperatorID)
}

// TestARefusalCarriesSchmetterpausesOwnSentence is what makes a failure
// actionable at the table: "422" says nothing, "the operator may not be one of
// the two players" says what to change.
func TestARefusalCarriesSchmetterpausesOwnSentence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"the operator may not be one of the two players"}`))
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL})
	require.NoError(t, err)

	_, err = c.Report(context.Background(), Result{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "the operator may not be one of the two players")
	require.Contains(t, err.Error(), "422")
}

func TestPlayersFailsLoudlyOnARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"a bearer token is required"}`))
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL})
	require.NoError(t, err)

	_, err = c.Players(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "a bearer token is required")
}

func TestOperatorsAreFetchedWithTheToken(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"o1","display_name":"timoboll","observer":true},{"id":"p1","display_name":"Anna","observer":false}]`))
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, Token: "tok"})
	require.NoError(t, err)

	operators, err := c.Operators(context.Background())
	require.NoError(t, err)
	require.Equal(t, "/api/operators", gotPath)
	require.Equal(t, "Bearer tok", gotAuth)
	require.Equal(t, []Operator{
		{ID: "o1", DisplayName: "timoboll", Observer: true},
		{ID: "p1", DisplayName: "Anna"},
	}, operators)
}

// TestOperatorsFallBackToPlayersOnAnOlderSchmetterpause: before its ADR-0023
// there is no /api/operators, and the player list is what an operator was
// picked from.
func TestOperatorsFallBackToPlayersOnAnOlderSchmetterpause(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/api/operators" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"p1","display_name":"Anna","ttr":1000}]`))
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, Token: "tok"})
	require.NoError(t, err)

	operators, err := c.Operators(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"/api/operators", "/api/players"}, paths)
	require.Equal(t, []Operator{{ID: "p1", DisplayName: "Anna"}}, operators)
}

func TestOperatorsFailsLoudlyOnARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"a bearer token is required"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, Token: "wrong"})
	require.NoError(t, err)

	_, err = c.Operators(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "fetching operators")
}
