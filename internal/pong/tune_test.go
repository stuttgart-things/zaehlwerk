package pong

import (
	"bytes"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// tune posts POST /games/{id}/faults with token and returns the status and
// the view it answered.
func (b *browserFixture) tune(token string, body any) (int, View) {
	b.t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(b.t, err)
	req, err := http.NewRequest(http.MethodPost, b.url+"/games/"+b.id+"/faults", bytes.NewReader(raw))
	require.NoError(b.t, err)
	if token != "" {
		req.Header.Set(TokenHeader, token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(b.t, err)
	defer func() { _ = resp.Body.Close() }()
	var v View
	if resp.StatusCode == http.StatusOK {
		require.NoError(b.t, json.NewDecoder(resp.Body).Decode(&v))
	}
	return resp.StatusCode, v
}

func TestThePageThatPlaysAGameChangesItsFaultsWhileItPlays(t *testing.T) {
	b := startBrowser(t, newPong(t, newTable(t).url, 1), map[string]any{"mode": ModePvP})
	rnd := rand.New(rand.NewSource(1))
	_, n := b.playUntil(rnd, 1, func(v View) bool { return v.State.Phase == scorer.PhasePlaying })

	status, v := b.tune(b.token, Faults{Lost: 1})
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, Faults{Lost: 1}, v.FaultShares)
	require.Len(t, v.FaultChanges, 1)
	require.Equal(t, Faults{Lost: 1}, v.FaultChanges[0].Faults)
	require.Equal(t, uint64(n-1), v.FaultChanges[0].Rally, "after the last press")

	v = b.press(Press{Rally: n, Kind: PressRally, Half: scorer.HalfLeft}).View
	require.Equal(t, FaultLost, v.Faults[len(v.Faults)-1].Kind, "the next rally draws against the new shares")
	require.Zero(t, v.Unexplained)

	status, v = b.tune(b.token, Faults{})
	require.Equal(t, http.StatusOK, status)
	v = b.press(Press{Rally: n + 1, Kind: PressRally, Half: scorer.HalfLeft}).View
	require.Len(t, v.Faults, 1, "off again")
	require.Len(t, v.FaultChanges, 2)
}

func TestOnlyThePageThatPlaysAGameChangesItsFaults(t *testing.T) {
	b := startBrowser(t, newPong(t, newTable(t).url, 1), nil)

	status, _ := b.tune("", Faults{Lost: 1})
	require.Equal(t, http.StatusForbidden, status)
	status, _ = b.tune("nope", Faults{Lost: 1})
	require.Equal(t, http.StatusForbidden, status)

	for _, body := range []any{Faults{Lost: 1.5}, map[string]any{"wrong-half": 0.1}} {
		status, _ = b.tune(b.token, body)
		require.Equal(t, http.StatusBadRequest, status, body)
	}
	require.Empty(t, b.get(b.id).FaultChanges)
}

// A check has no faults to change.
func TestACheckHasNoFaultsToChange(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)
	b := startBrowser(t, p, map[string]any{"driver": DriverCheck})

	status, _ := b.tune(b.token, Faults{Lost: 1})
	require.Equal(t, http.StatusConflict, status)
}

// The form is on the page of a game it can change, for faults.js to show the
// tab that holds the token, and not on a check's.
func TestThePlayPageOffersTheFaultsOfAGameItCanChange(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)
	b := startBrowser(t, p, map[string]any{"faults": Faults{Ghost: 0.25, Correct: true}})

	page := mustGet(t, b.url+"/play/"+b.id)
	require.Contains(t, page, `<details class="tune" id="tune" hidden>`)
	require.Contains(t, page, `name="ghost" min="0" max="1" step="0.01" value="0.25"`)
	require.Contains(t, page, `<input type="checkbox" name="correct" checked>`)
	require.Contains(t, page, `<script src="/static/faults.js"></script>`)

	resp, err := http.Get(b.url + "/static/faults.js")
	require.NoError(t, err)
	js, _ := io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(js), "/faults")
}
