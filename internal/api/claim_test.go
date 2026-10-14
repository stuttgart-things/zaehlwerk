package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/checkin"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
)

// Players with Schmetterpause ids, which a phone claim carries. The fob
// players of the stub keep their short ids: the resolve answers them.
const (
	claraID = "33333333-3333-4333-8333-333333333333"
	doraID  = "55555555-5555-4555-8555-555555555555"
)

// spToken is SCHMETTERPAUSE_TOKEN in these tests: what this service sends to
// Schmetterpause, and what Schmetterpause sends back on a claim.
const spToken = "sp-secret"

// claimClient is a server with the phone path on and, when hub is set, the
// fob path too, on one desk.
func claimClient(t *testing.T, hub string) (*client, *spStub, *lockedBuffer) {
	t.Helper()

	sp := &spStub{}
	srv := httptest.NewServer(sp.handler(t))
	t.Cleanup(srv.Close)
	spClient, err := schmetterpause.New(schmetterpause.Config{BaseURL: srv.URL, Token: spToken})
	require.NoError(t, err)

	logs := &lockedBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	registry := match.NewRegistry()
	desk := checkin.NewDesk(spClient, checkin.WithLogger(log))
	return &client{
		t: t,
		server: New(registry, WithLogger(log),
			WithCheckin(desk, hub), WithPhoneCheckin(desk, spToken)),
		registry: registry,
	}, sp, logs
}

// checkinID is a phone check-in Schmetterpause recorded; n tells them apart.
func checkinID(n int) string {
	return fmt.Sprintf("cccccccc-0000-4000-8000-%012d", n)
}

func claimBody(half, player, checkin string) string {
	return fmt.Sprintf(`{"half":%q,"player_id":%q,"checkin_id":%q}`, half, player, checkin)
}

// claim is Schmetterpause calling in, with its token.
func (c *client) claim(status int, body string) checkinAnswer {
	c.t.Helper()
	c.bearer = spToken
	defer func() { c.bearer = "" }()
	rec, raw := c.do(http.MethodPost, "/checkin/claim", body)
	require.Equalf(c.t, status, rec.Code, "%s", raw)
	var out checkinAnswer
	require.NoError(c.t, json.Unmarshal(raw, &out))
	return out
}

func TestAPhoneClaimTakesAHalfExactlyLikeAFob(t *testing.T) {
	c, sp, _ := claimClient(t, hubToken)

	a := c.claim(http.StatusOK, claimBody("left", claraID, checkinID(1)))
	require.Equal(t, checkin.Claimed, a.Outcome)
	require.Equal(t, "Clara", a.Name, "looked up in the player list, as for a fob")
	require.Equal(t, "Clara", a.Left)
	require.Zero(t, sp.calls(), "nothing is resolved: Schmetterpause sent the ids")

	_, raw := c.do(http.MethodGet, "/checkin", "")
	var status checkinStatus
	require.NoError(t, json.Unmarshal(raw, &status))
	require.True(t, status.Enabled)
	require.True(t, status.Fob)
	require.True(t, status.Phone)
	require.Equal(t, "Clara", status.Left.Name)
	require.NotContains(t, string(raw), checkinID(1), "no check-in id on an open route")

	// The rules of the fob path: the same player again, another player, the
	// other half.
	require.Equal(t, checkin.Unchanged, c.claim(http.StatusOK, claimBody("left", claraID, checkinID(2))).Outcome)
	require.Equal(t, checkin.Moved, c.claim(http.StatusOK, claimBody("right", claraID, checkinID(3))).Outcome)
	a = c.claim(http.StatusOK, claimBody("right", doraID, checkinID(4)))
	require.Equal(t, checkin.Replaced, a.Outcome)
	require.Equal(t, "Dora", a.Right)
}

