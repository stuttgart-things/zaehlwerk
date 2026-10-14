package pong

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// A browser game is played here the way game.js plays it: read the phase from
// the answer, press ready for a half zaehlwerk waits for, report the winner's
// half of every rally. The JavaScript itself is not run in CI.

type browserFixture struct {
	*pongFixture
	id    string
	token string
}

// startBrowser starts a browser game through POST /games and keeps its token;
// a body naming a driver of its own starts that, a button check.
func startBrowser(t *testing.T, p *pongFixture, body map[string]any) *browserFixture {
	t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	if body["driver"] == nil {
		body["driver"] = DriverBrowser
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	resp, err := http.Post(p.url+"/games", "application/json", bytes.NewReader(raw))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var s started
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&s))
	require.NotEmpty(t, s.Token)
	return &browserFixture{pongFixture: p, id: s.ID, token: s.Token}
}

// press posts one press with the given token and returns the status and the
// answer.
func (b *browserFixture) pressWith(token string, p Press) (int, pressed) {
	b.t.Helper()
	raw, err := json.Marshal(p)
	require.NoError(b.t, err)
	req, err := http.NewRequest(http.MethodPost, b.url+"/games/"+b.id+"/presses", bytes.NewReader(raw))
	require.NoError(b.t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set(TokenHeader, token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(b.t, err)
	defer func() { _ = resp.Body.Close() }()
	var answer pressed
	_ = json.NewDecoder(resp.Body).Decode(&answer)
	return resp.StatusCode, answer
}

func (b *browserFixture) press(p Press) pressed {
	b.t.Helper()
	status, answer := b.pressWith(b.token, p)
	require.Equal(b.t, http.StatusOK, status, answer.Error)
	return answer
}

// playUntil plays presses the way the page does until done says stop, and
// returns the press number to go on with.
func (b *browserFixture) playUntil(rnd *rand.Rand, n uint64, done func(View) bool) (View, uint64) {
	b.t.Helper()
	v := b.get(b.id)
	for !done(v) && v.Status == StatusPlaying {
		st := v.State
		var p Press
		switch st.Phase {
		case scorer.PhaseWarmup, scorer.PhaseChangeEnds:
			p = Press{Rally: n, Kind: PressReady, Half: st.Waiting[0]}
		default:
			half := scorer.HalfLeft
			if rnd.Intn(2) == 1 {
				half = scorer.HalfRight
			}
			p = Press{Rally: n, Kind: PressRally, Half: half}
		}
		v = b.press(p).View
		n++
		require.Less(b.t, n, uint64(1000), "the match does not end")
	}
	return v, n
}

func TestABrowserGamePlaysToTheEndThroughItsPresses(t *testing.T) {
	b := startBrowser(t, newPong(t, newTable(t).url, 1), map[string]any{"mode": ModePvP})

	b.playUntil(rand.New(rand.NewSource(1)), 1, func(View) bool { return false })
	v := b.finished(b.id)

	require.Equal(t, StatusWon, v.Status, v.Reason)
	require.Equal(t, DriverBrowser, v.Driver)
	require.Equal(t, [2]string{"Links", "Rechts"}, v.Players)
	require.Empty(t, v.Divergences)
	require.Equal(t, v.Truth, v.Zaehlwerk)
	require.NotEmpty(t, v.EndsChanges, "the browser's presses went through the changes of ends")
}

func TestARallyNumberSentTwiceIsPressedOnce(t *testing.T) {
	tbl := newTable(t)
	rec := &recorder{next: tbl.zaehlwerk}
	proxy := httptest.NewServer(rec)
	t.Cleanup(proxy.Close)
	b := startBrowser(t, newPong(t, proxy.URL, 1), nil)

	first := b.press(Press{Rally: 1, Kind: PressReady, Half: scorer.HalfLeft})
	again := b.press(Press{Rally: 1, Kind: PressReady, Half: scorer.HalfLeft})

	require.Equal(t, first.State, again.State)
	require.Equal(t, []scorer.Half{scorer.HalfRight}, again.State.Waiting)
	require.EqualValues(t, 1, again.LastPress)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Len(t, rec.sent, 1, "one press reached zaehlwerk")
}

func TestASecondTabWithoutTheTokenMayWatchButNotPress(t *testing.T) {
	b := startBrowser(t, newPong(t, newTable(t).url, 1), nil)
	ready := Press{Rally: 1, Kind: PressReady, Half: scorer.HalfLeft}

	status, _ := b.pressWith("", ready)
	require.Equal(t, http.StatusForbidden, status)
	status, _ = b.pressWith(strings.Repeat("0", len(b.token)), ready)
	require.Equal(t, http.StatusForbidden, status)

	page, err := http.Get(b.url + "/play/" + b.id)
	require.NoError(t, err)
	defer func() { _ = page.Body.Close() }()
	require.Equal(t, http.StatusOK, page.StatusCode)
	html, err := io.ReadAll(page.Body)
	require.NoError(t, err)
	require.Contains(t, string(html), "<canvas")
	require.NotContains(t, string(html), b.token, "the page does not hand out the token")
	require.NotContains(t, mustGet(t, b.url+"/games/"+b.id), b.token)

	require.Equal(t, []scorer.Half{scorer.HalfLeft, scorer.HalfRight}, b.get(b.id).State.Waiting,
		"nothing was pressed")
}

func TestFaultsApplyToPressesFromTheBrowser(t *testing.T) {
	b := startBrowser(t, newPong(t, newTable(t).url, 1), map[string]any{"faults": Faults{Bounce: 1}})
	rnd := rand.New(rand.NewSource(1))

	v, n := b.playUntil(rnd, 1, func(v View) bool { return v.State.Phase == scorer.PhasePlaying })
	before := Awarded(v.State)
	v = b.press(Press{Rally: n, Kind: PressRally, Half: scorer.HalfLeft}).View

	require.Equal(t, before[0]+before[1]+2, v.Zaehlwerk[0]+v.Zaehlwerk[1], "the rally and its bounce")
	require.Equal(t, FaultBounce, v.Faults[len(v.Faults)-1].Kind)
	require.Zero(t, v.Unexplained, "%+v", v.Divergences)
	require.Equal(t, [2]int{v.Truth[0] + v.Explained[0], v.Truth[1] + v.Explained[1]}, v.Zaehlwerk)
}

func TestALostPressFromTheBrowserNeverReachesZaehlwerk(t *testing.T) {
	b := startBrowser(t, newPong(t, newTable(t).url, 1), map[string]any{"faults": Faults{Lost: 1}})

	_, n := b.playUntil(rand.New(rand.NewSource(1)), 1, func(v View) bool { return v.State.Phase == scorer.PhasePlaying })
	v := b.press(Press{Rally: n, Kind: PressRally, Half: scorer.HalfRight}).View

	require.Equal(t, [2]int{0, 0}, v.Zaehlwerk)
	require.Equal(t, 1, v.Truth[0]+v.Truth[1])
	require.Equal(t, FaultLost, v.Faults[len(v.Faults)-1].Kind)
	require.Zero(t, v.Unexplained)
}

func TestAPressThatDoesNotFitThePhaseIsRefusedAndItsNumberStaysFree(t *testing.T) {
	b := startBrowser(t, newPong(t, newTable(t).url, 1), nil)

	status, answer := b.pressWith(b.token, Press{Rally: 1, Kind: PressRally, Half: scorer.HalfLeft})
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, scorer.PhaseWarmup, answer.State.Phase, "the answer says where zaehlwerk is")

	v := b.press(Press{Rally: 1, Kind: PressReady, Half: scorer.HalfLeft}).View
	require.EqualValues(t, 1, v.LastPress)
}

func TestAPressNumberThatSkipsOneIsRefused(t *testing.T) {
	b := startBrowser(t, newPong(t, newTable(t).url, 1), nil)

	status, answer := b.pressWith(b.token, Press{Rally: 3, Kind: PressReady, Half: scorer.HalfLeft})
	require.Equal(t, http.StatusConflict, status)
	require.Contains(t, answer.Error, "want 1")
}

func TestTheEventStreamCarriesPhaseAndEnds(t *testing.T) {
	b := startBrowser(t, newPong(t, newTable(t).url, 1), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url+"/games/"+b.id+"/events", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	lines := bufio.NewScanner(resp.Body)
	lines.Buffer(make([]byte, 0, 64<<10), 1<<20)
	next := func() View {
		for lines.Scan() {
			if data, ok := strings.CutPrefix(lines.Text(), "data: "); ok {
				var v View
				require.NoError(t, json.Unmarshal([]byte(data), &v))
				return v
			}
		}
		t.Fatalf("the stream ended: %v", lines.Err())
		return View{}
	}

	first := next()
	require.Equal(t, scorer.PhaseWarmup, first.State.Phase)
	require.Equal(t, [2]scorer.Player{scorer.PlayerA, scorer.PlayerB}, first.State.Ends)
	require.Equal(t, []scorer.Half{scorer.HalfLeft, scorer.HalfRight}, first.State.Waiting)

	b.press(Press{Rally: 1, Kind: PressReady, Half: scorer.HalfLeft})
	b.press(Press{Rally: 2, Kind: PressReady, Half: scorer.HalfRight})
	for v := next(); v.State.Phase != scorer.PhaseServeRally; v = next() {
		require.Equal(t, scorer.PhaseWarmup, v.State.Phase)
	}
}

func TestThePageThatEndsItsGameEndsItsMatch(t *testing.T) {
	tbl := newTable(t)
	b := startBrowser(t, newPong(t, tbl.url, 1), nil)

	req, err := http.NewRequest(http.MethodPost, b.url+"/games/"+b.id+"/end", nil)
	require.NoError(t, err)
	req.Header.Set(TokenHeader, b.token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	v := b.finished(b.id)
	require.Equal(t, StatusEnded, v.Status)
	require.Equal(t, errStopped.Error(), v.Reason)
	m, err := tbl.registry.Get(v.MatchID)
	require.NoError(t, err)
	require.False(t, m.Running())
}

func TestABrowserGameNobodyPressesInEndsItsMatch(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)
	p.pong.idle = 50 * time.Millisecond
	b := startBrowser(t, p, nil)

	v := b.finished(b.id)
	require.Equal(t, StatusEnded, v.Status)
	require.Contains(t, v.Reason, "nobody pressed")
	m, err := tbl.registry.Get(v.MatchID)
	require.NoError(t, err)
	require.False(t, m.Running(), "the panel is not left with a match nobody plays")
}

func TestTheLobbyStartsABrowserGameAndHandsTheTokenOverInTheFragment(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)
	form := url.Values{"mode": {ModePvC}, "name_a": {"Anna"},
		"bot_b_reaction": {"200"}, "bot_b_speed": {"1.5"}, "bot_b_aim": {"0.5"}, "fault_lost": {"0,1"}}
	req, err := http.NewRequest(http.MethodPost, p.url+"/ui/games", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	target, fragment, ok := strings.Cut(resp.Header.Get("HX-Redirect"), "#")
	require.True(t, ok, "the token travels in the fragment, which no server sees")
	id := strings.TrimPrefix(target, "/play/")
	v := p.get(id)
	require.Equal(t, ModePvC, v.Mode)
	require.Equal(t, [2]string{"Anna", "Computer"}, v.Players)
	require.Nil(t, v.Bots[0], "player a is the person")
	require.Equal(t, BotParams{ReactionMS: 200, Speed: 1.5, Aim: 0.5}, *v.Bots[1])
	require.Equal(t, 0.1, v.FaultShares.Lost)

	b := &browserFixture{pongFixture: p, id: id, token: fragment}
	b.press(Press{Rally: 1, Kind: PressReady, Half: scorer.HalfLeft})
}

func TestTheLobbySaysWhyAStartWasRefused(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)
	resp, err := http.PostForm(p.url+"/ui/games", url.Values{"mode": {"solo"}})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "htmx swaps only a 2xx")
	require.Contains(t, string(body), "Das geht so nicht")
}

