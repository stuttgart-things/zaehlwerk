package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/checkin"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// lockedBuffer is a log both the handler and the desk write to.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// spStub answers the resolve route the way Schmetterpause does: 200 with a
// new check-in for a known fob, an empty 404 for an unknown one. down makes
// it answer 503.
type spStub struct {
	mu       sync.Mutex
	resolves int
	down     bool
}

func (s *spStub) handler(t *testing.T) http.HandlerFunc {
	owners := map[string]string{"04:A3:1B:22": "anna", "04:5C:7E:91": "bernd"}
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/players":
			_, _ = w.Write([]byte(`[{"id":"anna","display_name":"Anna"},{"id":"bernd","display_name":"Bernd"}]`))
		case "/api/identities/rfid/resolve":
			var body struct{ UID, Half string }
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			s.mu.Lock()
			defer s.mu.Unlock()
			s.resolves++
			if s.down {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			id, ok := owners[body.UID]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = fmt.Fprintf(w, `{"player_id":%q,"checkin_id":"%s-%d"}`, id, id, s.resolves)
		default:
			http.NotFound(w, r)
		}
	}
}

func (s *spStub) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolves
}

// checkinClient is a server with check-in on, against the stub, logging to
// logs.
func checkinClient(t *testing.T) (*client, *spStub, *lockedBuffer) {
	t.Helper()

	sp := &spStub{}
	srv := httptest.NewServer(sp.handler(t))
	t.Cleanup(srv.Close)
	spClient, err := schmetterpause.New(schmetterpause.Config{BaseURL: srv.URL, Token: "tok"})
	require.NoError(t, err)

	logs := &lockedBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	registry := match.NewRegistry()
	desk := checkin.NewDesk(spClient, checkin.WithLogger(log))
	return &client{
		t:        t,
		server:   New(registry, WithLogger(log), WithCheckin(desk, hubToken)),
		registry: registry,
		bearer:   hubToken,
	}, sp, logs
}

// hubToken is CHECKIN_TOKEN in these tests.
const hubToken = "hub-secret"

func checkinBody(eventID int, half, uid string) string {
	return fmt.Sprintf(`{"source":"hub-1","event_id":%d,"half":%q,"uid":%q}`, eventID, half, uid)
}

func (c *client) checkIn(status int, body string) checkinAnswer {
	c.t.Helper()
	rec, raw := c.do(http.MethodPost, "/ingest/checkin", body)
	require.Equalf(c.t, status, rec.Code, "%s", raw)
	var out checkinAnswer
	require.NoError(c.t, json.Unmarshal(raw, &out))
	return out
}

func TestTwoFobsStartAMatchThroughTheAPI(t *testing.T) {
	c, _, _ := checkinClient(t)

	a := c.checkIn(http.StatusOK, checkinBody(1, "left", "04:a3:1b:22"))
	require.Equal(t, checkin.Claimed, a.Outcome)
	require.Equal(t, "Anna", a.Left)

	_, raw := c.do(http.MethodGet, "/checkin", "")
	var status checkinStatus
	require.NoError(t, json.Unmarshal(raw, &status))
	require.True(t, status.Enabled)
	require.Equal(t, "Anna", status.Left.Name)
	require.Nil(t, status.Right)
	require.NotContains(t, string(raw), "anna-", "no check-in id on an open route")

	a = c.checkIn(http.StatusOK, checkinBody(2, "right", "04-5C-7E-91"))
	require.Equal(t, checkin.Started, a.Outcome)
	require.NotEmpty(t, a.MatchID)

	st := c.state(http.StatusOK, http.MethodGet, "/matches/current", "")
	require.Equal(t, a.MatchID, st.MatchID)
	require.Equal(t, [2]string{"Anna", "Bernd"}, st.Players)
	require.Equal(t, scorer.PhaseServeRally, st.Phase)

	m, err := c.registry.Get(a.MatchID)
	require.NoError(t, err)
	require.Equal(t, "anna", m.Handover.HomeID)
	require.Equal(t, "bernd", m.Handover.AwayID)
	require.Empty(t, m.Handover.OperatorID)
	require.True(t, m.Handover.CheckedIn())

	// While it runs, a fob is refused.
	a = c.checkIn(http.StatusConflict, checkinBody(3, "left", "04:a3:1b:22"))
	require.Equal(t, checkin.Refused, a.Outcome)
}

func TestARetriedCheckInRequestClaimsOnce(t *testing.T) {
	c, sp, _ := checkinClient(t)

	first := c.checkIn(http.StatusOK, checkinBody(7, "left", "04:a3:1b:22"))
	again := c.checkIn(http.StatusOK, checkinBody(7, "left", "04:a3:1b:22"))

	require.Equal(t, 1, sp.calls())
	require.Equal(t, first.Outcome, again.Outcome)
	require.True(t, again.Repeated)
}

func TestCheckInIsOffWithoutSchmetterpause(t *testing.T) {
	c := newClient(t)

	out := c.failure(http.StatusServiceUnavailable, http.MethodPost, "/ingest/checkin",
		checkinBody(1, "left", "04:a3:1b:22"))
	require.Contains(t, out.Error, "check-in is off")

	_, raw := c.do(http.MethodGet, "/checkin", "")
	require.JSONEq(t, `{"enabled":false,"left":null,"right":null}`, string(raw))

	// Scoring is what it was.
	id := c.newMatch(`{"players":["A","B"]}`)
	c.state(http.StatusOK, http.MethodPost, "/ingest/web",
		fmt.Sprintf(`{"match_id":%q,"source":"s","player":"a","event_id":1}`, id))
}

