package pong

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/api"
	"github.com/stuttgart-things/zaehlwerk/internal/face"
	"github.com/stuttgart-things/zaehlwerk/internal/figure"
	"github.com/stuttgart-things/zaehlwerk/internal/live"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
	"github.com/stuttgart-things/zaehlwerk/internal/ui"
)

// figures stands in for Schmetterpause's figure routes, which zaehlwerk asks:
// each figure says whose it is, a random one which draw it was, and an id in
// fail has none.
type figures struct {
	mu     sync.Mutex
	ids    []string
	fail   map[string]bool
	random int
}

func (f *figures) Figure(_ context.Context, id string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids = append(f.ids, id)
	if f.fail[id] {
		return nil, errors.New("schmetterpause: 404")
	}
	who := id
	if id == "" {
		f.random++
		who = fmt.Sprintf("random-%d", f.random)
	}
	return []byte(`<svg data-face="neutral" data-who="` + who + `" style="--paddle:#159c8c"></svg>`), nil
}

func (f *figures) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ids...)
}

// newFiguredTable is zaehlwerk with its spectator view, figures from f and
// the names looked up in r: what the game asks for the figures.
func newFiguredTable(t *testing.T, r figure.Roster, f *figures) *table {
	t.Helper()
	hub := live.New()
	registry := match.NewRegistry(match.WithObserver(hub.Observe))
	zw := api.New(registry, api.WithLogger(quiet()), api.WithHub(hub),
		api.WithFigureNames(figure.NewNames(r, quiet())))
	view := ui.New(registry, zw, ui.WithLogger(quiet()), ui.WithHub(hub), ui.WithFigures(f))
	mux := http.NewServeMux()
	mux.Handle("/", zw)
	mux.Handle("/ui/", view)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &table{t: t, registry: registry, zaehlwerk: zw, url: srv.URL}
}

func newDressedLobby(t *testing.T, r roster, f *figures) *pongFixture {
	t.Helper()
	tbl := newFiguredTable(t, r, f)
	p, _ := newAttract(t, tbl.url, func(c *Config) {
		c.AttractAfter = 0
		c.Roster = r
		if f != nil {
			c.Figures = NewClient(tbl.url, quiet())
		}
	})
	return p
}

// playFromLobby starts a game from the lobby and returns its page and id.
func playFromLobby(t *testing.T, p *pongFixture, a, b string) (string, string) {
	t.Helper()
	resp, err := http.PostForm(p.url+"/ui/games", url.Values{"mode": {ModePvP}, "name_a": {a}, "name_b": {b}})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	id, ok := strings.CutPrefix(resp.Request.URL.Path, "/play/")
	require.True(t, ok, resp.Request.URL.Path)
	return string(body), id
}

func fetch(t *testing.T, u string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(u)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(body)
}

// A name Schmetterpause knows shows that player's figure, any other a random
// one. zaehlwerk looks the names up and fetches the figures, once a match;
// the game shows the ones zaehlwerk shows.
func TestTheBoardShowsTheFigureOfAPlayerSchmetterpauseKnows(t *testing.T) {
	f := &figures{}
	p := newDressedLobby(t, roster{players: []schmetterpause.Player{{ID: "id-anna", DisplayName: "Anna"}}}, f)

	page, id := playFromLobby(t, p, "Anna", "Gast")

	require.Contains(t, page, `src="/ui/games/`+id+`/figures/a.svg?face=neutral"`)
	require.Contains(t, page, `src="/ui/games/`+id+`/figures/b.svg?face=neutral"`)

	resp, svg := fetch(t, p.url+"/ui/games/"+id+"/figures/a.svg?face=happy")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "image/svg+xml", resp.Header.Get("Content-Type"))
	require.Equal(t, "default-src 'none'; style-src 'unsafe-inline'", resp.Header.Get("Content-Security-Policy"))
	require.Contains(t, svg, `data-face="happy" data-who="id-anna"`)

	_, svg = fetch(t, p.url+"/ui/games/"+id+"/figures/b.svg")
	require.Contains(t, svg, `data-who="random-1"`, "a guest gets a random figure")
	require.Equal(t, []string{"id-anna", ""}, f.asked(), "fetched once for the match")
}