func TestOneHalfByFobAndOneByPhoneStartAMatchWithBothCheckInsAndNoOperator(t *testing.T) {
	c, _, _ := claimClient(t, hubToken)

	c.bearer = hubToken
	require.Equal(t, checkin.Claimed, c.checkIn(http.StatusOK, checkinBody(1, "left", "04:a3:1b:22")).Outcome)
	c.bearer = ""

	a := c.claim(http.StatusOK, claimBody("right", claraID, checkinID(1)))
	require.Equal(t, checkin.Started, a.Outcome)
	require.NotEmpty(t, a.MatchID)

	m, err := c.registry.Get(a.MatchID)
	require.NoError(t, err)
	require.Equal(t, match.Handover{
		HomeID: "anna", AwayID: claraID,
		HomeCheckinID: "anna-1", AwayCheckinID: checkinID(1),
	}, m.Handover)
	require.True(t, m.Handover.CheckedIn())
	require.Empty(t, m.Handover.OperatorID)
	require.Equal(t, [2]string{"Anna", "Clara"}, m.Scorer.State().Players)
}

func TestAPhoneClaimDuringARunningMatchIs409AndChangesNothing(t *testing.T) {
	c, _, _ := claimClient(t, "")
	id := c.newMatch(`{"players":["A","B"]}`)

	a := c.claim(http.StatusConflict, claimBody("right", doraID, checkinID(2)))
	require.Equal(t, checkin.Refused, a.Outcome)
	require.NotEmpty(t, a.Error)
	require.Empty(t, a.Left)
	require.Empty(t, a.Right, "the half was not claimed")

	st := c.state(http.StatusOK, http.MethodGet, "/matches/current", "")
	require.Equal(t, id, st.MatchID, "the running match is the running match")

	// Once it is over, the same claim is refused again, not taken: a
	// repeat of a check-in id is the first answer.
	m, err := c.registry.Get(id)
	require.NoError(t, err)
	c.server.EndMatch(m)
	again := c.claim(http.StatusConflict, claimBody("right", doraID, checkinID(2)))
	require.True(t, again.Repeated)
	require.Equal(t, checkin.Refused, again.Outcome)
}

func TestTheSameCheckinIDTwiceClaimsOnce(t *testing.T) {
	c, _, _ := claimClient(t, "")

	first := c.claim(http.StatusOK, claimBody("left", claraID, checkinID(1)))
	c.claim(http.StatusOK, claimBody("left", doraID, checkinID(2)))

	// A proxy retry of Clara's claim, after Dora took the half: the first
	// answer, and Dora keeps it.
	again := c.claim(http.StatusOK, claimBody("left", claraID, strings.ToUpper(checkinID(1))))
	require.True(t, again.Repeated)
	require.False(t, first.Repeated)
	require.Equal(t, first.Outcome, again.Outcome)
	require.Equal(t, "Clara", again.Left, "the first answer, as it was")

	_, raw := c.do(http.MethodGet, "/checkin", "")
	require.Contains(t, string(raw), `"name":"Dora"`, "the repeat claimed nothing")
}

func TestAMalformedClaimIs400AndClaimsNothing(t *testing.T) {
	c, _, _ := claimClient(t, "")

	for _, body := range []string{
		claimBody("middle", claraID, checkinID(1)),
		claimBody("", claraID, checkinID(1)),
		claimBody("left", "anna", checkinID(1)),
		claimBody("left", claraID, "not-a-uuid"),
		claimBody("left", claraID, ""),
		`{"half":"left","player_id":"` + claraID + `"}`,
		`{"half":"left",`,
		`["left"]`,
	} {
		c.bearer = spToken
		c.failure(http.StatusBadRequest, http.MethodPost, "/checkin/claim", body)
	}
	c.bearer = ""
	_, raw := c.do(http.MethodGet, "/checkin", "")
	require.Contains(t, string(raw), `"left":null`)
}

func TestAClaimWithoutSchmetterpausesTokenIs401BeforeItIsRead(t *testing.T) {
	c, _, logs := claimClient(t, hubToken)

	// The hub's token is the hub's: it does not claim for Schmetterpause.
	for _, bearer := range []string{"", "sp-secre", "sp-secret-and-more", "SP-SECRET", hubToken} {
		c.bearer = bearer
		for _, body := range []string{claimBody("left", claraID, checkinID(1)), `{"half":`} {
			rec, raw := c.do(http.MethodPost, "/checkin/claim", body)
			require.Equalf(t, http.StatusUnauthorized, rec.Code, "bearer %q: %s", bearer, raw)
			require.Contains(t, rec.Header().Get("WWW-Authenticate"), "Bearer")
			require.Contains(t, string(raw), "Schmetterpause's token")
		}
	}

	// And Schmetterpause's token does not check a fob in for the hub.
	c.bearer = spToken
	rec, _ := c.do(http.MethodPost, "/ingest/checkin", checkinBody(1, "left", "04:a3:1b:22"))
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	c.bearer = ""
	_, raw := c.do(http.MethodGet, "/checkin", "")
	require.Contains(t, string(raw), `"left":null`, "the half is still free")
	require.NotContains(t, logs.String(), spToken)

	// Refused requests were not remembered: the real one goes through.
	require.Equal(t, checkin.Claimed, c.claim(http.StatusOK, claimBody("left", claraID, checkinID(1))).Outcome)
}

