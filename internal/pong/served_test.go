package pong

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// A served game (ADR-0014) is played by the service on its court, with no page
// pressing anything. These tests run the court as fast as it goes unless they
// watch it, and against the real zaehlwerk handlers like every game here.

// startServed starts a served game through POST /games, with the court ticking
// every tick, zero for as fast as it goes.
func startServed(t *testing.T, p *pongFixture, tick time.Duration) *remoteFixture {
	t.Helper()
	p.pong.tick = tick
	b := startBrowser(t, p, map[string]any{"driver": DriverServer})
	return &remoteFixture{browserFixture: b}
}

func TestAServedGamePlaysItsMatchToTheEndWithNoPageOpen(t *testing.T) {
	g := startServed(t, newPong(t, newTable(t).url, 1), 0)

	v := g.finished(g.id)

	require.Equal(t, StatusWon, v.Status, v.Reason)
	require.Equal(t, DriverServer, v.Driver)
	require.Equal(t, ModeCvC, v.Mode)
	require.Equal(t, BotNames, v.Players)
	require.Positive(t, v.Rallies)
	require.Zero(t, v.Unexplained, "the court reports what it played, and zaehlwerk counts it: %+v", v.Divergences)
	require.Equal(t, v.Truth, v.Zaehlwerk)
	require.Zero(t, v.LastPress, "nobody pressed")
}

func TestAServedGameIsDrawnFromTheRelayByAnyTab(t *testing.T) {
	g := startServed(t, newPong(t, newTable(t).url, 1), servedTick)

	events := g.relayFor(300 * time.Millisecond)

	var frames []relayFrame
	for _, ev := range events {
		require.Equal(t, "frame", ev.name, "a served game has no guest")
		var f relayFrame
		require.NoError(t, json.Unmarshal([]byte(ev.data), &f))
		frames = append(frames, f)
	}
	require.GreaterOrEqual(t, len(frames), 3, "about thirty frames a second")
	for i, f := range frames {
		if i > 0 {
			require.Greater(t, f.Tick, frames[i-1].Tick)
		}
		for _, y := range f.Paddles {
			require.InDelta(t, tableY+tableH/2, y, tableH/2+60, "a paddle stays at the table")
		}
	}
}

func TestAServedGameTakesNoPressAndNoFrameButItsTokenEndsIt(t *testing.T) {
	g := startServed(t, newPong(t, newTable(t).url, 1), servedTick)

	status, answer := g.pressWith(g.token, Press{Rally: 1, Kind: PressReady, Half: scorer.HalfLeft})
	require.Equal(t, http.StatusConflict, status)
	require.Contains(t, answer.Error, "the service plays it")

	status, _ = g.post("/frames", g.token, `{"tick": 1, "ball": null, "resting": null, "paddles": [0, 0]}`)
	require.Equal(t, http.StatusConflict, status, "its frames come from its court only")

	status, _ = g.post("/end", "", "")
	require.Equal(t, http.StatusForbidden, status, "a watching tab does not end it")
	status, _ = g.post("/end", g.token, "")
	require.Equal(t, http.StatusOK, status)
	v := g.finished(g.id)
	require.Equal(t, StatusEnded, v.Status)
	require.Equal(t, errStopped.Error(), v.Reason)
}

func TestTheAPITakesAServedGameForBotsOnly(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)
	for _, body := range []startBody{
		{Driver: DriverServer, Mode: ModePvP},
		{Driver: DriverServer, Mode: ModeRemote},
		{Driver: DriverServer, Pace: "1s"},
		{Driver: DriverServer, Diagonal: true},
	} {
		status, _, raw := p.startWith(body)
		require.Equal(t, http.StatusBadRequest, status, "%+v: %s", body, raw)
	}

	status, v, raw := p.startWith(startBody{Driver: DriverServer, Mode: ModeCvC, Players: [2]string{"Ping", "Pong"}})
	require.Equal(t, http.StatusCreated, status, raw)
	require.Equal(t, [2]string{"Ping", "Pong"}, v.Players)
	require.NotNil(t, v.Bots[0])
	require.NotNil(t, v.Bots[1])
}

func TestTheLobbyStartsAServedGameAndItsPageWatchesIt(t *testing.T) {
	p, _ := newLobby(t, nil)
	require.Contains(t, get(t, p.url+"/"), `name="served"`)

	resp, err := http.PostForm(p.url+"/ui/games", url.Values{"mode": {ModePvP}, "served": {"on"}})
	require.NoError(t, err)
	_ = resp.Body.Close()
	p.pong.mu.Lock()
	require.Nil(t, p.pong.running, "people play in the browser")
	p.pong.mu.Unlock()

	form := url.Values{"mode": {ModeCvC}, "served": {"on"}}
	for _, side := range []string{"a", "b"} {
		form.Set("bot_"+side+"_reaction", "200")
		form.Set("bot_"+side+"_speed", "2")
		form.Set("bot_"+side+"_aim", "0.3")
	}
	resp, err = http.PostForm(p.url+"/ui/games", form)
	require.NoError(t, err)
	_ = resp.Body.Close()

	p.pong.mu.Lock()
	require.NotNil(t, p.pong.running)
	running := p.pong.running.View()
	p.pong.mu.Unlock()
	require.Equal(t, DriverServer, running.Driver)
	page := get(t, p.url+"/play/"+running.ID)
	require.Contains(t, page, `data-driver="server"`)
	require.Contains(t, page, `<canvas`)
	require.Contains(t, get(t, p.url+"/"), "auf dem Server, zum Zuschauen")
}

