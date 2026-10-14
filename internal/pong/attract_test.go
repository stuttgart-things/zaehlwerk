package pong

import (
	"bytes"
	"context"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// The waits are short here, as resetQuiet is: the rules do not change with the
// length of the wait, and a test that sat through ten minutes would not be run.
const (
	testAttractAfter = 150 * time.Millisecond
	testAttractPace  = 10 * time.Millisecond
)

// logs is a log the test can read while the service writes to it.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) count(msg string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.buf.String(), msg)
}

// newAttract is a service with attract mode on, its log kept.
func newAttract(t *testing.T, zaehlwerkURL string, edit func(*Config)) (*pongFixture, *logs) {
	t.Helper()
	cfg := Config{
		ZaehlwerkURL: zaehlwerkURL,
		BestOf:       3,
		Skills:       [2]Skill{skills["mittel"], skills["mittel"]},
		Seed:         1,
		AttractAfter: testAttractAfter,
		AttractPace:  testAttractPace,
	}
	if edit != nil {
		edit(&cfg)
	}
	l := &logs{}
	p := New(cfg, slog.New(slog.NewTextHandler(l, nil)))
	p.client.retryAfter = time.Millisecond
	srv := httptest.NewServer(p)
	t.Cleanup(func() {
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, p.Close(ctx))
	})
	p.Start()
	return &pongFixture{t: t, pong: p, url: srv.URL}, l
}

// demo waits for attract mode to start a demo and returns it.
func (f *pongFixture) demo() View {
	f.t.Helper()
	var v View
	require.Eventually(f.t, func() bool {
		f.pong.mu.Lock()
		g := f.pong.running
		f.pong.mu.Unlock()
		if g == nil || !g.cfg.Attract || g.matchIDNow() == "" {
			return false
		}
		v = g.View()
		return true
	}, 5*time.Second, 5*time.Millisecond)
	return v
}

// demos counts the demos the service has started.
func (f *pongFixture) demos() int {
	f.pong.mu.Lock()
	defer f.pong.mu.Unlock()
	n := 0
	for _, g := range f.pong.games {
		if g.cfg.Attract {
			n++
		}
	}
	return n
}

func TestAFreeTableStartsADemoAfterTheWait(t *testing.T) {
	tbl := newTable(t)
	started := time.Now()
	p, _ := newAttract(t, tbl.url, func(c *Config) {
		// Whatever PONG_FAULTS says, a demo has none.
		c.Faults = Faults{WrongHalf: 1, Lost: 1}
	})

	v := p.demo()
	require.GreaterOrEqual(t, time.Since(started), testAttractAfter, "not before the table was free for the whole wait")
	require.True(t, v.Attract)
	require.Equal(t, DriverBots, v.Driver)
	require.Equal(t, BotNames, v.Players)
	require.Equal(t, Faults{}, v.FaultShares)

	v = p.finished(v.ID)
	require.Equal(t, StatusWon, v.Status, v.Reason)
	require.Empty(t, v.Faults)
	require.Empty(t, v.Divergences)

	m, err := tbl.registry.Get(v.MatchID)
	require.NoError(t, err)
	require.False(t, m.Handover.Wanted(), "a demo is never reported")
}

func TestADemoGoesAtTheAttractPace(t *testing.T) {
	tbl := newTable(t)
	p, _ := newAttract(t, tbl.url, nil)

	v := p.demo()
	began := time.Now()
	v = p.finished(v.ID)
	require.Equal(t, StatusWon, v.Status, v.Reason)
	// A pause before every report, and a rally is one. Half of them leaves
	// room for the ones played before the poll above saw the demo.
	require.GreaterOrEqual(t, time.Since(began), time.Duration(v.Rallies)*testAttractPace/2,
		"a demo is followed on the panel, not over before anybody sees it")
}

func TestAWonDemoIsFollowedByAFreeTableForTheWholeWait(t *testing.T) {
	tbl := newTable(t)
	p, _ := newAttract(t, tbl.url, func(c *Config) {
		c.AttractAfter = 400 * time.Millisecond
		c.AttractPace = time.Millisecond
	})

	first := p.finished(p.demo().ID)
	require.Equal(t, StatusWon, first.Status)
	won := time.Now()

	next := p.demo()
	require.NotEqual(t, first.ID, next.ID)
	// The wait starts at the no_match after the win. Some of it passed
	// between that event and the poll above noticing the win, so it is
	// checked with room for that.
	require.Greater(t, time.Since(won), 300*time.Millisecond, "a demo is not hung on the end of the last one")
}

