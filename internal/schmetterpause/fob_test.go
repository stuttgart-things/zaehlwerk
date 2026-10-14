package schmetterpause

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

const testUID = "04:A3:1B:22"

func fobClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, Token: "tok"})
	require.NoError(t, err)
	return c
}

func TestAFobIsResolvedWithTheTokenAndTheUIDInTheBody(t *testing.T) {
	c := fobClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/api/identities/rfid/resolve", r.URL.Path)
		require.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, map[string]string{"uid": testUID, "half": "left"}, body)
		_, _ = w.Write([]byte(`{"player_id":"p-1","checkin_id":"c-1"}`))
	})

	got, err := c.ResolveFob(context.Background(), testUID, "left")
	require.NoError(t, err)
	require.Equal(t, FobCheckin{PlayerID: "p-1", CheckinID: "c-1"}, got)
}

func TestAnEmpty404IsAnUnknownFob(t *testing.T) {
	c := fobClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := c.ResolveFob(context.Background(), testUID, "left")
	require.ErrorIs(t, err, ErrUnknownFob)
}

// The route is not registered on a Schmetterpause without SP_RFID_UID_KEY,
// and Go's mux answers that with a body of its own.
func TestA404WithTheMuxBodyIsAMissingRoute(t *testing.T) {
	c := fobClient(t, http.NotFound)
	_, err := c.ResolveFob(context.Background(), testUID, "left")
	require.ErrorIs(t, err, ErrNoFobRoute)
	require.NotErrorIs(t, err, ErrUnknownFob)
}

func TestARefusalThatQuotesTheUIDDoesNotPassItOn(t *testing.T) {
	c := fobClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"proxy saw uid 04-a3-1b-22"}`))
	})
	_, err := c.ResolveFob(context.Background(), testUID, "left")
	require.Error(t, err)
	require.NotContains(t, strings.ToLower(err.Error()), "a3")
	require.Contains(t, err.Error(), "502")
}

func TestARefusalWithoutTheUIDKeepsItsSentence(t *testing.T) {
	c := fobClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"a bearer token is required"}`))
	})
	_, err := c.ResolveFob(context.Background(), testUID, "left")
	require.ErrorContains(t, err, "a bearer token is required")
}

func TestACheckedInMatchIsReportedWithItsCheckinsAndNoOperator(t *testing.T) {
	var raw map[string]any
	registry, _, wg := reporterHarness(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &raw))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"match_id":"sp-9","status":"pending"}`))
	})

	m, _, err := registry.CreateFor(scorer.Config{BestOf: 3},
		match.Setup{Handover: match.Handover{HomeID: "h", AwayID: "a",
			HomeCheckinID: "ch", AwayCheckinID: "ca"}})
	require.NoError(t, err)

	playOut(t, m)
	wg.Wait()

	require.True(t, m.Report().Done)
	require.Equal(t, "ch", raw["home_checkin_id"])
	require.Equal(t, "ca", raw["away_checkin_id"])
	require.NotContains(t, raw, "operator_id", "schmetterpause refuses an operator next to check-ins")
}

func TestADropdownMatchIsReportedWithItsOperatorAndNoCheckins(t *testing.T) {
	var raw map[string]any
	registry, _, wg := reporterHarness(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&raw))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"match_id":"sp-8","status":"pending"}`))
	})

	m, _, err := registry.CreateFor(scorer.Config{BestOf: 3},
		match.Setup{Handover: match.Handover{HomeID: "h", AwayID: "a", OperatorID: "o"}})
	require.NoError(t, err)

	playOut(t, m)
	wg.Wait()

	require.Equal(t, "o", raw["operator_id"])
	require.NotContains(t, raw, "home_checkin_id")
	require.NotContains(t, raw, "away_checkin_id")
}

// A 409 is a check-in that already carries a result: the first attempt
// arrived and its answer got lost. Retrying forever would never succeed.
func TestA409OnACheckedInResultIsDelivered(t *testing.T) {
	registry, _, wg := reporterHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"a check-in already carries a result"}`))
	})

	m, _, err := registry.CreateFor(scorer.Config{BestOf: 3},
		match.Setup{Handover: match.Handover{HomeID: "h", AwayID: "a",
			HomeCheckinID: "ch", AwayCheckinID: "ca"}})
	require.NoError(t, err)

	playOut(t, m)
	wg.Wait()

	report := m.Report()
	require.True(t, report.Done)
	require.NoError(t, report.Err)
}

func TestA409OnAnOperatorResultIsStillAFailure(t *testing.T) {
	registry, _, wg := reporterHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"conflict"}`))
	})

	m, _, err := registry.CreateFor(scorer.Config{BestOf: 3},
		match.Setup{Handover: match.Handover{HomeID: "h", AwayID: "a", OperatorID: "o"}})
	require.NoError(t, err)

	playOut(t, m)
	wg.Wait()

	report := m.Report()
	require.False(t, report.Done)
	require.Error(t, report.Err)
}
