package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

type client struct {
	t        *testing.T
	server   *Server
	registry *match.Registry
}

func newClient(t *testing.T, opts ...match.Option) *client {
	t.Helper()

	registry := match.NewRegistry(opts...)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := func() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) }

	return &client{
		t:        t,
		server:   New(registry, WithLogger(quiet), WithClock(now)),
		registry: registry,
	}
}

func (c *client) do(method, path, body string) (*httptest.ResponseRecorder, []byte) {
	c.t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	rec := httptest.NewRecorder()
	c.server.ServeHTTP(rec, req)
	return rec, rec.Body.Bytes()
}

// state requires the status and decodes the body as a match state.
func (c *client) state(status int, method, path, body string) scorer.State {
	c.t.Helper()

	rec, raw := c.do(method, path, body)
	require.Equalf(c.t, status, rec.Code, "%s %s: %s", method, path, raw)

	var st scorer.State
	require.NoError(c.t, json.Unmarshal(raw, &st))
	return st
}

// failure requires the status and returns the decoded error body.
func (c *client) failure(status int, method, path, body string) errorBody {
	c.t.Helper()

	rec, raw := c.do(method, path, body)
	require.Equalf(c.t, status, rec.Code, "%s %s: %s", method, path, raw)

	var out errorBody
	require.NoError(c.t, json.Unmarshal(raw, &out))
	require.NotEmpty(c.t, out.Error)
	return out
}

// newMatch creates a match and returns its id.
func (c *client) newMatch(body string) string {
	c.t.Helper()
	return c.state(http.StatusCreated, http.MethodPost, "/matches", body).MatchID
}

func TestHealthz(t *testing.T) {
	c := newClient(t)

	rec, raw := c.do(http.MethodGet, "/healthz", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t,
		`{"status":"ok","version":"dev","commit":"unknown","date":"unknown"}`,
		string(raw))
}

// TestHealthzReportsTheBuild covers the whole reason the endpoint grew fields:
// a pod that cannot name its own revision leaves every deployment question to
// a comparison of registry digests.
func TestHealthzReportsTheBuild(t *testing.T) {
	c := newClient(t)
	c.server = New(c.registry, WithBuildInfo(BuildInfo{
		Version: "v1.2.3", Commit: "abc1234", Date: "2026-09-08T10:00:00Z",
	}))

	rec, raw := c.do(http.MethodGet, "/healthz", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t,
		`{"status":"ok","version":"v1.2.3","commit":"abc1234","date":"2026-09-08T10:00:00Z"}`,
		string(raw))
}

// TestAnUnstampedBuildSaysDev is the case CI produces today. An -X flag with an
// empty value overwrites the variable rather than leaving it alone, so a build
// with no tag in reach arrives here blank, and a blank field in a health
// response reads as a broken endpoint rather than an unstamped binary.
func TestAnUnstampedBuildSaysDev(t *testing.T) {
	c := newClient(t)
	c.server = New(c.registry, WithBuildInfo(BuildInfo{}))

	_, raw := c.do(http.MethodGet, "/healthz", "")
	require.JSONEq(t,
		`{"status":"ok","version":"dev","commit":"unknown","date":"unknown"}`,
		string(raw))
}

func TestCreateMatch(t *testing.T) {
	c := newClient(t)

	rec, raw := c.do(http.MethodPost, "/matches",
		`{"players":["Anna","Bernd"],"best_of":3,"first_server":"b"}`)
	require.Equal(t, http.StatusCreated, rec.Code, string(raw))

	var st scorer.State
	require.NoError(t, json.Unmarshal(raw, &st))
	require.NotEmpty(t, st.MatchID)
	require.Equal(t, [2]string{"Anna", "Bernd"}, st.Players)
	require.Equal(t, scorer.PlayerB, st.Serving)
	require.Equal(t, 1, st.SetNumber)
	require.Equal(t, "/matches/"+st.MatchID, rec.Header().Get("Location"))
}

func TestCreateMatchWithoutABody(t *testing.T) {
	c := newClient(t)

	// The hardware path has no opinion about names or format, so an empty
	// request has to work.
	st := c.state(http.StatusCreated, http.MethodPost, "/matches", "")
	require.Equal(t, [2]string{"a", "b"}, st.Players)
	require.Equal(t, scorer.PlayerA, st.Serving)
}

func TestCreateMatchRejectsNonsense(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "not json", body: `{`},
		{name: "one player", body: `{"players":["Anna"]}`},
		{name: "three players", body: `{"players":["Anna","Bernd","Clara"]}`},
		{name: "even best of", body: `{"best_of":4}`},
		{name: "unknown first server", body: `{"first_server":"c"}`},
		{name: "set shorter than the lead", body: `{"points_per_set":1}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t)
			c.failure(http.StatusBadRequest, http.MethodPost, "/matches", tt.body)
		})
	}
}

func TestGetMatch(t *testing.T) {
	c := newClient(t)
	id := c.newMatch("")

	st := c.state(http.StatusOK, http.MethodGet, "/matches/"+id, "")
	require.Equal(t, id, st.MatchID)
	require.Equal(t, [2]int{0, 0}, st.Points)

	c.failure(http.StatusNotFound, http.MethodGet, "/matches/nope", "")
}

func TestRoutingRejectsTheWrongMethodAndPath(t *testing.T) {
	c := newClient(t)

	rec, _ := c.do(http.MethodGet, "/ingest/web", "")
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)

	rec, _ = c.do(http.MethodGet, "/nope", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
}
