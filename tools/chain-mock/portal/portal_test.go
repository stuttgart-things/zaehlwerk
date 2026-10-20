package portal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/api"
	"github.com/stuttgart-things/zaehlwerk/internal/checkin"
	"github.com/stuttgart-things/zaehlwerk/internal/live"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
	"github.com/stuttgart-things/zaehlwerk/tools/chain-mock/fakesp"
	"github.com/stuttgart-things/zaehlwerk/tools/chain-mock/piezo"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// nowhere is an address nothing listens on: a part that is not started.
const nowhere = "http://127.0.0.1:1"

const anna = "11111111-1111-4111-8111-111111111111"

// chain is the parts the portal looks at, each the real thing where this
// repository has it: a zaehlwerk API with its table stream and phone check-in,
// the fake Schmetterpause claiming at it, and a piezo board's control
// endpoint. Anything not given is nowhere.
type chain struct {
	targets  Targets
	registry *match.Registry
	fake     *fakesp.Server
	board    *piezo.Board
}

func newChain(t *testing.T) *chain {
	t.Helper()

	// zaehlwerk's address first, so the fake knows where to claim.
	zw := httptest.NewUnstartedServer(nil)
	fake, err := fakesp.New("secret", fakesp.ModeAccept, quiet)
	require.NoError(t, err)
	fake.Zaehlwerk = "http://" + zw.Listener.Addr().String()
	sp := httptest.NewServer(fake.Handler())
	t.Cleanup(sp.Close)

	client, err := schmetterpause.New(schmetterpause.Config{BaseURL: sp.URL, Token: "secret", Timeout: time.Second})
	require.NoError(t, err)
	hub := live.New()
	registry := match.NewRegistry(match.WithObserver(hub.Observe))
	desk := checkin.NewDesk(client, checkin.WithLogger(quiet))
	zw.Config.Handler = api.New(registry, api.WithLogger(quiet), api.WithHub(hub),
		api.WithPhoneCheckin(desk, "secret"))
	zw.Start()
	t.Cleanup(zw.Close)

	board := piezo.New(piezo.Config{Pace: time.Second}, quiet)
	ctl := httptest.NewServer(board.ControlHandler())
	t.Cleanup(ctl.Close)

	return &chain{
		targets: Targets{
			Zaehlwerk:      Part{URL: zw.URL},
			Simulator:      Part{URL: nowhere},
			Schmetterpause: Part{URL: sp.URL},
			Buttons:        Part{URL: nowhere},
			Piezo:          Part{URL: ctl.URL},
			Pong:           Part{URL: nowhere},
		},
		registry: registry,
		fake:     fake,
		board:    board,
	}
}

func (c *chain) portal(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(New(c.targets, quiet))
	t.Cleanup(srv.Close)
	return srv.URL
}