func TestAMalformedCheckInIsRefusedBeforeSchmetterpauseIsAsked(t *testing.T) {
	c, sp, _ := checkinClient(t)

	for _, body := range []string{
		checkinBody(1, "left", "04:A3:1B"),
		checkinBody(1, "middle", "04:A3:1B:22"),
		`{"event_id":1,"half":"left","uid":"04:A3:1B:22"}`,
		`{"source":"hub","uid":"04:A3:1B:22", "half":`,
	} {
		c.failure(http.StatusBadRequest, http.MethodPost, "/ingest/checkin", body)
	}
	require.Zero(t, sp.calls())
}

func TestAnOutageAnswers503AndLeavesTheHalfFree(t *testing.T) {
	c, sp, _ := checkinClient(t)
	sp.down = true

	a := c.checkIn(http.StatusServiceUnavailable, checkinBody(1, "left", "04:a3:1b:22"))
	require.Equal(t, checkin.Unavailable, a.Outcome)
	require.Empty(t, a.Left)
}

// The acceptance item of #92: an unknown UID appears in no log, no response
// body and no state. Every way a check-in with it can go is tried, and the
// UID is looked for in every spelling.
func TestAnUnknownUIDAppearsInNoLogNoAnswerAndNoState(t *testing.T) {
	c, sp, logs := checkinClient(t)
	const uid = "DE:AD:BE:EF"
	var bodies strings.Builder

	record := func(method, path, body string) {
		_, raw := c.do(method, path, body)
		bodies.Write(raw)
	}

	record(http.MethodPost, "/ingest/checkin", checkinBody(1, "left", uid))              // unknown
	record(http.MethodPost, "/ingest/checkin", checkinBody(1, "left", uid))              // its retry
	record(http.MethodPost, "/ingest/checkin", checkinBody(0, "left", uid))              // older id
	record(http.MethodPost, "/ingest/checkin", checkinBody(2, "left", "de-ad-be-ef-00")) // malformed
	record(http.MethodPost, "/ingest/checkin", `{"source":"hub-1","event_id":3,"half":"left","uid":["DE:AD:BE:EF"]}`)
	sp.down = true
	record(http.MethodPost, "/ingest/checkin", checkinBody(4, "right", uid)) // outage
	sp.down = false
	record(http.MethodPost, "/ingest/checkin", checkinBody(5, "left", "04:a3:1b:22"))
	record(http.MethodPost, "/ingest/checkin", checkinBody(6, "right", "04:5c:7e:91"))
	record(http.MethodPost, "/ingest/checkin", checkinBody(7, "right", uid)) // match running
	record(http.MethodGet, "/checkin", "")
	record(http.MethodGet, "/matches/current", "")

	haystack := strings.ToLower(logs.String() + bodies.String())
	require.NotEmpty(t, logs.String())
	for _, spelling := range []string{"de:ad:be:ef", "de-ad-be-ef", "de ad be ef", "deadbeef"} {
		require.NotContains(t, haystack, spelling, "the UID leaked")
	}
	for _, known := range []string{"04:a3:1b:22", "04a31b22", "04:5c:7e:91", "045c7e91"} {
		require.NotContains(t, haystack, known, "a known UID leaked")
	}
}

func TestACheckInWithoutTheHubsTokenIsRefusedBeforeItIsRead(t *testing.T) {
	c, sp, logs := checkinClient(t)

	for _, bearer := range []string{"", "hub-secre", "hub-secret-and-more", "HUB-SECRET"} {
		c.bearer = bearer
		rec, raw := c.do(http.MethodPost, "/ingest/checkin", checkinBody(1, "left", "04:a3:1b:22"))
		require.Equalf(t, http.StatusUnauthorized, rec.Code, "bearer %q: %s", bearer, raw)
		require.Contains(t, rec.Header().Get("WWW-Authenticate"), "Bearer")
		require.Contains(t, string(raw), "the hub's token")
	}

	// The token as anything but a bearer is no token.
	c.bearer = ""
	req := httptest.NewRequest(http.MethodPost, "/ingest/checkin", strings.NewReader(checkinBody(1, "left", "04:a3:1b:22")))
	req.Header.Set("Authorization", hubToken)
	rec := httptest.NewRecorder()
	c.server.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	require.Zero(t, sp.calls(), "Schmetterpause was never asked who the fob is")
	_, raw := c.do(http.MethodGet, "/checkin", "")
	require.Contains(t, string(raw), `"left":null`, "the half is still free")
	require.NotContains(t, logs.String(), hubToken)

	// The hub's own request still goes through, with the same event id: a
	// refused request was not counted.
	c.bearer = hubToken
	require.Equal(t, checkin.Claimed, c.checkIn(http.StatusOK, checkinBody(1, "left", "04:a3:1b:22")).Outcome)
}

func TestCheckInIsOffWithoutAToken(t *testing.T) {
	sp := &spStub{}
	srv := httptest.NewServer(sp.handler(t))
	t.Cleanup(srv.Close)
	spClient, err := schmetterpause.New(schmetterpause.Config{BaseURL: srv.URL, Token: "tok"})
	require.NoError(t, err)
	c := newClient(t)
	c.server = New(c.registry, WithCheckin(checkin.NewDesk(spClient), ""))

	out := c.failure(http.StatusServiceUnavailable, http.MethodPost, "/ingest/checkin",
		checkinBody(1, "left", "04:a3:1b:22"))
	require.Contains(t, out.Error, "check-in is off")
	_, raw := c.do(http.MethodGet, "/checkin", "")
	require.JSONEq(t, `{"enabled":false,"left":null,"right":null}`, string(raw))
	require.Zero(t, sp.calls())
}
