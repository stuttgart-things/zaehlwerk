package pong

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// heldLeaguePong is leaguePong with the court at its real pace, so a match is
// still playing when the test stops the league.
func heldLeaguePong(t *testing.T, zaehlwerkURL string) *Server {
	t.Helper()
	l := sandboxLeague(sandboxRoster())
	l.Pause = time.Millisecond
	p := New(Config{
		ZaehlwerkURL: zaehlwerkURL,
		BestOf:       1,
		Skills:       [2]Skill{skills["mittel"], skills["mittel"]},
		Seed:         7,
		League:       &l,
	}, slog.New(slog.NewTextHandler(&lockedLog{}, nil)))
	p.client.retryAfter = time.Millisecond
	p.tick = servedTick
	p.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, p.Close(ctx))
	})
	return p
}

func leagueGameNow(p *Server) *Game {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.leagueNow
}

func leagueGames(p *Server) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, g := range p.games {
		if g.cfg.Handover != nil {
			n++
		}
	}
	return n
}

// Stopped in the lobby, the league ends the match it plays, which is not won
// and so reported nowhere, and starts no other until it plays again.
func TestStoppingTheLeagueEndsItsMatchAndStartsNoneUntilItPlaysAgain(t *testing.T) {
	zw := newSandboxTable(t, leagueToken)
	p := heldLeaguePong(t, zw.url)

	require.Eventually(t, func() bool { return leagueGameNow(p) != nil }, 10*time.Second, 5*time.Millisecond)
	g := leagueGameNow(p)

	p.holdLeague()
	select {
	case <-g.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the league match did not stop")
	}
	v := g.View()
	require.Equal(t, StatusEnded, v.Status)
	require.Equal(t, errHeld.Error(), v.Reason)
	m, err := zw.registry.Get(v.MatchID)
	require.NoError(t, err)
	require.False(t, m.Running(), "ended in zaehlwerk, not left running")
	require.False(t, m.Report().Done, "a match that is not won is not reported")

	started := leagueGames(p)
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, started, leagueGames(p), "no match while the league is stopped")
	require.Nil(t, leagueGameNow(p))

	p.resumeLeague()
	require.Eventually(t, func() bool { return leagueGames(p) > started }, 10*time.Second, 5*time.Millisecond,
		"playing again starts the next match")
}

// The lobby has the switch; /live says why nothing plays.
func TestTheLobbyStopsAndPlaysTheLeague(t *testing.T) {
	zw := newSandboxTable(t, leagueToken)
	p := heldLeaguePong(t, zw.url)
	ts := httptest.NewServer(p)
	t.Cleanup(ts.Close)
	srv := ts.URL

	require.Contains(t, get(t, srv+"/"), `value="stop">Anhalten</button>`)

	req, err := http.NewRequest(http.MethodPost, srv+"/ui/league", strings.NewReader(url.Values{"action": {"stop"}}.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.True(t, p.leagueHeldNow())
	require.Contains(t, get(t, srv+"/"), `value="play">Weiter</button>`)
	require.Eventually(t, func() bool { return strings.Contains(get(t, srv+"/live"), "Die Liga ist angehalten") },
		5*time.Second, 10*time.Millisecond)

	resp, err = noRedirects.PostForm(srv+"/ui/league", url.Values{"action": {"play"}})
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, "without htmx, back to the lobby")
	require.False(t, p.leagueHeldNow())
}

func TestTheLobbyHasNoLeagueSwitchWithoutLeagueMode(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)
	require.NotContains(t, get(t, p.url+"/"), `id="league"`)
	resp, err := http.PostForm(p.url+"/ui/league", url.Values{"action": {"stop"}})
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}
