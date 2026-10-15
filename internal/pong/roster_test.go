package pong

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
)

// roster stands in for the Schmetterpause client.
type roster struct {
	players []schmetterpause.Player
	err     error
}

func (r roster) Players(context.Context) ([]schmetterpause.Player, error) { return r.players, r.err }

func newLobby(t *testing.T, r Roster) (*pongFixture, *table) {
	t.Helper()
	tbl := newTable(t)
	p, _ := newAttract(t, tbl.url, func(c *Config) {
		c.AttractAfter = 0
		c.Roster = r
	})
	return p, tbl
}

func TestTheLobbyOffersSchmetterpausesPlayersByName(t *testing.T) {
	p, _ := newLobby(t, roster{players: []schmetterpause.Player{
		{ID: "id-skywalker", DisplayName: "skywalker"},
		{ID: "id-anna", DisplayName: "Anna"},
		{ID: "id-long", DisplayName: "somebody with a name far too long for a game"},
		{ID: "id-empty", DisplayName: ""},
	}})

	page := get(t, p.url+"/")

	_, list, ok := strings.Cut(page, `<datalist id="players">`)
	require.True(t, ok)
	list, _, _ = strings.Cut(list, "</datalist>")
	require.Equal(t, `<option value="Anna"><option value="skywalker">`, list,
		"sorted, and a name a game cannot take is left out rather than cut")
	require.Equal(t, 2, strings.Count(page, `list="players"`), "both name fields")
	require.Contains(t, page, "zählt trotzdem nicht für die Liga")
	for _, id := range []string{"id-skywalker", "id-anna", "id-long", "id-empty"} {
		require.NotContains(t, page, id, "an id is never on the page")
	}
}

func TestWithoutSchmetterpauseTheNamesAreFree(t *testing.T) {
	p, _ := newLobby(t, nil)

	page := get(t, p.url+"/")

	require.NotContains(t, page, "<datalist")
	require.NotContains(t, page, `list="players"`)
	require.NotContains(t, page, "Schmetterpause sind gerade nicht abrufbar")
}

// A list that cannot be read is said, and the lobby still starts games.
func TestALobbyWithoutTheListStillWorksAndSaysWhy(t *testing.T) {
	p, _ := newLobby(t, roster{err: errors.New("401")})

	page := get(t, p.url+"/")

	require.NotContains(t, page, "<datalist")
	require.Contains(t, page, "Schmetterpause sind gerade nicht abrufbar")
}

// A game between two of Schmetterpause's players is still a demo: named
// after them, and never reported (ADR-0008).
func TestAGameBetweenSchmetterpausePlayersIsStillADemo(t *testing.T) {
	p, tbl := newLobby(t, roster{players: []schmetterpause.Player{{ID: "1", DisplayName: "Anna"}, {ID: "2", DisplayName: "Bernd"}}})

	resp, err := http.PostForm(p.url+"/ui/games", url.Values{"mode": {ModePvP}, "name_a": {"Anna"}, "name_b": {"Bernd"}})
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	m, err := tbl.registry.Current()
	require.NoError(t, err)
	require.Equal(t, [2]string{"Anna", "Bernd"}, m.Scorer.State().Players)
	require.True(t, m.Demo)
	require.False(t, m.Handover.Wanted())
}

// The lobby offers a single set, best of three and best of five, starting on
// the service's own length, and the game plays the length picked.
func TestTheLobbyPicksHowLongAGameLasts(t *testing.T) {
	p, tbl := newLobby(t, nil)

	page := get(t, p.url+"/")
	require.Contains(t, page, `value="1"> 1 Satz`)
	require.Contains(t, page, `value="3" checked> Best of 3`, "the service's PONG_BEST_OF")
	require.Contains(t, page, `value="5"> Best of 5`)

	resp, err := http.PostForm(p.url+"/ui/games", url.Values{"mode": {ModePvP}, "best_of": {"1"}})
	require.NoError(t, err)
	_ = resp.Body.Close()
	m, err := tbl.registry.Current()
	require.NoError(t, err)
	bestOf, points := m.Scorer.Rules()
	require.Equal(t, 1, bestOf)
	require.Equal(t, 11, points)
}

func TestTheLobbyRefusesALengthItDoesNotOffer(t *testing.T) {
	p, tbl := newLobby(t, nil)

	resp, err := http.PostForm(p.url+"/ui/games", url.Values{"mode": {ModePvP}, "best_of": {"7"}})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "Das geht so nicht")
	_, err = tbl.registry.Current()
	require.Error(t, err, "no game started")
}

// A demo is not the lobby's: it plays the service's length.
func TestADemoPlaysTheServicesLength(t *testing.T) {
	p, tbl := newLobby(t, nil)
	b, _ := startSaver(t, p)
	require.NotNil(t, b)

	m, err := tbl.registry.Current()
	require.NoError(t, err)
	bestOf, _ := m.Scorer.Rules()
	require.Equal(t, 3, bestOf)
}
