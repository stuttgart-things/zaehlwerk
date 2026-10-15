package pong

import (
	"io"
	"math/rand"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// startSaver does what the screensaver page does on load, and returns the
// game it was sent to, or the page's answer when it was sent nowhere.
func startSaver(t *testing.T, p *pongFixture) (*browserFixture, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, p.url+"/ui/screensaver", nil)
	require.NoError(t, err)
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	target := resp.Header.Get("HX-Redirect")
	if target == "" {
		return nil, string(body)
	}
	path, token, ok := strings.Cut(target, "#")
	require.True(t, ok, "the token travels in the fragment: %q", target)
	id, ok := strings.CutPrefix(path, "/play/")
	require.True(t, ok, target)
	return &browserFixture{pongFixture: p, id: id, token: token}, ""
}

func TestTheScreensaverStartsADemoBetweenTwoBots(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)

	b, _ := startSaver(t, p)
	require.NotNil(t, b)
	v := b.get(b.id)
	require.True(t, v.Screensaver)
	require.False(t, v.Attract)
	require.Equal(t, DriverBrowser, v.Driver)
	require.Equal(t, ModeCvC, v.Mode)
	require.Equal(t, BotNames, v.Players)
	require.NotNil(t, v.Bots[0])
	require.NotNil(t, v.Bots[1])

	m, err := tbl.registry.Get(v.MatchID)
	require.NoError(t, err)
	require.True(t, m.Demo)
	require.False(t, m.Handover.Wanted(), "a demo is never reported")

	// The page plays it like any browser game, to the end.
	v, _ = b.playUntil(rand.New(rand.NewSource(1)), 1, func(View) bool { return false })
	require.Equal(t, StatusWon, v.Status, v.Reason)
}

// The page the screensaver is sent to is the game's own, made for a screen:
// quiet, without the lobby's controls, and with the script that ends the demo
// when the screen moves on.
func TestTheScreensaverGamePageIsForAScreen(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)
	b, _ := startSaver(t, p)

	page := get(t, p.url+"/play/"+b.id)
	require.Contains(t, page, `<body class="saver">`)
	require.Contains(t, page, `data-quiet="1"`)
	require.Contains(t, page, `/static/saver.js`)

	js := get(t, p.url+"/static/saver.js")
	require.Contains(t, js, "keepalive")

	// A game somebody plays has none of it.
	other := newPong(t, newTable(t).url, 1)
	lobby := startBrowser(t, other, map[string]any{"mode": ModeCvC})
	page = get(t, other.url+"/play/"+lobby.id)
	require.Contains(t, page, "<body>")
	require.NotContains(t, page, "saver.js")
	require.NotContains(t, page, "data-quiet")
}

func TestTheScreensaverTakesTheTableFromAttractMode(t *testing.T) {
	tbl := newTable(t)
	p, _ := newAttract(t, tbl.url, nil)
	demo := p.demo()

	b, _ := startSaver(t, p)
	require.NotNil(t, b)
	require.Equal(t, errYielded.Error(), p.get(demo.ID).Reason)

	cur, err := tbl.registry.Current()
	require.NoError(t, err)
	require.Equal(t, b.get(b.id).MatchID, cur.ID)

	// And attract mode does not take it back: the table is not free.
	time.Sleep(4 * testAttractAfter)
	require.Equal(t, 1, p.demos())
	require.Equal(t, StatusPlaying, b.get(b.id).Status)
}

// Two screens at one table would otherwise take the table from each other in
// turn.
func TestASecondScreensaverLeavesTheFirstPlaying(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)
	first, _ := startSaver(t, p)

	second, page := startSaver(t, p)
	require.Nil(t, second)
	require.Contains(t, page, "Der Tisch ist belegt.")
	require.Contains(t, page, `hx-trigger="load delay:30s"`, "and asks again later")
	require.Equal(t, StatusPlaying, first.get(first.id).Status)
}

func TestALobbyGameTakesTheTableFromTheScreensaver(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)
	saver, _ := startSaver(t, p)

	game := startBrowser(t, p, map[string]any{"mode": ModePvP})

	v := p.get(saver.id)
	require.Equal(t, StatusEnded, v.Status)
	require.Equal(t, errYielded.Error(), v.Reason)
	require.Equal(t, StatusPlaying, p.get(game.id).Status)
}

func TestARealMatchEndsTheScreensaver(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)
	saver, _ := startSaver(t, p)

	_, err := tbl.zaehlwerk.StartMatch(scorer.Config{Players: [2]string{"Anna", "Bernd"}},
		match.Handover{HomeID: "h1", AwayID: "a1", OperatorID: "o1"})
	require.NoError(t, err)

	v := p.finished(saver.id)
	require.Equal(t, StatusEnded, v.Status)
	require.Equal(t, errDisplaced.Error(), v.Reason)

	// The next demo waits for the table.
	again, page := startSaver(t, p)
	require.Nil(t, again)
	require.Contains(t, page, "Der Tisch ist belegt.")
}

// A real button pressed into the demo is somebody playing: the demo gives way,
// as attract mode's does.
func TestAButtonPressedDuringTheScreensaverEndsIt(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)
	saver, _ := startSaver(t, p)
	rnd := rand.New(rand.NewSource(1))
	_, n := saver.playUntil(rnd, 1, func(v View) bool { return v.State.Phase == scorer.PhasePlaying })

	body := `{"events": [{"source": "button-left", "half": "left", "event_id": 1}]}`
	resp, err := http.Post(tbl.url+"/ingest/button", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// The page's next rally is where the game sees zaehlwerk part from it.
	saver.pressWith(saver.token, Press{Rally: n, Kind: PressRally, Half: scorer.HalfRight})
	v := p.finished(saver.id)
	require.Equal(t, StatusEnded, v.Status)
	require.Equal(t, errIntruded.Error(), v.Reason)
}

// Closing the screen's page ends the demo through the game's own end, with the
// token: saver.js sends exactly this on pagehide.
func TestTheScreensaverEndsWhenItsPageSaysSo(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)
	saver, _ := startSaver(t, p)

	req, err := http.NewRequest(http.MethodPost, p.url+"/games/"+saver.id+"/end", nil)
	require.NoError(t, err)
	req.Header.Set(TokenHeader, saver.token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	_, err = tbl.registry.Current()
	require.Error(t, err, "the table is free again")
}

func TestTheScreensaverPageStartsADemoWhenItLoads(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)

	page := get(t, p.url+"/screensaver")
	require.Contains(t, page, `hx-post="/ui/screensaver"`)
	require.Contains(t, page, `hx-trigger="load"`)
}

func get(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode, url)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}
