package ui

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/live"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/panel"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// lifecycle stands in for *api.Server. It records what it was asked to do, so
// that a test can tell that starting a match from the page goes through the
// same door as POST /matches — which is what hands the LED panel over.
type lifecycle struct {
	registry *match.Registry
	started  []string
	ended    []string
	err      error
}

func (l *lifecycle) StartMatch(cfg scorer.Config) (*match.Match, error) {
	if l.err != nil {
		return nil, l.err
	}
	m, err := l.registry.Create(cfg)
	if err != nil {
		return nil, err
	}
	l.started = append(l.started, m.ID)
	return m, nil
}

func (l *lifecycle) EndMatch(m *match.Match) scorer.State {
	l.ended = append(l.ended, m.ID)
	m.End(time.Now())
	return m.Scorer.State()
}

type fixture struct {
	t        *testing.T
	registry *match.Registry
	hub      *live.Hub
	life     *lifecycle
	server   *Server
	event    uint64
}

func newFixture(t *testing.T, opts ...Option) *fixture {
	t.Helper()

	hub := live.New()
	registry := match.NewRegistry(match.WithObserver(hub.Observe))
	life := &lifecycle{registry: registry}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := func() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) }

	base := []Option{WithLogger(quiet), WithClock(now), WithHub(hub)}
	return &fixture{
		t:        t,
		registry: registry,
		hub:      hub,
		life:     life,
		server:   New(registry, life, append(base, opts...)...),
	}
}

func (f *fixture) do(method, path string, form url.Values) (*httptest.ResponseRecorder, string) {
	f.t.Helper()

	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	rec := httptest.NewRecorder()
	f.server.ServeHTTP(rec, req)
	return rec, rec.Body.String()
}

// match starts one the way the page does.
func (f *fixture) match(players ...string) *match.Match {
	f.t.Helper()

	form := url.Values{"best_of": {"3"}}
	if len(players) == 2 {
		form.Set("player_a", players[0])
		form.Set("player_b", players[1])
	}

	rec, _ := f.do(http.MethodPost, "/ui/matches", form)
	require.Equal(f.t, http.StatusOK, rec.Code)
	require.NotEmpty(f.t, f.life.started, "no match was started")

	m, err := f.registry.Get(f.life.started[len(f.life.started)-1])
	require.NoError(f.t, err)
	return m
}

// point clicks one, with a counter that goes up the way the page's does.
func (f *fixture) point(m *match.Match, player string) string {
	f.t.Helper()

	f.event++
	rec, body := f.do(http.MethodPost, "/ui/matches/"+m.ID+"/point", url.Values{
		"player":   {player},
		"source":   {"ui-test"},
		"event_id": {strconv.FormatUint(f.event, 10)},
	})
	require.Equal(f.t, http.StatusOK, rec.Code)
	return body
}

