package pong

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// A game on two devices (ADR-0009) is played here the way the two pages play
// it: the host through its token, the guest through the code and the token
// the join hands out, and the relay between them read as SSE.

type remoteFixture struct {
	*browserFixture
	code string
}

// clock is a settable time for the code's expiry.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// startRemote starts a game on two devices through POST /games and keeps the
// host's token and the code.
func startRemote(t *testing.T, p *pongFixture) *remoteFixture {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"driver": DriverBrowser, "mode": ModeRemote})
	require.NoError(t, err)
	resp, err := http.Post(p.url+"/games", "application/json", bytes.NewReader(raw))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var s started
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&s))
	require.NotEmpty(t, s.Token)
	require.Len(t, s.JoinCode, joinCodeDigits)
	return &remoteFixture{browserFixture: &browserFixture{pongFixture: p, id: s.ID, token: s.Token}, code: s.JoinCode}
}

// noRedirects keeps the 303 of a join, whose Location is the point.
var noRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}}

// join posts a code to /ui/join as the form does without htmx, and returns the
// status and where it sends the guest.
func (r *remoteFixture) join(code string) (int, string) {
	r.t.Helper()
	resp, err := noRedirects.PostForm(r.url+"/ui/join", url.Values{"code": {code}})
	require.NoError(r.t, err)
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, resp.Header.Get("Location")
}

// joinAsGuest joins with the code and returns the guest's token.
func (r *remoteFixture) joinAsGuest() string {
	r.t.Helper()
	status, location := r.join(r.code)
	require.Equal(r.t, http.StatusSeeOther, status)
	prefix := "/play/" + r.id + "#" + GuestTokenPrefix
	require.True(r.t, strings.HasPrefix(location, prefix), location)
	return strings.TrimPrefix(location, prefix)
}

// post sends a body to one of the game's routes with a token.
func (r *remoteFixture) post(path, token, body string) (int, string) {
	r.t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.url+"/games/"+r.id+path, strings.NewReader(body))
	require.NoError(r.t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set(TokenHeader, token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(r.t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(r.t, err)
	return resp.StatusCode, string(raw)
}

func (r *remoteFixture) drawCode(token string) (int, Code) {
	r.t.Helper()
	status, body := r.post("/code", token, "")
	var c Code
	_ = json.Unmarshal([]byte(body), &c)
	return status, c
}

type relayEvent struct {
	name, data string
}

// relayFor reads the relay for d and returns what came in that time.
func (r *remoteFixture) relayFor(d time.Duration) []relayEvent {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url+"/games/"+r.id+"/relay", nil)
	require.NoError(r.t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(r.t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(r.t, http.StatusOK, resp.StatusCode)
	require.Equal(r.t, "text/event-stream", resp.Header.Get("Content-Type"))

	var events []relayEvent
	var ev relayEvent
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			ev.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.data += strings.TrimPrefix(line, "data: ")
		case line == "" && ev.name != "":
			events = append(events, ev)
			ev = relayEvent{}
		}
	}
	return events
}

func TestAGuestJoinsWithTheCodeAndGetsItsOwnToken(t *testing.T) {
	r := startRemote(t, newPong(t, newTable(t).url, 1))
	require.Equal(t, &RemoteView{Joined: false, ExpiresAt: r.get(r.id).Remote.ExpiresAt}, r.get(r.id).Remote)

	guest := r.joinAsGuest()

	require.Len(t, guest, 32)
	require.NotEqual(t, r.token, guest)
	v := r.get(r.id)
	require.Equal(t, ModeRemote, v.Mode)
	require.Equal(t, [2]string{"Gastgeber", "Gast"}, v.Players)
	require.Equal(t, &RemoteView{Joined: true}, v.Remote)
}

func TestTheCodeIsNotInTheViewOrThePage(t *testing.T) {
	r := startRemote(t, newPong(t, newTable(t).url, 1))

	require.NotContains(t, mustGet(t, r.url+"/games/"+r.id), r.code)
	require.NotContains(t, mustGet(t, r.url+"/play/"+r.id), r.code)
	require.NotContains(t, mustGet(t, r.url+"/"), r.code)
}

func TestTheCodeIsUsedUpByTheFirstJoin(t *testing.T) {
	r := startRemote(t, newPong(t, newTable(t).url, 1))
	r.joinAsGuest()

	status, location := r.join(r.code)

	require.Equal(t, http.StatusConflict, status)
	require.Empty(t, location)
	status, _ = r.drawCode(r.token)
	require.Equal(t, http.StatusConflict, status, "no code joins once a guest has")
}

func TestTheCodeExpiresAfterFiveMinutes(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)
	c := &clock{now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	p.pong.now = c.Now
	r := startRemote(t, p)

	c.advance(joinCodeTTL - time.Second)
	require.False(t, r.get(r.id).Remote.ExpiresAt.Before(c.Now()), "still valid a second before")
	c.advance(time.Second)

	status, location := r.join(r.code)
	require.Equal(t, http.StatusConflict, status)
	require.Empty(t, location)

	status, fresh := r.drawCode(r.token)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, c.Now().Add(joinCodeTTL), fresh.ExpiresAt)
	r.code = fresh.Code
	r.joinAsGuest()
}