func get(t *testing.T, u string) string {
	t.Helper()
	resp, err := http.Get(u)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

func post(t *testing.T, u string, form map[string]string) string {
	t.Helper()
	vals := url.Values{}
	for k, v := range form {
		vals.Set(k, v)
	}
	resp, err := http.PostForm(u, vals)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode, "a failed button is drawn, not answered with an error")
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

func TestTheStatusRowSaysWhichPartsAreUpAndWhichAreNotRunning(t *testing.T) {
	c := newChain(t)
	body := get(t, c.portal(t)+"/status")

	require.Contains(t, body, `class="probe up" href="`+c.targets.Zaehlwerk.URL+`"`)
	require.Contains(t, body, "fake, accept", "the fake is told apart by its own /mode")
	require.Contains(t, body, `class="probe" href="`+nowhere+`"`)
	require.Contains(t, body, "not running", "a refused connection reads as a part not started")
}

func TestARealSchmetterpauseGetsAFrameAndTheFakeDoesNot(t *testing.T) {
	c := newChain(t)
	require.NotContains(t, get(t, c.portal(t)+"/"), `<iframe src="`+c.targets.Schmetterpause.URL,
		"the fake has no page; its card is the page")

	// The real one has no /mode and answers /readyz.
	realSP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mode" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(realSP.Close)
	c.targets.Schmetterpause = Part{URL: realSP.URL}
	page := c.portal(t)

	require.Contains(t, get(t, page+"/"), `<iframe src="`+realSP.URL+`/"`)
	require.Contains(t, get(t, page+"/status"), "real")
}

func TestTheFramesShownAreTheOnesInTheQuery(t *testing.T) {
	c := newChain(t)
	body := get(t, c.portal(t)+"/?show=watch&show=pong")

	require.Contains(t, body, `<iframe src="`+c.targets.Zaehlwerk.URL+`/ui/watch"`)
	require.Contains(t, body, `<iframe src="`+nowhere+`/"`, "pong's frame")
	require.NotContains(t, body, `<iframe src="`+c.targets.Zaehlwerk.URL+`/ui"`, "the scoring page was not picked")
}

func TestTheMatchButtonsStartAndEndAMatchOnZaehlwerk(t *testing.T) {
	c := newChain(t)
	page := c.portal(t)

	body := post(t, page+"/match/start", map[string]string{"best_of": "1"})
	m, err := c.registry.Current()
	require.NoError(t, err)
	require.Equal(t, [2]string{"Anna", "Bernd"}, m.Scorer.State().Players)
	require.Contains(t, body, "start match: 201")
	require.Contains(t, body, "Anna 0:0 Bernd")

	body = post(t, page+"/match/end", map[string]string{"match_id": m.ID})
	_, err = c.registry.Current()
	require.Error(t, err, "nothing is running any more")
	require.Contains(t, body, "No match running.")
}

func TestThePhoneButtonsCheckAPlayerInThroughTheFake(t *testing.T) {
	c := newChain(t)
	body := post(t, c.portal(t)+"/sp/phone", map[string]string{"half": "left", "player_id": anna})

	require.Contains(t, body, "phone check-in: 200", "zaehlwerk's answer, passed through by the fake")
	require.Contains(t, body, "claimed")
	checkins := c.fake.Checkins()
	require.Len(t, checkins, 1)
	require.Equal(t, "Anna", checkins[0].Player)
	require.Equal(t, "left", checkins[0].Half)
}

func TestTheModeButtonsSwitchTheFake(t *testing.T) {
	c := newChain(t)
	page := c.portal(t)

	post(t, page+"/sp/mode", map[string]string{"mode": fakesp.ModeRefuse})
	require.Equal(t, fakesp.ModeRefuse, c.fake.Mode())

	body := post(t, page+"/sp/mode", map[string]string{"mode": "sideways"})
	require.Equal(t, fakesp.ModeRefuse, c.fake.Mode())
	require.Contains(t, body, `class="card bad"`, "the fake's refusal is shown, not swallowed")
}

func TestThePiezoButtonPausesOnlyAndKeepsTheOtherKnobs(t *testing.T) {
	c := newChain(t)
	page := c.portal(t)
	ctl := c.board.Control()
	ctl.Resend = 0.5
	require.NoError(t, c.board.SetControl(ctl))

	post(t, page+"/piezo", map[string]string{"action": "pause"})
	require.True(t, c.board.Control().Paused)
	require.InDelta(t, 0.5, c.board.Control().Resend, 1e-9, "the buttons page's knob is left alone")

	post(t, page+"/piezo", map[string]string{"action": "resume"})
	require.False(t, c.board.Control().Paused)
}

func TestThePongButtonStartsAHeadlessGameAtItsPace(t *testing.T) {
	c := newChain(t)
	var got map[string]string
	pong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/games", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"g1"}`))
	}))
	t.Cleanup(pong.Close)
	c.targets.Pong = Part{URL: pong.URL}

	body := post(t, c.portal(t)+"/pong/game", map[string]string{"pace": "200ms"})
	require.Equal(t, map[string]string{"pace": "200ms"}, got)
	require.Contains(t, body, "pong game: 201")
}