// A guest's random figure is drawn once, by zaehlwerk: the game shows that
// one, not a second draw of its own with another blade, so the paddle and the
// name on the matrix have one colour.
func TestAGuestHasTheFigureZaehlwerkShows(t *testing.T) {
	f := &figures{}
	tbl := newFiguredTable(t, roster{}, f)
	p, _ := newAttract(t, tbl.url, func(c *Config) {
		c.AttractAfter = 0
		c.Figures = NewClient(tbl.url, quiet())
	})

	_, id := playFromLobby(t, p, "Gast", "Besuch")
	_, onBoard := fetch(t, p.url+"/ui/games/"+id+"/figures/b.svg")
	_, onView := fetch(t, tbl.url+"/ui/matches/"+p.get(id).MatchID+"/figures/b.svg")

	require.Contains(t, onBoard, `data-who="random-2"`)
	require.Equal(t, onView, onBoard)
	require.Equal(t, []string{"", ""}, f.asked(), "two draws, one a side")
}

// The canvas fills each player's paddle with the blade their figure carries,
// resolved as zaehlwerk resolves it for the panel: here both carry petrol, and
// b is told apart by hue. The board passes them on once the figures are in.
func TestTheBoardPassesTheBladesOnOnceTheFiguresAreIn(t *testing.T) {
	p := newDressedLobby(t, roster{players: []schmetterpause.Player{{ID: "id-anna", DisplayName: "Anna"}}}, &figures{})

	_, id := playFromLobby(t, p, "Anna", "Gast")

	fetch(t, p.url+"/ui/games/"+id+"/figures/a.svg") // waits for the fetch
	_, page := fetch(t, p.url+"/play/"+id)
	require.Contains(t, page, `data-blade-a="#00e6a0" data-blade-b="#963cff"`)
}

// Without the token zaehlwerk has no list to look names up in; the figures
// are public there, so the board still shows two, random ones.
func TestWithoutThePlayersBothSidesGetARandomFigure(t *testing.T) {
	f := &figures{}
	p := newDressedLobby(t, roster{err: errors.New("401")}, f)

	_, id := playFromLobby(t, p, "Anna", "Bernd")
	_, svg := fetch(t, p.url+"/ui/games/"+id+"/figures/a.svg")

	require.Contains(t, svg, `data-who="random-1"`)
	require.Equal(t, []string{"", ""}, f.asked())
}

// zaehlwerk serves its mascot, not to be kept, for a side whose figure it
// could not fetch: that is no figure, and the board shows its own mascot.
func TestASideWhoseFigureFailedShowsTheMascot(t *testing.T) {
	f := &figures{fail: map[string]bool{"id-anna": true}}
	p := newDressedLobby(t, roster{players: []schmetterpause.Player{{ID: "id-anna", DisplayName: "Anna"}}}, f)

	_, id := playFromLobby(t, p, "Anna", "Gast")
	resp, svg := fetch(t, p.url+"/ui/games/"+id+"/figures/a.svg")

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, svg, `<svg id="mascot"`)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))

	_, svg = fetch(t, p.url+"/ui/games/"+id+"/figures/b.svg")
	require.Contains(t, svg, `data-who="random-1"`, "the other side has its figure")
}