func TestFiveWrongCodesLockIt(t *testing.T) {
	r := startRemote(t, newPong(t, newTable(t).url, 1))
	wrong := "0000"
	if r.code == wrong {
		wrong = "1111"
	}

	for range joinCodeTries {
		status, _ := r.join(wrong)
		require.Equal(t, http.StatusForbidden, status)
	}
	require.True(t, r.get(r.id).Remote.Locked)

	status, location := r.join(r.code)
	require.Equal(t, http.StatusForbidden, status, "the right code no longer joins")
	require.Empty(t, location)
}

func TestTheHostDrawsANewCode(t *testing.T) {
	r := startRemote(t, newPong(t, newTable(t).url, 1))

	status, same := r.drawCode(r.token)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, r.code, same.Code, "a code that still joins is kept")

	for range joinCodeTries {
		r.join("x")
	}
	status, _ = r.drawCode("")
	require.Equal(t, http.StatusForbidden, status)

	status, fresh := r.drawCode(r.token)
	require.Equal(t, http.StatusOK, status)
	require.False(t, r.get(r.id).Remote.Locked)
	r.code = fresh.Code
	guest := r.joinAsGuest()

	status, _ = r.drawCode(guest)
	require.Equal(t, http.StatusForbidden, status, "only the host draws a code")
}

func TestTheGuestTokenCannotPressOrEnd(t *testing.T) {
	r := startRemote(t, newPong(t, newTable(t).url, 1))
	guest := r.joinAsGuest()

	status, _ := r.pressWith(guest, Press{Rally: 1, Kind: PressReady, Half: scorer.HalfRight})
	require.Equal(t, http.StatusForbidden, status)
	status, _ = r.post("/end", guest, "")
	require.Equal(t, http.StatusForbidden, status)
	status, _ = r.post("/frames", guest, `{"tick":1,"ball":null,"resting":null,"paddles":[1,2]}`)
	require.Equal(t, http.StatusForbidden, status, "nor send the host's frames")

	v := r.get(r.id)
	require.Equal(t, StatusPlaying, v.Status)
	require.Equal(t, []scorer.Half{scorer.HalfLeft, scorer.HalfRight}, v.State.Waiting, "nothing was pressed")
}

func TestOnlyTheGuestMovesItsPaddle(t *testing.T) {
	r := startRemote(t, newPong(t, newTable(t).url, 1))
	sample := `{"tick":1,"y":200,"ready":0}`

	status, _ := r.post("/paddle", r.token, sample)
	require.Equal(t, http.StatusForbidden, status, "not the host")
	status, _ = r.post("/paddle", "", sample)
	require.Equal(t, http.StatusForbidden, status, "not before anybody joined")

	guest := r.joinAsGuest()
	status, _ = r.post("/paddle", guest, sample)
	require.Equal(t, http.StatusNoContent, status)
}

func TestTheRelayKeepsOnlyTheLatestFrame(t *testing.T) {
	r := startRemote(t, newPong(t, newTable(t).url, 1))
	for _, tick := range []string{"1", "2", "3"} {
		status, body := r.post("/frames", r.token,
			`{"tick":`+tick+`,"ball":{"x":1,"y":2,"z":3},"resting":null,"paddles":[100,200],"fx":[700]}`)
		require.Equal(t, http.StatusNoContent, status, body)
	}

	events := r.relayFor(300 * time.Millisecond)

	require.Equal(t, []relayEvent{{"frame",
		`{"tick":3,"ball":{"x":1,"y":2,"z":3},"resting":null,"paddles":[100,200],"fx":[700]}`}}, events)
}

func TestTheRelayKeepsOnlyTheLatestPaddle(t *testing.T) {
	r := startRemote(t, newPong(t, newTable(t).url, 1))
	guest := r.joinAsGuest()
	for _, y := range []string{"10", "20", "30"} {
		status, body := r.post("/paddle", guest, "{\n  \"tick\": 7, \"y\": "+y+", \"ready\": 1\n}")
		require.Equal(t, http.StatusNoContent, status, body)
	}

	events := r.relayFor(300 * time.Millisecond)

	require.Equal(t, []relayEvent{{"guest", `{"tick":7,"y":30,"ready":1}`}}, events,
		"the latest only, on one line")
}