func TestARealMatchEndsTheDemo(t *testing.T) {
	tbl := newTable(t)
	p, _ := newAttract(t, tbl.url, nil)
	demo := p.demo()

	real, err := tbl.zaehlwerk.StartMatch(scorer.Config{Players: [2]string{"Anna", "Bernd"}},
		match.Handover{HomeID: "h1", AwayID: "a1", OperatorID: "o1"})
	require.NoError(t, err)

	demo = p.finished(demo.ID)
	require.Equal(t, StatusEnded, demo.Status)
	require.Equal(t, errDisplaced.Error(), demo.Reason)
	ours, err := tbl.registry.Get(demo.MatchID)
	require.NoError(t, err)
	require.False(t, ours.Running(), "the demo ended its own match")

	// While the real match is current, the wait does not run at all: no
	// demo however long it takes.
	time.Sleep(4 * testAttractAfter)
	require.Equal(t, 1, p.demos())
	require.True(t, real.Running())
	require.True(t, p.pong.Attract().Next.IsZero())

	// Its end is the next no_match, and the wait starts from there.
	tbl.zaehlwerk.EndMatch(real)
	require.NotEqual(t, demo.ID, p.demo().ID)
}

func TestABrowserGameTakesTheTableFromTheDemo(t *testing.T) {
	tbl := newTable(t)
	p, _ := newAttract(t, tbl.url, nil)
	demo := p.demo()

	status, game, raw := p.startWith(startBody{Driver: DriverBrowser, Mode: ModePvP})
	require.Equal(t, http.StatusCreated, status, raw)
	require.False(t, game.Attract)

	demo = p.get(demo.ID)
	require.Equal(t, StatusEnded, demo.Status)
	require.Equal(t, errYielded.Error(), demo.Reason)
	ours, err := tbl.registry.Get(demo.MatchID)
	require.NoError(t, err)
	require.False(t, ours.Running())

	cur, err := tbl.registry.Current()
	require.NoError(t, err)
	require.Equal(t, game.MatchID, cur.ID, "the new game has the table")

	// And keeps it: its match is current, so no demo waits behind it.
	time.Sleep(4 * testAttractAfter)
	require.Equal(t, 1, p.demos())
}

func TestPOSTGamesTakesTheTableFromTheDemo(t *testing.T) {
	tbl := newTable(t)
	p, _ := newAttract(t, tbl.url, nil)
	demo := p.demo()

	status, game, raw := p.start("0s")
	require.Equal(t, http.StatusCreated, status, raw)
	require.False(t, game.Attract)
	require.Equal(t, errYielded.Error(), p.get(demo.ID).Reason)
	require.Equal(t, StatusWon, p.finished(game.ID).Status)
}

func TestTwoGamesAskedForAtOnceEndTheDemoOnce(t *testing.T) {
	tbl := newTable(t)
	p, _ := newAttract(t, tbl.url, nil)
	p.demo()

	// Both see the demo; the lock lets one end it and start, and the other
	// then finds that game, not a free table.
	statuses := make(chan int, 2)
	for range 2 {
		go func() {
			status, _, _ := p.start("50ms")
			statuses <- status
		}()
	}
	got := []int{<-statuses, <-statuses}
	require.ElementsMatch(t, []int{http.StatusCreated, http.StatusConflict}, got)
}