// ADR-0011: the phone path needs Schmetterpause's token only. Without the
// hub's, the fob route answers exactly as it did before.
func TestThePhonePathWorksWithoutTheHubsToken(t *testing.T) {
	c, sp, _ := claimClient(t, "")

	a := c.claim(http.StatusOK, claimBody("left", claraID, checkinID(1)))
	require.Equal(t, checkin.Claimed, a.Outcome)
	a = c.claim(http.StatusOK, claimBody("right", doraID, checkinID(2)))
	require.Equal(t, checkin.Started, a.Outcome, "two phones start a match")

	for _, bearer := range []string{"", spToken, hubToken} {
		c.bearer = bearer
		out := c.failure(http.StatusServiceUnavailable, http.MethodPost, "/ingest/checkin",
			checkinBody(1, "left", "04:a3:1b:22"))
		require.Contains(t, out.Error, "check-in is off")
	}
	require.Zero(t, sp.calls(), "no fob was resolved")

	c.bearer = ""
	_, raw := c.do(http.MethodGet, "/checkin", "")
	var status checkinStatus
	require.NoError(t, json.Unmarshal(raw, &status))
	require.True(t, status.Enabled)
	require.False(t, status.Fob)
	require.True(t, status.Phone)
}

func TestTheClaimRouteIsOffWithoutSchmetterpausesToken(t *testing.T) {
	// The fob path alone, as before ADR-0011.
	c, _, _ := checkinClient(t)
	c.bearer = spToken
	out := c.failure(http.StatusServiceUnavailable, http.MethodPost, "/checkin/claim",
		claimBody("left", claraID, checkinID(1)))
	require.Contains(t, out.Error, "check-in is off")

	c.bearer = hubToken
	require.Equal(t, checkin.Claimed, c.checkIn(http.StatusOK, checkinBody(1, "left", "04:a3:1b:22")).Outcome)
	_, raw := c.do(http.MethodGet, "/checkin", "")
	require.NotContains(t, string(raw), `"phone"`)
}

func TestTheClaimRouteIsOffWithoutSchmetterpause(t *testing.T) {
	c := newClient(t)
	c.server = New(c.registry, WithPhoneCheckin(nil, spToken))
	c.bearer = spToken

	out := c.failure(http.StatusServiceUnavailable, http.MethodPost, "/checkin/claim",
		claimBody("left", claraID, checkinID(1)))
	require.Contains(t, out.Error, "check-in is off")
	_, raw := c.do(http.MethodGet, "/checkin", "")
	require.JSONEq(t, `{"enabled":false,"left":null,"right":null}`, string(raw))
}

// #114 holds for the phone path too, also on a desk that has no fob path: a
// real match started elsewhere drops a claim made by phone before it.
func TestAMatchStartedElsewhereDropsAPhoneClaim(t *testing.T) {
	c, _, _ := claimClient(t, "")

	c.claim(http.StatusOK, claimBody("left", claraID, checkinID(1)))
	id := c.newMatch(`{"players":["Carla","Dieter"]}`)
	_, raw := c.do(http.MethodGet, "/checkin", "")
	require.Contains(t, string(raw), `"left":null`)

	m, err := c.registry.Get(id)
	require.NoError(t, err)
	c.server.EndMatch(m)
	a := c.claim(http.StatusOK, claimBody("right", doraID, checkinID(2)))
	require.Equal(t, checkin.Claimed, a.Outcome, "no match in Clara's name")
	require.Empty(t, a.Left)
}