func TestARelayBodyOverTheCapIsRefused(t *testing.T) {
	r := startRemote(t, newPong(t, newTable(t).url, 1))
	guest := r.joinAsGuest()
	long := `{"tick":1,"ball":null,"resting":null,"paddles":[1,2],"say":"` + strings.Repeat("x", relayBodyCap) + `"}`

	status, _ := r.post("/frames", r.token, long)
	require.Equal(t, http.StatusRequestEntityTooLarge, status)
	status, _ = r.post("/paddle", guest, `{"tick":1,"y":2,"ready":0,"score":[11,0]}`)
	require.Equal(t, http.StatusBadRequest, status, "a field the shape does not have")
	status, _ = r.post("/paddle", guest, `{"tick":1,"y":2,"ready":0}{"tick":2}`)
	require.Equal(t, http.StatusBadRequest, status, "two values")

	require.Empty(t, r.relayFor(200*time.Millisecond), "nothing refused was relayed")
}

func TestTheRelayIsOnlyForAGameOnTwoDevices(t *testing.T) {
	b := startBrowser(t, newPong(t, newTable(t).url, 1), map[string]any{"mode": ModePvP})

	resp, err := http.Get(b.url + "/games/" + b.id + "/relay")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)

	r := &remoteFixture{browserFixture: b}
	status, _ := r.post("/frames", b.token, `{"tick":1,"ball":null,"resting":null,"paddles":[1,2]}`)
	require.Equal(t, http.StatusConflict, status)
	status, _ = r.drawCode(b.token)
	require.Equal(t, http.StatusConflict, status)
	status, _ = r.join("1234")
	require.Equal(t, http.StatusConflict, status, "no game waits for a guest")
}

// The guest's ready goes to the host over the relay, and the host presses it
// with its own token and its own press numbers: zaehlwerk, the real one in
// this process, sees one source and nothing from the guest.
func TestTheGuestsReadyReachesZaehlwerkOnlyThroughTheHost(t *testing.T) {
	tbl := newTable(t)
	rec := &recorder{next: tbl.zaehlwerk}
	proxy := httptest.NewServer(rec)
	t.Cleanup(proxy.Close)
	r := startRemote(t, newPong(t, proxy.URL, 1))
	guest := r.joinAsGuest()

	status, _ := r.post("/paddle", guest, `{"tick":12,"y":240,"ready":1}`)
	require.Equal(t, http.StatusNoContent, status)
	rec.mu.Lock()
	require.Empty(t, rec.sent, "the guest's ready did not reach zaehlwerk by itself")
	rec.mu.Unlock()

	events := r.relayFor(200 * time.Millisecond)
	require.Len(t, events, 1)
	var sample guestSample
	require.NoError(t, json.Unmarshal([]byte(events[0].data), &sample))
	require.EqualValues(t, 1, sample.Ready)

	// The host presses ready where zaehlwerk has the guest, player b.
	v := r.get(r.id)
	half := scorer.HalfLeft
	if v.State.Ends[1] == scorer.PlayerB {
		half = scorer.HalfRight
	}
	answer := r.press(Press{Rally: 1, Kind: PressReady, Half: half})
	require.NotContains(t, answer.State.Waiting, half)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Len(t, rec.sent, 1)
	require.Equal(t, "pong-"+r.id, rec.sent[0]["source"])
	require.Equal(t, string(half), rec.sent[0]["half"])
	m, err := tbl.registry.Get(v.MatchID)
	require.NoError(t, err)
	require.Equal(t, answer.State.Waiting, m.Scorer.State().Waiting)
}

func TestTheLobbyOffersTwoDevicesAndTheJoinPageAsksForTheCode(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)

	require.Contains(t, mustGet(t, p.url+"/"), `value="remote"`)
	page := mustGet(t, p.url+"/join")
	require.Contains(t, page, `hx-post="/ui/join"`)
	require.Contains(t, page, `name="code"`)

	r := startRemote(t, p)
	require.Contains(t, mustGet(t, p.url+"/"), `href="/join"`, "the lobby points a guest to the join page")
	r.joinAsGuest()
	require.NotContains(t, mustGet(t, p.url+"/"), `href="/join"`, "not once the guest is in")
}

func TestAJoinFromHtmxIsARedirectHeaderOrAMessage(t *testing.T) {
	r := startRemote(t, newPong(t, newTable(t).url, 1))
	post := func(code string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, r.url+"/ui/join", strings.NewReader(url.Values{"code": {code}}.Encode()))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		resp, err := noRedirects.Do(req)
		require.NoError(t, err)
		return resp
	}
	wrong := "0000"
	if r.code == wrong {
		wrong = "1111"
	}

	resp := post(wrong)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "htmx swaps a 200 only")
	require.Contains(t, string(body), "Der Code passt nicht.")

	resp = post(r.code)
	_ = resp.Body.Close()
	require.True(t, strings.HasPrefix(resp.Header.Get("HX-Redirect"), "/play/"+r.id+"#"+GuestTokenPrefix))
}
