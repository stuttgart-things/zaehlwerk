package ui

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/figure"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// fakeFigures is Schmetterpause's figure routes: a figure per id in the colour
// given for it, a fresh random one for an empty id, and a failure for an id
// it is told to fail.
type fakeFigures struct {
	mu     sync.Mutex
	blades map[string]string
	fail   map[string]bool
	asked  []string
}

func (f *fakeFigures) Figure(_ context.Context, id string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, id)
	if f.fail[id] {
		return nil, errors.New("schmetterpause is down")
	}
	blade := f.blades[id]
	if id == "" {
		blade = "#c05fd0"
		id = "random"
	}
	return []byte(`<svg viewBox="0 0 420 420" class="sthings" data-face="neutral" style="--fur:#f89828;--paddle:` +
		blade + `"><g id="figure-` + id + `"/></svg>`), nil
}

func (f *fakeFigures) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

func (f *fixture) reportedBetween(home, away string) *match.Match {
	f.t.Helper()
	m, _, err := f.registry.CreateFor(scorer.Config{BestOf: 3, PointsPerSet: 11},
		match.Setup{Handover: match.Handover{HomeID: home, AwayID: away, OperatorID: "id-cem"}})
	require.NoError(f.t, err)
	return m
}

var figureImg = regexp.MustCompile(`<img class="figure" src="([^"]+)"`)

// figureSrcs is the two figure addresses on a spectator page, a then b.
func figureSrcs(t *testing.T, body string) []string {
	t.Helper()
	var srcs []string
	for _, m := range figureImg.FindAllStringSubmatch(body, -1) {
		srcs = append(srcs, strings.ReplaceAll(m[1], "&amp;", "&"))
	}
	return srcs
}

func TestASpectatorSeesBothPlayersFiguresWithTheirFaces(t *testing.T) {
	src := &fakeFigures{blades: map[string]string{"id-anna": "#159c8c", "id-bernd": "#2f6fd0"}}
	f := newFixture(t, WithFigures(src))
	m := f.reportedBetween("id-anna", "id-bernd")

	_, body := f.do(http.MethodGet, "/ui/watch", nil)
	srcs := figureSrcs(t, body)
	require.Equal(t, []string{
		"/ui/matches/" + m.ID + "/figures/a.svg?face=neutral",
		"/ui/matches/" + m.ID + "/figures/b.svg?face=neutral",
	}, srcs)

	rec, svg := f.do(http.MethodGet, strings.Replace(srcs[0], "neutral", "happy", 1), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "image/svg+xml", rec.Header().Get("Content-Type"))
	require.Contains(t, svg, `id="figure-id-anna"`)
	require.Contains(t, svg, `data-face="happy"`, "the face the address asks for")
	require.NotContains(t, svg, `data-face="neutral"`)

	_, svg = f.do(http.MethodGet, srcs[1], nil)
	require.Contains(t, svg, `id="figure-id-bernd"`)
}

// The referee's board carries the same figures, smaller, and the stream that
// keeps it current gives them the faces of each point like the spectator's.
func TestTheRefereeSeesBothPlayersFiguresToo(t *testing.T) {
	src := &fakeFigures{blades: map[string]string{"id-anna": "#159c8c", "id-bernd": "#2f6fd0"}}
	f := newFixture(t, WithFigures(src))
	m := f.reportedBetween("id-anna", "id-bernd")

	_, body := f.do(http.MethodGet, "/ui", nil)
	require.Equal(t, []string{
		"/ui/matches/" + m.ID + "/figures/a.svg?face=neutral",
		"/ui/matches/" + m.ID + "/figures/b.svg?face=neutral",
	}, figureSrcs(t, body))
}

// Fetched once per match: a random figure is a new one per request there, and
// a side that changed its figure every point would not be one figure.
func TestAMatchWithNobodyBehindItGetsTwoRandomFiguresOnce(t *testing.T) {
	src := &fakeFigures{}
	f := newFixture(t, WithFigures(src))
	m := f.demo()

	for _, face := range []string{"neutral", "happy", "sad"} {
		for _, side := range []string{"a", "b"} {
			_, svg := f.do(http.MethodGet, "/ui/matches/"+m.ID+"/figures/"+side+".svg?face="+face, nil)
			require.Contains(t, svg, `id="figure-random"`)
		}
	}
	require.Equal(t, []string{"", ""}, src.calls())
}

// A face the query string makes up never reaches the document.
func TestAnUnknownFaceIsNeutral(t *testing.T) {
	f := newFixture(t, WithFigures(&fakeFigures{}))
	m := f.demo()

	_, svg := f.do(http.MethodGet, "/ui/matches/"+m.ID+"/figures/a.svg?face=%22%3E%3Cscript%3E", nil)
	require.Contains(t, svg, `data-face="neutral"`)
	require.NotContains(t, svg, "script")
}

// Schmetterpause down is the screen before figures existed, side by side: the
// mascot, in that side's colour, and asked for again next time.
func TestASideWhoseFigureFailedShowsTheMascot(t *testing.T) {
	src := &fakeFigures{blades: map[string]string{"id-bernd": "#2f6fd0"}, fail: map[string]bool{"id-anna": true}}
	f := newFixture(t, WithFigures(src))
	m := f.reportedBetween("id-anna", "id-bernd")

	rec, svg := f.do(http.MethodGet, "/ui/matches/"+m.ID+"/figures/a.svg?face=happy", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, strings.HasPrefix(svg, `<svg id="mascot" style="--paddle: #`), svg[:80])
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	rec, svg = f.do(http.MethodGet, "/ui/matches/"+m.ID+"/figures/b.svg?face=happy", nil)
	require.Contains(t, svg, `id="figure-id-bernd"`)
	require.Equal(t, "private, max-age=3600", rec.Header().Get("Cache-Control"))
}

