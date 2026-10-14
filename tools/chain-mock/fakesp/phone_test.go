package fakesp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/api"
	"github.com/stuttgart-things/zaehlwerk/internal/checkin"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
)

const hubToken = "hub"

// table is the fake and a real zaehlwerk API with both check-in paths on,
// each pointed at the other: the fake claims at zaehlwerk, zaehlwerk resolves
// fobs and names at the fake. Neither side of the claim is a stub.
type table struct {
	fake     *Server
	sp       string
	zw       string
	client   *schmetterpause.Client
	registry *match.Registry
}

func newTable(t *testing.T) *table {
	t.Helper()

	// zaehlwerk's address first, so the fake knows it before it serves.
	zw := httptest.NewUnstartedServer(nil)
	f, err := New("secret", ModeAccept, quiet)
	require.NoError(t, err)
	f.Zaehlwerk = "http://" + zw.Listener.Addr().String()
	sp := httptest.NewServer(f.Handler())
	t.Cleanup(sp.Close)

	c, err := schmetterpause.New(schmetterpause.Config{BaseURL: sp.URL, Token: "secret", Timeout: time.Second})
	require.NoError(t, err)
	registry := match.NewRegistry()
	desk := checkin.NewDesk(c, checkin.WithLogger(quiet))
	zw.Config.Handler = api.New(registry, api.WithLogger(quiet),
		api.WithCheckin(desk, hubToken), api.WithPhoneCheckin(desk, "secret"))
	zw.Start()
	t.Cleanup(zw.Close)

	return &table{fake: f, sp: sp.URL, zw: zw.URL, client: c, registry: registry}
}

// phone presses the button on the fake's table page.
func (tb *table) phone(t *testing.T, form url.Values) (int, map[string]any) {
	t.Helper()
	resp, err := http.PostForm(tb.sp+"/phone", form)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out), string(raw))
	return resp.StatusCode, out
}

func TestAPhoneCheckInIsRecordedAndClaimedAtZaehlwerk(t *testing.T) {
	tb := newTable(t)

	status, out := tb.phone(t, url.Values{"player_id": {bernd}, "half": {"right"}})
	require.Equal(t, http.StatusOK, status, out)
	require.Equal(t, "claimed", out["outcome"])
	require.Equal(t, "Bernd", out["name"], "zaehlwerk read the name from the fake's player list")

	got := tb.fake.Checkins()
	require.Len(t, got, 1)
	require.Equal(t, MethodPhone, got[0].Method)
	require.Equal(t, "right", got[0].Half)
}

// The acceptance of #116 through both real ends: one half by fob, the other
// by phone, and the result carries both check-ins and no operator.
func TestOneHalfByFobAndOneByPhoneReportWithoutAnOperator(t *testing.T) {
	tb := newTable(t)

	req, err := http.NewRequest(http.MethodPost, tb.zw+"/ingest/checkin",
		strings.NewReader(`{"source":"hub","event_id":1,"half":"left","uid":"04:A3:1B:22"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+hubToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	status, out := tb.phone(t, url.Values{"player_id": {bernd}, "half": {"right"}})
	require.Equal(t, http.StatusOK, status, out)
	require.Equal(t, "started", out["outcome"])

	m, err := tb.registry.Get(out["match_id"].(string))
	require.NoError(t, err)
	require.True(t, m.Handover.CheckedIn())

	r := checkedIn(m.Handover.HomeID, m.Handover.AwayID, m.Handover.HomeCheckinID, m.Handover.AwayCheckinID)
	_, err = tb.client.Report(context.Background(), r)
	require.NoError(t, err, "the fake accepts the check-ins zaehlwerk was given")
	stored := tb.fake.Results()
	require.Len(t, stored, 1)
	require.Equal(t, "Anna", stored[0].Home)
	require.Equal(t, "Bernd", stored[0].Away)
	require.Equal(t, "checkin", stored[0].EnteredVia)

	methods := map[string]bool{}
	for _, c := range tb.fake.Checkins() {
		require.True(t, c.Used)
		methods[c.Method] = true
	}
	require.Equal(t, map[string]bool{MethodRFID: true, MethodPhone: true}, methods)
}

func TestTheSamePhoneCheckInSentTwiceIsARepeat(t *testing.T) {
	tb := newTable(t)

	_, first := tb.phone(t, url.Values{"player_id": {anna}, "half": {"left"}})
	id := tb.fake.Checkins()[0].ID.String()

	status, again := tb.phone(t, url.Values{"player_id": {anna}, "half": {"left"}, "checkin_id": {id}})
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, true, again["repeated"])
	require.Equal(t, first["outcome"], again["outcome"])
	require.Len(t, tb.fake.Checkins(), 1, "a repeat records nothing")
}

func TestAnObserverDoesNotCheckInByPhone(t *testing.T) {
	tb := newTable(t)
	status, _ := tb.phone(t, url.Values{"player_id": {olga}, "half": {"left"}})
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Empty(t, tb.fake.Checkins())
}

func TestWithoutZaehlwerkThereIsNoPhoneCheckIn(t *testing.T) {
	f, srv, _ := fake(t, ModeAccept)
	resp, err := http.PostForm(srv.URL+"/phone", url.Values{"player_id": {anna}, "half": {"left"}})
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Empty(t, f.Checkins())
}
