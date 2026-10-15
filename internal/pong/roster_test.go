package pong

import (
	"context"
	"errors"
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
		{ID: "1", DisplayName: "skywalker"},
		{ID: "2", DisplayName: "Anna"},
		{ID: "3", DisplayName: "somebody with a name far too long for a game"},
		{ID: "4", DisplayName: ""},
	}})

	page := get(t, p.url+"/")

	_, list, ok := strings.Cut(page, `<datalist id="players">`)
	require.True(t, ok)
	list, _, _ = strings.Cut(list, "</datalist>")
	require.Equal(t, `<option value="Anna"><option value="skywalker">`, list,
		"sorted, and a name a game cannot take is left out rather than cut")
	require.Equal(t, 2, strings.Count(page, `list="players"`), "both name fields")
	require.Contains(t, page, "zählt trotzdem nicht für die Liga")
	require.NotContains(t, page, `value="1"`, "an id is never on the page")
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