func TestThePageOffersTheFormWhenNothingIsRunning(t *testing.T) {
	f := newFixture(t)

	rec, body := f.do(http.MethodGet, "/ui", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	require.Contains(t, body, "No match is running")
	require.Contains(t, body, `hx-post="/ui/matches"`)
	require.NotContains(t, body, "sse-connect", "nothing to watch yet")
}

func TestThePageJoinsTheRunningMatch(t *testing.T) {
	f := newFixture(t)
	m := f.match("Anna", "Bernd")
	f.point(m, "a")

	_, body := f.do(http.MethodGet, "/ui", nil)

	require.Contains(t, body, "Anna")
	require.Contains(t, body, `sse-connect="/ui/matches/`+m.ID+`/stream"`)
	require.Contains(t, body, `<div class="points">1</div>`)
}

// A match nobody ended is the one a button would score into, so that is the one
// the page shows — the same rule POST /ingest/button resolves by.
func TestThePageShowsAFinishedMatchOnlyWhenAsked(t *testing.T) {
	f := newFixture(t)
	m := f.match("Anna", "Bernd")
	f.do(http.MethodPost, "/ui/matches/"+m.ID+"/end", nil)

	_, current := f.do(http.MethodGet, "/ui", nil)
	require.Contains(t, current, "No match is running")

	_, named := f.do(http.MethodGet, "/ui?match="+m.ID, nil)
	require.Contains(t, named, "Anna")
	require.Contains(t, named, "ended")
}

func TestAnUnknownMatchIsSaidRatherThanShown(t *testing.T) {
	f := newFixture(t)

	rec, body := f.do(http.MethodGet, "/ui?match=deadbeef", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, body, "no such match")
}

// The form is what nobody needs during a match and the first thing they need
// without one. Open while a match runs, it sat between the scorekeeper's thumb
// and the timeline for twenty minutes.
func TestTheNewMatchFormFoldsAwayWhileAMatchRuns(t *testing.T) {
	f := newFixture(t)
	openForm := regexp.MustCompile(`<details class="card newmatch" open>`)

	_, empty := f.do(http.MethodGet, "/ui", nil)
	require.Regexp(t, openForm, empty, "nothing is running, so the form is the page")

	m := f.match("Anna", "Bernd")
	_, running := f.do(http.MethodGet, "/ui", nil)
	require.Contains(t, running, `class="card newmatch"`)
	require.NotRegexp(t, openForm, running)

	f.do(http.MethodPost, "/ui/matches/"+m.ID+"/end", nil)
	_, ended := f.do(http.MethodGet, "/ui?match="+m.ID, nil)
	require.Regexp(t, openForm, ended, "an ended match is followed by the next one")
}

// The pills are radio buttons, and a radio button sends only what its name and
// value say. A renamed one would start every match as a best of five with the
// left player serving, and nothing on the page would look wrong.
func TestTheFormSendsTheFieldsTheHandlerReads(t *testing.T) {
	f := newFixture(t)

	_, body := f.do(http.MethodGet, "/ui", nil)

	for _, field := range []string{
		`name="player_a"`, `name="player_b"`,
		`name="best_of" value="3"`, `name="best_of" value="5" checked`, `name="best_of" value="7"`,
		`name="first_server" value="a" checked`, `name="first_server" value="b"`,
	} {
		require.Contains(t, body, field)
	}
}

// A won match says so on the winner's side. "ended" is for a match somebody
// stopped, and calling a win that would read as if nobody had won it.
func TestAWonMatchIsMarkedOnTheWinnersSide(t *testing.T) {
	f := newFixture(t)
	m := f.match("Anna", "Bernd")

	var body string
	for range 22 {
		body = f.point(m, "b")
	}
	require.True(t, m.Scorer.State().Complete)

	require.Regexp(t, `class="side winner">\s*<div class="name">Bernd</div>\s*<div class="status"><span class="state state-won">won</span>`, body)
	require.NotContains(t, body, "state-ended")
}

func TestStartingAMatchGoesThroughTheLifecycle(t *testing.T) {
	f := newFixture(t)

	rec, body := f.do(http.MethodPost, "/ui/matches", url.Values{
		"player_a": {"Ada"}, "player_b": {"Grace"}, "best_of": {"3"}, "first_server": {"b"},
	})

	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, f.life.started, 1, "the panel is handed over by StartMatch, not by the registry")
	require.Contains(t, body, "Ada")
	require.Contains(t, body, "Grace")
	// The whole live panel comes back, so the browser reconnects to the new match.
	require.Contains(t, body, `id="live"`)
	require.Contains(t, body, `sse-connect="/ui/matches/`+f.life.started[0]+`/stream"`)
}

func TestOnlyOneNameIsRejected(t *testing.T) {
	f := newFixture(t)

	rec, body := f.do(http.MethodPost, "/ui/matches", url.Values{"player_a": {"Ada"}})

	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, f.life.started)
	require.Contains(t, body, "name both of them")
}

// htmx does not swap a 4xx by default, so a refused click would leave the page
// as it was with nothing said. Every failure here is a 200 with the reason
// rendered into the partial instead.
func TestFailuresAnswerWithTheReasonRatherThanAStatus(t *testing.T) {
	f := newFixture(t)
	m := f.match("Anna", "Bernd")

	for _, tc := range []struct {
		name string
		path string
		form url.Values
		want string
	}{
		{"unknown match", "/ui/matches/deadbeef/point", url.Values{"player": {"a"}, "source": {"x"}, "event_id": {"1"}}, "no such match"},
		{"no source", "/ui/matches/" + m.ID + "/point", url.Values{"player": {"a"}, "event_id": {"1"}}, "source is required"},
		{"event id is not a counter", "/ui/matches/" + m.ID + "/point", url.Values{"player": {"a"}, "source": {"x"}, "event_id": {"soon"}}, "not a counter"},
		{"unknown player", "/ui/matches/" + m.ID + "/point", url.Values{"player": {"c"}, "source": {"x"}, "event_id": {"1"}}, "unknown player"},
		{"nothing to undo", "/ui/matches/" + m.ID + "/undo", nil, "nothing to undo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, body := f.do(http.MethodPost, tc.path, tc.form)

			require.Equal(t, http.StatusOK, rec.Code)
			require.Contains(t, body, `class="error"`)
			require.Contains(t, body, tc.want)
		})
	}
}

func TestAPointIsScoredAndCountedOnce(t *testing.T) {
	f := newFixture(t)
	m := f.match("Anna", "Bernd")

	body := f.point(m, "a")
	require.Contains(t, body, `<div class="points">1</div>`)

	// The same click posted twice — the counter has not moved, so this is the
	// retry ADR-0002 discards rather than a second point.
	_, again := f.do(http.MethodPost, "/ui/matches/"+m.ID+"/point", url.Values{
		"player": {"a"}, "source": {"ui-test"}, "event_id": {"1"},
	})
	require.Contains(t, again, `<div class="points">1</div>`)
	require.Equal(t, [2]int{1, 0}, m.Scorer.State().Points)
}