// Since Schmetterpause ADR-0029 a player chooses their blade; recomputing it
// from the id gave everybody who chose the colour they had before (#195).
func TestTheCordTakesTheBladeThePlayerChose(t *testing.T) {
	// FNV over these ids gives paddle-2 and paddle-3 there; both chose others.
	src := &fakeFigures{blades: map[string]string{"skywalker": "#c05fd0", "sthings": "#4d7c4a"}}
	f := newFixture(t, WithFigures(src))
	m := f.reportedBetween("skywalker", "sthings")

	// The figures are fetched on the first render and are there for the next.
	f.do(http.MethodGet, "/ui/watch", nil)
	f.do(http.MethodGet, "/ui/matches/"+m.ID+"/figures/a.svg", nil)

	f.point(m, "b")
	_, body := f.do(http.MethodGet, "/ui/watch", nil)
	require.Contains(t, body, `style="--blade: #c05fd0"`)
	require.Contains(t, body, `style="--blade: #4d7c4a"`)
	require.Contains(t, cordPattern.FindString(body), `style="--paddle: #4d7c4a"`)
}

func TestTwoPlayersWhoChoseTheSameBladeAreStillToldApart(t *testing.T) {
	src := &fakeFigures{blades: map[string]string{"id-anna": "#159c8c", "id-bernd": "#159c8c"}}
	f := newFixture(t, WithFigures(src))
	m := f.reportedBetween("id-anna", "id-bernd")
	f.do(http.MethodGet, "/ui/matches/"+m.ID+"/figures/a.svg", nil)

	_, body := f.do(http.MethodGet, "/ui/watch", nil)
	require.Contains(t, body, `style="--blade: #159c8c"`)
	require.Contains(t, body, `style="--blade: #4d7c4a"`, "the next colour along")
}

func TestWithoutFiguresTheSpectatorViewIsUnchanged(t *testing.T) {
	f := newFixture(t)
	m := f.reportedBetween("id-anna", "id-bernd")

	_, body := f.do(http.MethodGet, "/ui/watch", nil)
	require.NotContains(t, body, `class="figure"`)
	require.NotRegexp(t, `class="side[^"]*dressed`, body)
	_, body = f.do(http.MethodGet, "/ui", nil)
	require.NotContains(t, body, `<img class="figure"`)

	rec, _ := f.do(http.MethodGet, "/ui/matches/"+m.ID+"/figures/a.svg", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// The stream that feeds the spectator view carries the faces of each point,
// with the streak it counts itself: angry by the third lost in a row.
func TestTheSpectatorStreamChangesTheFacesWithThePoints(t *testing.T) {
	f := newStreamFixture(t, WithFigures(&fakeFigures{}))
	r, _ := f.connectTo("/ui/matches/" + f.match.ID + "/stream?view=watch")
	require.Equal(t, []string{
		"/ui/matches/" + f.match.ID + "/figures/a.svg?face=neutral",
		"/ui/matches/" + f.match.ID + "/figures/b.svg?face=neutral",
	}, figureSrcs(t, readEvent(t, r).data))

	f.point(scorer.PlayerB)
	require.Equal(t, []string{
		"/ui/matches/" + f.match.ID + "/figures/a.svg?face=skeptical",
		"/ui/matches/" + f.match.ID + "/figures/b.svg?face=happy",
	}, figureSrcs(t, readEvent(t, r).data))

	f.point(scorer.PlayerB)
	readEvent(t, r)
	f.point(scorer.PlayerB)
	require.Contains(t, figureSrcs(t, readEvent(t, r).data)[0], "face=angry")
}

// The panel draws the names in the colours of the blades the players chose,
// resolved onto its own, and waits for the figures to have them, even on a
// first point that comes before anybody opened the view.
func TestThePanelGetsTheBladesThePlayersChose(t *testing.T) {
	src := &fakeFigures{blades: map[string]string{"skywalker": "#c05fd0", "sthings": "#4d7c4a"}}
	f := newFixture(t, WithFigures(src))
	m := f.reportedBetween("skywalker", "sthings")

	require.Equal(t, figure.Resolve("#c05fd0", "#4d7c4a"), f.server.Colours(context.Background(), m))
	require.Equal(t, [2]string{"#963cff", "#28dc28"}, f.server.Colours(context.Background(), m), "orchid is violet, moss green")
}

// Two players with one blade are parted by figure.Resolve alone, as the
// game's board parts them (pong's TestTheBoardPassesTheBladesOnOnceTheFiguresAreIn
// has the same pair). Moved apart first, as the cord moves them, petrol and
// petrol were teal and green on the matrix and teal and violet on the board.
func TestThePanelPartsTwoPlayersWithOneBladeAsTheGameDoes(t *testing.T) {
	src := &fakeFigures{blades: map[string]string{"id-anna": "#159c8c", "id-bernd": "#159c8c"}}
	f := newFixture(t, WithFigures(src))
	m := f.reportedBetween("id-anna", "id-bernd")

	require.Equal(t, figure.Resolve("#159c8c", "#159c8c"), f.server.Colours(context.Background(), m))
	require.Equal(t, [2]string{"#00e6a0", "#963cff"}, f.server.Colours(context.Background(), m))

	_, body := f.do(http.MethodGet, "/ui/watch", nil)
	require.Contains(t, body, `style="--blade: #4d7c4a"`, "the cord still moves b along its own colours")
}

func TestWithoutFiguresThePanelGetsTheColoursOfTheView(t *testing.T) {
	f := newFixture(t)
	m := f.reportedBetween("id-anna", "id-bernd")

	a, b := blades(m.Handover)
	require.Equal(t, figure.Resolve(a, b), f.server.Colours(context.Background(), m))
}