func TestWhoIsABotFollowsFromTheMode(t *testing.T) {
	bot := &BotParams{ReactionMS: 100, Speed: 2, Aim: 0.2}

	_, err := browserGame(ModePvP, [2]string{}, [2]*BotParams{bot, nil})
	require.ErrorContains(t, err, "a person")
	cfg, err := browserGame(ModeCvC, [2]string{}, [2]*BotParams{})
	require.NoError(t, err)
	require.NotNil(t, cfg.Bots[0])
	require.NotNil(t, cfg.Bots[1])
	_, err = browserGame(ModePvC, [2]string{"Anna", "Anna"}, [2]*BotParams{})
	require.ErrorContains(t, err, "two different names")
	_, err = browserGame(ModePvP, [2]string{strings.Repeat("x", 21), ""}, [2]*BotParams{})
	require.ErrorContains(t, err, "at most 20")
}

func TestAnEasierPresetMakesASlowerBot(t *testing.T) {
	leicht, _ := Preset("leicht")
	schwer, _ := Preset("schwer")
	easy, hard := leicht.Bot(), schwer.Bot()

	require.Greater(t, easy.ReactionMS, hard.ReactionMS)
	require.Less(t, easy.Speed, hard.Speed)
	require.Greater(t, easy.Aim, hard.Aim)
	for _, name := range PresetNames() {
		sk, _ := Preset(name)
		require.NoError(t, sk.Bot().Validate(), "the %s preset fits its own sliders", name)
	}
}