func TestScoringAnEndedMatchSaysSo(t *testing.T) {
	f := newFixture(t)
	m := f.match("Anna", "Bernd")

	rec, body := f.do(http.MethodPost, "/ui/matches/"+m.ID+"/end", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []string{m.ID}, f.life.ended, "the panel is given back by EndMatch")
	require.Contains(t, body, "disabled")

	_, refused := f.do(http.MethodPost, "/ui/matches/"+m.ID+"/point", url.Values{
		"player": {"a"}, "source": {"ui-test"}, "event_id": {"1"},
	})
	require.Contains(t, refused, "the match is over")
	require.Equal(t, [2]int{0, 0}, m.Scorer.State().Points)
}

// Undo is not gated on the match still running: taking back a wrongly awarded
// match point is exactly when it is needed, so the button stays live.
func TestUndoWorksAfterTheMatchIsOver(t *testing.T) {
	f := newFixture(t)
	m := f.match("Anna", "Bernd")
	f.point(m, "a")
	f.do(http.MethodPost, "/ui/matches/"+m.ID+"/end", nil)

	rec, body := f.do(http.MethodPost, "/ui/matches/"+m.ID+"/undo", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, body, `class="error"`)
	require.Equal(t, [2]int{0, 0}, m.Scorer.State().Points)
}

// The board shows the title the panel is given, so that the two can be
// disagreed with in one glance — and a set score has to read as one.
func TestTheBoardShowsWhatThePanelShows(t *testing.T) {
	f := newFixture(t)
	m := f.match("Anna", "Bernd")

	var body string
	for range 11 {
		body = f.point(m, "a")
	}

	st := m.Scorer.State()
	want := panel.Title(scorer.Transition{Kind: scorer.TransitionSetWon, State: st})
	require.Equal(t, "SET 1:0", want, "the panel's own wording changed; the board follows it")
	require.Contains(t, body, want)
	require.NotContains(t, body, `<strong>0:0</strong>`, "the point score would be ambiguous with a set score")
}

func TestPlayerNamesAreEscaped(t *testing.T) {
	f := newFixture(t)

	// The page is unauthenticated and a name typed into it is served back to
	// everyone watching the match.
	_, body := f.do(http.MethodPost, "/ui/matches", url.Values{
		"player_a": {`<script>alert(1)</script>`}, "player_b": {"Bernd"},
	})

	require.NotContains(t, body, "<script>alert(1)</script>")
	require.Contains(t, body, "&lt;script&gt;")
}

func TestWithoutAHubThePageStillScores(t *testing.T) {
	f := newFixture(t)
	f.server = New(f.registry, f.life, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))

	m := f.match("Anna", "Bernd")
	body := f.point(m, "a")

	require.Contains(t, body, `<div class="points">1</div>`)

	_, page := f.do(http.MethodGet, "/ui", nil)
	require.NotContains(t, page, "sse-connect")

	rec, _ := f.do(http.MethodGet, "/ui/matches/"+m.ID+"/stream", nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestKindOfReadsTheStateTheWayTheTransitionDid(t *testing.T) {
	point := scorer.State{Points: [2]int{3, 5}}
	setWon := scorer.State{CompletedSets: [][2]int{{11, 9}}}
	won := scorer.State{Complete: true, CompletedSets: [][2]int{{11, 9}, {11, 7}}}

	require.Equal(t, scorer.TransitionPoint, kindOf(point))
	require.Equal(t, scorer.TransitionSetWon, kindOf(setWon))
	require.Equal(t, scorer.TransitionMatchWon, kindOf(won))

	require.Equal(t, scorer.TransitionSetWon, kindBetween(point, setWon))
	require.Equal(t, scorer.TransitionMatchWon, kindBetween(setWon, won))
	require.Equal(t, scorer.TransitionPoint, kindBetween(point, scorer.State{Points: [2]int{4, 5}}))
}

// TestTheScoringButtonsCarryAnObjectLiteral guards the shape of hx-vals rather
// than its meaning. htmx wraps a js: expression that does not already begin
// with "{" in braces of its own, so `js:zwPoint("a")` is evaluated as
// `{zwPoint("a")}` — a SyntaxError raised before the request is built. The
// button then does nothing at all, with nothing in the response to show for it,
// which is why no other test here can see it.
func TestTheScoringButtonsCarryAnObjectLiteral(t *testing.T) {
	f := newFixture(t)
	f.match("Anna", "Bernd")

	_, body := f.do(http.MethodGet, "/ui", nil)

	vals := regexp.MustCompile(`hx-vals='([^']*)'`).FindAllStringSubmatch(body, -1)
	require.Len(t, vals, 2, "one per scoring button")

	for _, v := range vals {
		expr, ok := strings.CutPrefix(v[1], "js:")
		require.True(t, ok, "hx-vals %q is not a js: expression", v[1])
		require.True(t, strings.HasPrefix(expr, "{"),
			"hx-vals %q must be an object literal: htmx braces anything else into a SyntaxError", v[1])
	}
}