func TestTheEventLogIsZaehlwerksTableStreamRelayed(t *testing.T) {
	c := newChain(t)
	resp, err := http.Get(c.portal(t) + "/events")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	lines := bufio.NewScanner(resp.Body)
	require.True(t, lines.Scan())
	require.Equal(t, `data: {"kind":"no_match"}`, strings.TrimSpace(lines.Text()),
		"on connect, with nothing running, the table stream says so")
}

func TestThePortalStillDrawsWithEveryPartDown(t *testing.T) {
	down := Part{URL: nowhere}
	page := httptest.NewServer(New(Targets{
		Zaehlwerk: down, Simulator: down, Schmetterpause: down,
		Buttons: down, Piezo: down, Pong: down,
	}, quiet))
	t.Cleanup(page.Close)

	body := get(t, page.URL+"/")
	require.Contains(t, body, "Zählwerk dev portal")
	require.Contains(t, body, "zaehlwerk: not running")
	require.Contains(t, body, "task chain:piezo JOIN=1")
}

func TestThePageFramesThePublicAddressAndCallsTheOtherOne(t *testing.T) {
	c := newChain(t)
	c.targets.Zaehlwerk.Public = "https://zaehlwerk.example.com"
	page := c.portal(t)

	body := get(t, page+"/?show=scoring")
	require.Contains(t, body, `<iframe src="https://zaehlwerk.example.com/ui"`, "the browser gets the route")
	require.Contains(t, get(t, page+"/status"), `class="probe up" href="https://zaehlwerk.example.com"`,
		"probed at the Service, linked at the route")
}

func TestAPartThatIsOffIsNotOnThePageAtAll(t *testing.T) {
	c := newChain(t)
	c.targets.Buttons, c.targets.Piezo, c.targets.Pong, c.targets.Simulator = Part{}, Part{}, Part{}, Part{}
	page := c.portal(t)

	status := get(t, page+"/status")
	require.NotContains(t, status, "Buttons")
	require.NotContains(t, status, "not running", "an absent part is not a part that is down")

	body := get(t, page+"/?show=buttons&show=scoring")
	require.NotContains(t, body, `value="buttons"`, "no frame to pick")
	require.NotContains(t, body, "Piezo board")
	require.NotContains(t, body, "Bot game")
	require.Contains(t, body, `<iframe src="`+c.targets.Zaehlwerk.URL+`/ui"`)
}

func TestOffSwitchesAPartOffAndUnsetKeepsTheLaptopDefault(t *testing.T) {
	t.Setenv("BUTTONS_URL", "off")
	t.Setenv("PONG_URL", "http://zaehlwerk-pong/")
	t.Setenv("PONG_PUBLIC_URL", "https://pong.example.com/")

	require.False(t, part("BUTTONS", "http://localhost:8084").On())
	require.Equal(t, Part{URL: "http://localhost:8081"}, part("SIMULATOR", "http://localhost:8081"))
	require.Equal(t, Part{URL: "http://zaehlwerk-pong", Public: "https://pong.example.com"},
		part("PONG", "http://localhost:8087"))
}

func TestTheEventLogPadsKindsToTheLongestAndKeepsThemApartFromTheName(t *testing.T) {
	for _, kind := range logKinds {
		require.LessOrEqual(t, len(kind), kindWidth, kind)
	}
	require.Equal(t, len(live.KindStarted), kindWidth, "match_started and serve_decided are the longest today")

	body := get(t, newChain(t).portal(t)+"/")
	require.Contains(t, body, fmt.Sprintf("ev.kind.padEnd( %d ) + ' ' + s.players[0]", kindWidth),
		"warmup_over ran into the player's name at a guessed width of 10")
}

func TestTheSideColumnCannotGrowUnderTheFrames(t *testing.T) {
	body := get(t, newChain(t).portal(t)+"/")
	require.Contains(t, body, "aside, #side { display: grid; gap: 1rem; grid-template-columns: minmax(0, 1fr); }",
		"an implicit auto column grows to #log's longest line")
	require.Contains(t, body, ".card { min-width: 0;")
}