func TestTheBoardMarksTheFirstFindingAndTellsExpectedOnesApart(t *testing.T) {
	v := View{Status: StatusPlaying, State: scorer.State{Players: [2]string{"Anna", "Bernd"},
		Ends: [2]scorer.Player{scorer.PlayerA, scorer.PlayerB}, Phase: scorer.PhasePlaying, Serving: scorer.PlayerB},
		Divergences: []Divergence{
			{Rally: 4, What: "points", Expected: true},
			{Rally: 9, What: "points"},
			{Rally: 12, What: "ends"},
		}}

	b := newBoardView(v)
	require.Equal(t, "ANGABE Bernd", b.Banner)
	require.Equal(t, 2, b.Findings)
	require.False(t, b.Rows[0].First)
	require.True(t, b.Rows[1].First, "the first finding is the one to look at")
	require.False(t, b.Rows[2].First)
	require.Equal(t, "Seiten", b.Rows[2].What)
}

func TestTheBannerUsesThePanelsWords(t *testing.T) {
	players := [2]string{"Anna", "Bernd"}
	for phase, want := range map[scorer.Phase]string{
		scorer.PhaseWarmup:     "EINSPIELEN",
		scorer.PhaseServeRally: "UM DIE ANGABE",
		scorer.PhaseChangeEnds: "SEITENWECHSEL",
	} {
		require.Equal(t, want, banner(View{Status: StatusPlaying, State: scorer.State{Players: players, Phase: phase}}))
	}
	won := View{Status: StatusWon, State: scorer.State{Players: players, Phase: scorer.PhaseComplete, Winner: scorer.PlayerA}}
	require.Equal(t, "SIEG Anna", banner(won))
}

func mustGet(t *testing.T, u string) string {
	t.Helper()
	resp, err := http.Get(u)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(raw)
}

func TestABrowserGameEndsWhenZaehlwerkEndsItsMatch(t *testing.T) {
	tbl := newTable(t)
	b := startBrowser(t, newPong(t, tbl.url, 1), nil)

	// Nobody presses, so nothing the game reports is refused: only the table
	// can tell it the match is over, and PONG_BROWSER_IDLE is ten minutes.
	m, err := tbl.registry.Get(b.get(b.id).MatchID)
	require.NoError(t, err)
	tbl.zaehlwerk.EndMatch(m)

	v := b.finished(b.id)
	require.Equal(t, StatusEnded, v.Status)
	require.Equal(t, errMatchEnded.Error(), v.Reason)
}