func TestAFigureOfASideThatDoesNotExistIsNotFound(t *testing.T) {
	p := newDressedLobby(t, roster{}, &figures{})
	_, id := playFromLobby(t, p, "Anna", "Gast")

	resp, _ := fetch(t, p.url+"/ui/games/"+id+"/figures/c.svg")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp, _ = fetch(t, p.url+"/ui/games/nope/figures/a.svg")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestWithoutFiguresTheBoardIsUnchanged(t *testing.T) {
	p := newDressedLobby(t, roster{}, nil)

	page, id := playFromLobby(t, p, "Anna", "Gast")

	require.NotContains(t, page, `class="figure"`)
	resp, _ := fetch(t, p.url+"/ui/games/"+id+"/figures/a.svg")
	require.Equal(t, http.StatusNotFound, resp.StatusCode, "no route at all")
}

// A game zaehlwerk has not yet created a match for has no figures to ask for,
// and does not keep a fetch that could only fail.
func TestAGameWithoutItsMatchHasNoFiguresYet(t *testing.T) {
	s := &Server{log: quiet(), figures: figure.NewCache(&figures{}, quiet())}
	g := &Game{ID: "g1"}

	require.Nil(t, s.figuresOf(g))

	g.matchID = "m1"
	require.NotNil(t, s.figuresOf(g))
}

func TestZaehlwerkIsAskedForAMatchsSideOnly(t *testing.T) {
	c := NewClient("http://zaehlwerk.invalid", quiet())
	for _, key := range []string{"", "m1", "m1/c", "/a", "m1/a/b"} {
		_, err := c.Figure(context.Background(), key)
		require.ErrorContains(t, err, "want <match id>/a or /b", key)
	}
}

// The game's stream sends states, and the faces want the moment between two:
// that is read back from the score.
func TestTheMomentIsReadFromTheChangeOfTheScore(t *testing.T) {
	at := func(points, sets [2]int) scorer.State { return scorer.State{Points: points, Sets: sets} }
	won := at([2]int{0, 0}, [2]int{2, 1})
	won.Complete, won.Winner = true, scorer.PlayerA

	for name, tc := range map[string]struct {
		prev, cur scorer.State
		kind      scorer.TransitionKind
		side      scorer.Player
	}{
		"a point for a":       {at([2]int{3, 4}, [2]int{}), at([2]int{4, 4}, [2]int{}), scorer.TransitionPoint, scorer.PlayerA},
		"a point for b":       {at([2]int{3, 4}, [2]int{}), at([2]int{3, 5}, [2]int{}), scorer.TransitionPoint, scorer.PlayerB},
		"a set for b":         {at([2]int{9, 10}, [2]int{1, 0}), at([2]int{0, 0}, [2]int{1, 1}), scorer.TransitionSetWon, scorer.PlayerB},
		"the match for a":     {at([2]int{10, 9}, [2]int{1, 1}), won, scorer.TransitionMatchWon, scorer.PlayerA},
		"a point taken back":  {at([2]int{4, 4}, [2]int{}), at([2]int{3, 4}, [2]int{}), scorer.TransitionUndo, ""},
		"a set taken back":    {at([2]int{0, 0}, [2]int{1, 0}), at([2]int{10, 9}, [2]int{0, 0}), scorer.TransitionUndo, ""},
		"nothing that counts": {at([2]int{3, 4}, [2]int{}), at([2]int{3, 4}, [2]int{}), "", ""},
		"two points at once":  {at([2]int{3, 4}, [2]int{}), at([2]int{4, 5}, [2]int{}), "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			kind, side := change(tc.prev, tc.cur)
			require.Equal(t, tc.kind, kind)
			require.Equal(t, tc.side, side)
		})
	}
}

// The faces come from the face package, with the stake from the game's own
// rules: a set point at 10:9 has the side facing it surprised.
func TestTheFacesReadTheStakeOffTheGamesRules(t *testing.T) {
	g := &Game{ID: "g1", cfg: GameConfig{BestOf: 3}}
	prev := scorer.State{Phase: scorer.PhasePlaying, Points: [2]int{9, 9}}
	cur := scorer.State{Phase: scorer.PhasePlaying, Points: [2]int{10, 9}}

	got, _ := faces(g, &prev, cur, face.Faces{}, face.Streak{})

	require.Equal(t, face.Faces{face.Happy, face.Surprised}, got)
}

// The board goes out with every rally; a point's faces stay until the next.
func TestAChangeThatIsNoPointKeepsTheFaces(t *testing.T) {
	g := &Game{ID: "g1", cfg: GameConfig{BestOf: 3}}
	st := scorer.State{Phase: scorer.PhasePlaying, Points: [2]int{3, 2}}
	shown := face.Faces{face.Happy, face.Skeptical}

	got, streak := faces(g, &st, st, shown, face.Streak{1, 0})

	require.Equal(t, shown, got)
	require.Equal(t, face.Streak{1, 0}, streak)
}
