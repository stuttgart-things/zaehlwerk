package pong

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// GET /live is one address for a screen that stays on: the game playing now,
// and the waiting page between two.
func TestLiveWaitsWhileNoGamePlays(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)

	page := get(t, p.url+"/live")
	require.Contains(t, page, `http-equiv="refresh"`, "it looks again by itself")
	require.Contains(t, page, "Gleich geht es weiter")
	require.NotContains(t, page, "live.js", "there is no game to follow yet")
}

func TestLiveDrawsTheGamePlayingNowAndMovesOnFromIt(t *testing.T) {
	g := startServed(t, newPong(t, newTable(t).url, 1), servedTick)

	page := get(t, g.url+"/live")
	require.Contains(t, page, `data-game="`+g.id+`"`, "the same canvas as /play/{id}")
	require.Contains(t, page, `data-driver="server"`)
	require.Contains(t, page, `data-live="`+g.id+`"`)
	require.Contains(t, page, `<script src="/static/live.js">`)
	require.NotContains(t, get(t, g.url+"/play/"+g.id), "live.js", "a game's own page stays where it is")
	require.Contains(t, get(t, g.url+"/static/live.js"), "location.replace('/live')")

	g.post("/end", g.token, "")
	require.Eventually(t, func() bool {
		return !strings.Contains(get(t, g.url+"/live"), `data-live="`+g.id+`"`)
	}, 2*time.Second, 10*time.Millisecond, "an ended game leaves /live to the next one")
}