// The court decides from its seed and zaehlwerk's answers alone: the wall
// clock only paces it.
func TestTheCourtPlaysTheSameRalliesFromTheSameSeed(t *testing.T) {
	play := func() []courtPress {
		c := newCourt(rand.New(rand.NewSource(7)), [2]BotParams{Skill{Miss: 0.15}.Bot(), Skill{Miss: 0.15}.Bot()})
		c.apply(scorer.State{Phase: scorer.PhasePlaying, Ends: [2]scorer.Player{scorer.PlayerA, scorer.PlayerB},
			Serving: scorer.PlayerA})
		var out []courtPress
		for range 20 {
			for len(c.presses) == 0 {
				c.update()
				require.Less(t, c.tick, uint64(1_000_000), "a rally that never ends")
			}
			out = append(out, c.presses...)
			c.presses = nil
			c.apply(*c.st)
		}
		return out
	}
	first := play()
	require.Equal(t, first, play())
	won := map[int]int{}
	for _, p := range first {
		require.Equal(t, PressRally, p.kind)
		won[p.half]++
	}
	require.Len(t, won, 2, "both sides win rallies: %v", won)
}

func TestTheCourtWarmsUpAndThenBothBotsPressReady(t *testing.T) {
	c := newCourt(rand.New(rand.NewSource(1)), [2]BotParams{Skill{Miss: 0.15}.Bot(), Skill{Miss: 0.15}.Bot()})
	c.apply(scorer.State{Phase: scorer.PhaseWarmup, Ends: [2]scorer.Player{scorer.PlayerA, scorer.PlayerB},
		Waiting: []scorer.Half{scorer.HalfLeft, scorer.HalfRight}})

	served := false
	for range 60 {
		c.update()
		served = served || c.ball != nil
	}
	require.Empty(t, c.presses, "nobody is ready within the first second")
	for c.now < 1300 {
		c.update()
		served = served || c.ball != nil
	}
	require.True(t, served, "the warm-up is played")
	require.Equal(t, []courtPress{{kind: PressReady, half: 0}, {kind: PressReady, half: 1}}, c.presses)
}

// game.js and the court are the same game in two languages (ADR-0014). The
// behaviour is held together by reading both; the numbers by this.
func TestTheCourtKeepsTheConstantsOfGameJS(t *testing.T) {
	js := get(t, newPong(t, newTable(t).url, 1).url+"/static/game.js")
	for _, want := range []string{
		fmt.Sprintf("START_SPEED = %v;", startSpeed),
		fmt.Sprintf("MAX_SPEED = %v;", maxSpeed),
		fmt.Sprintf("SPEEDUP = %v;", speedup),
		fmt.Sprintf("LET = %v;", letShare),
		fmt.Sprintf("SPIN = %v;", spinShare),
		fmt.Sprintf("PADDLE_HALF = %v, BALL_R = %v;", paddleHalf, ballR),
		fmt.Sprintf("const T = { x: %v, y: %v, w: W - %v, h: H - %v };", tableX, tableY, courtW-tableW, courtH-tableH),
		fmt.Sprintf("const PADDLE_X = [T.x - %v, T.x + T.w + %v];", tableX-paddleX[0], paddleX[1]-tableX-tableW),
		"now - phaseSince > 1200", "now - restingSince > 900", "now - restingSince > 700",
		"Math.abs(b.x - PADDLE_X[r]) > 60",
	} {
		require.Contains(t, js, want)
	}
	page, err := web.ReadFile("web/play.html")
	require.NoError(t, err)
	require.True(t, strings.Contains(string(page), fmt.Sprintf(`width="%v" height="%v"`, courtW, courtH)),
		"the canvas is the court's size")
}

// zaehlwerk's "Demo" button posts here from its own origin, into a new tab.
func TestTheDemoButtonStartsAServedGameAndSendsItsTabToWatch(t *testing.T) {
	p, _ := newLobby(t, nil)

	resp, err := noRedirects.PostForm(p.url+"/ui/demo", nil)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	p.pong.mu.Lock()
	require.NotNil(t, p.pong.running)
	running := p.pong.running.View()
	token := p.pong.running.token
	p.pong.mu.Unlock()
	require.Equal(t, DriverServer, running.Driver, "it goes on in a tab left in the background")
	require.Equal(t, ModeCvC, running.Mode)
	require.Equal(t, "/play/"+running.ID+"#"+token, resp.Header.Get("Location"))

	resp, err = noRedirects.PostForm(p.url+"/ui/demo", nil)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, "/play/"+running.ID, resp.Header.Get("Location"),
		"a second press watches the game already playing, without its token")
}

func TestTheDemoButtonNeverTakesTheTableFromARealMatch(t *testing.T) {
	p, tbl := newLobby(t, nil)
	_, err := tbl.zaehlwerk.StartMatch(scorer.Config{}, match.Handover{})
	require.NoError(t, err)

	resp, err := noRedirects.PostForm(p.url+"/ui/demo", nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	body := string(raw)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Contains(t, body, "echtes Match")
	require.Contains(t, body, "<!DOCTYPE html>", "a page of its own: it is all the new tab shows")
}

func TestPongsLinkLandsOnTheLobbysForm(t *testing.T) {
	p, _ := newLobby(t, nil)
	require.Contains(t, get(t, p.url+"/"), `id="new-game"`)
}
