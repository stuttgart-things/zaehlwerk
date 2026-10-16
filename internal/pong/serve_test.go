package pong

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// The lobby offers the diagonal serve, and a game started with it carries it
// to its page, which loads the rule.
func TestADiagonalServeGoesFromTheLobbyToThePage(t *testing.T) {
	p, tbl := newLobby(t, nil)
	require.Contains(t, get(t, p.url+"/"), `name="diagonal"`)

	resp, err := http.PostForm(p.url+"/ui/games", url.Values{"mode": {ModePvP}, "diagonal": {"on"}})
	require.NoError(t, err)
	_ = resp.Body.Close()
	_, err = tbl.registry.Current()
	require.NoError(t, err)

	p.pong.mu.Lock()
	running := p.pong.running.View()
	p.pong.mu.Unlock()
	require.True(t, running.Diagonal)
	page := get(t, p.url+"/play/"+running.ID)
	require.Contains(t, page, `data-diagonal="1"`)
	require.Contains(t, page, `<script src="/static/serve.js">`)
	require.Contains(t, get(t, p.url+"/static/serve.js"), "window.pongServe")
}

// Without it the page is as it was: no rule loaded, nothing to judge.
func TestAGameServesStraightUnlessAskedOtherwise(t *testing.T) {
	p, _ := newLobby(t, nil)
	game := startBrowser(t, p, map[string]any{"mode": ModePvC})

	page := get(t, p.url+"/play/"+game.id)
	require.NotContains(t, page, "data-diagonal")
	require.NotContains(t, page, "serve.js")
	require.False(t, p.get(game.id).Diagonal)
}

func TestTheAPITakesADiagonalServeForABrowserGameOnly(t *testing.T) {
	p, _ := newLobby(t, nil)

	game := startBrowser(t, p, map[string]any{"mode": ModePvP, "diagonal": true})
	require.True(t, p.get(game.id).Diagonal)

	raw, err := json.Marshal(map[string]any{"diagonal": true})
	require.NoError(t, err)
	resp, err := http.Post(p.url+"/games", "application/json", bytes.NewReader(raw))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "a headless game has no serve to judge")
}