func TestAButtonPressedDuringADemoEndsItAndTheWaitStartsAgain(t *testing.T) {
	tbl := newTable(t)
	p, log := newAttract(t, tbl.url, func(c *Config) { c.AttractPace = 30 * time.Millisecond })
	demo := p.demo()
	// Early in the first set, so the press is a rally and not a ready, which
	// would change nothing the demo could see.
	require.Eventually(t, func() bool {
		st := p.get(demo.ID).State
		return st.Phase == scorer.PhasePlaying && st.Points[0]+st.Points[1] < 4
	}, 5*time.Second, time.Millisecond)

	body := `{"events": [{"source": "button-left", "half": "left", "event_id": 1}]}`
	resp, err := http.Post(tbl.url+"/ingest/button", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	demo = p.finished(demo.ID)
	ended := time.Now()
	require.Equal(t, StatusEnded, demo.Status)
	require.Equal(t, errIntruded.Error(), demo.Reason)
	require.NotEmpty(t, demo.Divergences)
	require.Equal(t, 1, log.count("truth and zaehlwerk disagree in a demo, giving way"))

	ours, err := tbl.registry.Get(demo.MatchID)
	require.NoError(t, err)
	require.False(t, ours.Running(), "the demo ended its own match")
	_, err = tbl.registry.Current()
	require.Error(t, err, "and left the table free")

	// The end is a no_match, and the wait starts from there, whole.
	next := p.demo()
	require.NotEqual(t, demo.ID, next.ID)
	require.Greater(t, time.Since(ended), testAttractAfter/2)
}

func TestABrowserGameDoesNotEndAtADivergenceItsFaultsExplain(t *testing.T) {
	// Attract mode on, so it is the game that decides, not the service.
	p, _ := newAttract(t, newTable(t).url, func(c *Config) { c.AttractAfter = 10 * time.Minute })
	b := startBrowser(t, p, map[string]any{"faults": Faults{Lost: 1}})
	rnd := rand.New(rand.NewSource(1))

	_, n := b.playUntil(rnd, 1, func(v View) bool { return v.State.Phase == scorer.PhasePlaying })
	v := b.press(Press{Rally: n, Kind: PressRally, Half: scorer.HalfRight}).View
	require.NotEmpty(t, v.Divergences)
	require.True(t, v.Divergences[0].Expected)

	v = b.press(Press{Rally: n + 1, Kind: PressRally, Half: scorer.HalfLeft}).View
	require.Equal(t, StatusPlaying, v.Status, "giving way is for demos only")
}

func TestAStaleMatchBlocksTheDemoAndIsLeftAlone(t *testing.T) {
	tbl := newTable(t)
	stale, err := tbl.zaehlwerk.StartMatch(scorer.Config{}, match.Handover{})
	require.NoError(t, err)
	p, log := newAttract(t, tbl.url, nil)

	const said = "a match has been quiet at the table for the whole wait"
	require.Eventually(t, func() bool { return log.count(said) == 1 }, 5*time.Second, 5*time.Millisecond)
	// Many waits later: still one line, still no demo, and the match is
	// still running.
	time.Sleep(5 * testAttractAfter)
	require.Equal(t, 1, log.count(said))
	require.Zero(t, p.demos())
	require.True(t, stale.Running(), "a match somebody left is never ended")
}

func TestAPointOnAQuietMatchDoesNotRepeatTheLine(t *testing.T) {
	tbl := newTable(t)
	stale, err := tbl.zaehlwerk.StartMatch(scorer.Config{}, match.Handover{})
	require.NoError(t, err)
	_, log := newAttract(t, tbl.url, nil)

	const said = "a match has been quiet at the table for the whole wait"
	require.Eventually(t, func() bool { return log.count(said) == 1 }, 5*time.Second, 5*time.Millisecond)
	// A point through ingest, so it is on the stream as an event of the
	// current match.
	body := `{"match_id": "` + stale.ID + `", "source": "phone", "event_id": 1, "player": "a", "delta": 1}`
	resp, err := http.Post(tbl.url+"/ingest/web", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	time.Sleep(4 * testAttractAfter)
	require.Equal(t, 1, log.count(said), "once per match, not once per quiet spell")
}

func TestTheLobbySaysWhenTheNextDemoStarts(t *testing.T) {
	tbl := newTable(t)
	p, _ := newAttract(t, tbl.url, func(c *Config) { c.AttractAfter = 10 * time.Minute })
	require.Eventually(t, func() bool { return !p.pong.Attract().Next.IsZero() }, 5*time.Second, 5*time.Millisecond)

	body := lobbyPage(t, p.url)
	require.Contains(t, body, "startet die nächste Demo in")
	require.Contains(t, body, "Min.")

	_, err := tbl.zaehlwerk.StartMatch(scorer.Config{}, match.Handover{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return p.pong.Attract().Next.IsZero() }, 5*time.Second, 5*time.Millisecond)
	require.Contains(t, lobbyPage(t, p.url), "beendet wird es von ihr nie")
}

func TestTheLobbySaysNothingAboutADemoWhenAttractModeIsOff(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)
	require.NotContains(t, lobbyPage(t, p.url), `id="attract"`)

	resp, err := http.Get(p.url + "/ui/attract")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestHealthzIsTheSameWithAttractModeOn(t *testing.T) {
	p, _ := newAttract(t, newTable(t).url, nil)
	resp, err := http.Get(p.url + "/healthz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestAWaitIsSaidInMinutes(t *testing.T) {
	require.Equal(t, "10 Min.", minutes(10*time.Minute))
	require.Equal(t, "8 Min.", minutes(7*time.Minute+30*time.Second))
	require.Equal(t, "45 s", minutes(45*time.Second))
	require.Equal(t, "0 s", minutes(0))
}

func lobbyPage(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url + "/")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(resp.Body)
	require.NoError(t, err)
	return buf.String()
}

// foreignDemo is a demo the service did not start: left behind by a pod that
// is gone, or another instance's. It is created the way any Pong creates one.
func foreignDemo(t *testing.T, tbl *table) *match.Match {
	t.Helper()
	resp, err := http.Post(tbl.url+"/matches", "application/json",
		strings.NewReader(`{"players":["Pong A","Pong B"],"demo":true}`))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	m, err := tbl.registry.Current()
	require.NoError(t, err)
	require.True(t, m.Demo)
	return m
}

func TestAttractEndsAQuietDemoThatIsNotItsOwn(t *testing.T) {
	tbl := newTable(t)
	left := foreignDemo(t, tbl)
	p, log := newAttract(t, tbl.url, nil)

	require.Eventually(t, func() bool {
		return !left.Running() && log.count("a demo that is not ours was quiet for the whole wait, ended it") == 1
	}, 5*time.Second, 5*time.Millisecond)
	require.Zero(t, log.count("a match has been quiet at the table"), "a demo is not reported as somebody's match")

	// The no_match after it starts the wait, and a demo of its own follows.
	v := p.demo()
	require.NotEqual(t, left.ID, v.MatchID)
}

// A demo somebody plays is never quiet for a whole wait, so attract mode
// leaves it be.
func TestAttractLeavesADemoThatIsPlayedAlone(t *testing.T) {
	tbl := newTable(t)
	other := foreignDemo(t, tbl)
	_, log := newAttract(t, tbl.url, nil)

	for id := uint64(1); id <= 8; id++ {
		body := `{"match_id":"` + other.ID + `","source":"other-pong","event_id":` + strconv.FormatUint(id, 10) + `,"player":"a","delta":1}`
		resp, err := http.Post(tbl.url+"/ingest/web", "application/json", strings.NewReader(body))
		require.NoError(t, err)
		_ = resp.Body.Close()
		time.Sleep(testAttractAfter / 2)
	}
	require.True(t, other.Running())
	require.Zero(t, log.count("ended it"))
}

func TestALobbyGameEndsAForeignDemoAtOnce(t *testing.T) {
	tbl := newTable(t)
	left := foreignDemo(t, tbl)
	p := newPong(t, tbl.url, 1)

	status, game, raw := p.start("0s")
	require.Equal(t, http.StatusCreated, status, raw)
	require.False(t, left.Running(), "ended before the game's match was created")
	require.NotEqual(t, left.ID, game.MatchID)
	require.Equal(t, StatusWon, p.finished(game.ID).Status)
}

// The rule ADR-0006 gave stays for everything that is not marked: a match
// nobody scores any more looks exactly like one paused between two points.
func TestAnUnmarkedQuietMatchIsStillLeftAlone(t *testing.T) {
	tbl := newTable(t)
	stale, err := tbl.zaehlwerk.StartMatch(scorer.Config{}, match.Handover{})
	require.NoError(t, err)
	p, log := newAttract(t, tbl.url, nil)

	require.Eventually(t, func() bool {
		return log.count("a match has been quiet at the table for the whole wait") == 1
	}, 5*time.Second, 5*time.Millisecond)
	status, _, raw := p.start("0s")
	require.Equal(t, http.StatusConflict, status, raw)

	time.Sleep(3 * testAttractAfter)
	require.True(t, stale.Running(), "neither attract mode nor a game asked for ends it")
	require.Zero(t, p.demos())
}

// The stream said the table was free, and zaehlwerk refuses the demo all the
// same: a real match was created in between (ADR-0008). That is a busy table,
// not a failure.
func TestA409OnCreateIsABusyTable(t *testing.T) {
	tbl := newTable(t)
	real, err := tbl.zaehlwerk.StartMatch(scorer.Config{}, match.Handover{})
	require.NoError(t, err)
	p := newPong(t, tbl.url, 1)

	st, err := p.pong.client.CreateMatch(context.Background(), BotNames, 3)
	require.ErrorIs(t, err, errBusy)
	require.Equal(t, real.ID, st.MatchID, "the answer names the match that has the table")
	require.True(t, real.Running())
}

func TestEveryMatchTheGameCreatesIsADemo(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)

	status, game, raw := p.start("50ms")
	require.Equal(t, http.StatusCreated, status, raw)
	m, err := tbl.registry.Get(game.MatchID)
	require.NoError(t, err)
	require.True(t, m.Demo)
}
