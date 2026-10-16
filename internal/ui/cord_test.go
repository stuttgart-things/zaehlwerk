package ui

import (
	"net/http"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// The expected classes are what schmetterpause's PaddleClass returned for
// these ids, run there: a player's colour here has to be the one their mascot
// carries there.
func TestAPlayersColourIsTheBladeSchmetterpauseGivesThem(t *testing.T) {
	a, b := blades(match.Handover{HomeID: "skywalker", AwayID: "sthings"})

	require.Equal(t, paddleColours[2], a, "paddle-2 there")
	require.Equal(t, paddleColours[3], b, "paddle-3 there")
}

// Both of these are paddle-3 there. Sharing it here would leave the cord
// saying nothing, so the right-hand side moves on to the next colour.
func TestTwoPlayersWithTheSameBladeAreToldApart(t *testing.T) {
	a, b := blades(match.Handover{
		HomeID: "7f1c2a90-3b4d-4e5f-8a6b-1c2d3e4f5a6b",
		AwayID: "0b9e8d7c-6a5b-4c3d-9e2f-1a0b9c8d7e6f",
	})

	require.Equal(t, paddleColours[3], a)
	require.Equal(t, paddleColours[4], b)
}

func TestAMatchWithoutIdsWearsTheWidestApartPair(t *testing.T) {
	a, b := blades(match.Handover{})

	require.Equal(t, "#3aa6ff", a, "light blue on the left")
	require.Equal(t, "#d64f9c", b, "pink on the right")
}

func TestTheLeaderIsTheWinnerThenSetsThenPoints(t *testing.T) {
	tests := []struct {
		name string
		st   scorer.State
		want scorer.Player
	}{
		{name: "level at the start", st: scorer.State{}},
		{name: "points in the set in play", st: scorer.State{Points: [2]int{3, 5}}, want: scorer.PlayerB},
		{name: "sets before points", st: scorer.State{Sets: [2]int{1, 0}, Points: [2]int{2, 9}}, want: scorer.PlayerA},
		{name: "level on sets and points", st: scorer.State{Sets: [2]int{1, 1}, Points: [2]int{4, 4}}},
		{name: "the winner, whatever the last set", st: scorer.State{Complete: true, Winner: scorer.PlayerB, Sets: [2]int{1, 2}, Points: [2]int{11, 13}}, want: scorer.PlayerB},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, leader(tt.st))
		})
	}
}

var cordPattern = regexp.MustCompile(`<svg class="referee"[^>]*>`)

// The referee stands under the score, its cord in the colour of whoever leads
// and in the drawing's own gold while it is level; each name carries its
// side's colour as the key.
func TestTheRefereesCordFollowsTheScore(t *testing.T) {
	f := newFixture(t)
	m := f.match("Anna", "Bernd")

	_, body := f.do(http.MethodGet, "/ui/live", nil)
	require.Equal(t, `<svg class="referee" viewBox="0 0 410 420" aria-hidden="true">`, cordPattern.FindString(body), "level: no colour of its own")
	require.Contains(t, body, `style="--blade: #3aa6ff"`)
	require.Contains(t, body, `style="--blade: #d64f9c"`)
	require.Contains(t, body, `<use href="/ui/static/mascot.svg#mascot"/>`)

	f.point(m, "a")
	_, body = f.do(http.MethodGet, "/ui/live", nil)
	require.Contains(t, cordPattern.FindString(body), `style="--paddle: #3aa6ff"`)

	f.point(m, "b")
	f.point(m, "b")
	_, body = f.do(http.MethodGet, "/ui/watch", nil)
	require.Contains(t, cordPattern.FindString(body), `style="--paddle: #d64f9c"`)
}

// The partial goes out with every point; the drawing is referenced, not sent.
func TestTheStreamCarriesTheCordAndNotTheDrawing(t *testing.T) {
	f := newStreamFixture(t)
	r, _ := f.connectTo("/ui/matches/" + f.match.ID + "/stream?view=watch")
	readEvent(t, r)

	f.point(scorer.PlayerB)
	ev := readEvent(t, r)

	require.Contains(t, cordPattern.FindString(ev.data), `--paddle: `)
	require.NotContains(t, ev.data, `<path id="fur"`)
}
