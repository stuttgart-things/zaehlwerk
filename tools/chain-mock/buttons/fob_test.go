package buttons

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/api"
	"github.com/stuttgart-things/zaehlwerk/internal/checkin"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
	"github.com/stuttgart-things/zaehlwerk/tools/chain-mock/fakesp"
)

// checkedInTable is a zaehlwerk with check-in on, against the fake
// Schmetterpause, and no match running.
func checkedInTable(t *testing.T) (*httptest.Server, *match.Registry, *fakesp.Server) {
	t.Helper()
	fake, err := fakesp.New("tok", fakesp.ModeAccept, quiet)
	require.NoError(t, err)
	spSrv := httptest.NewServer(fake.Handler())
	t.Cleanup(spSrv.Close)
	sp, err := schmetterpause.New(schmetterpause.Config{BaseURL: spSrv.URL, Token: "tok"})
	require.NoError(t, err)

	registry := match.NewRegistry()
	srv := httptest.NewServer(api.New(registry, api.WithLogger(quiet),
		api.WithCheckin(checkin.NewDesk(sp, checkin.WithLogger(quiet)), testHubToken)))
	t.Cleanup(srv.Close)
	return srv, registry, fake
}

// roster is the fake's players as the real one lists them.
func roster() []schmetterpause.Player {
	var players []schmetterpause.Player
	for _, p := range fakesp.DefaultRoster {
		if !p.Observer {
			players = append(players, schmetterpause.Player{ID: p.ID.String(), DisplayName: p.DisplayName, TTR: p.TTR})
		}
	}
	return players
}

// fob is the page's fob with label, from the fake's roster.
func fob(t *testing.T, label string) Fob {
	t.Helper()
	for _, f := range FobsFor(roster()) {
		if f.Label == label {
			return f
		}
	}
	t.Fatalf("no fob %q", label)
	return Fob{}
}

func TestThePageOffersAFobPerPlayerAndTwoNobodyOwns(t *testing.T) {
	var labels []string
	for _, f := range FobsFor(roster()) {
		labels = append(labels, f.Label)
	}
	require.Equal(t, []string{"Anna's fob", "Bernd's fob", "Clara's fob", "a new fob", "a stranger's badge"}, labels)
	require.Equal(t, []Fob{SpareFob, StrangerFob}, FobsFor(nil), "without a roster, only the two")
}

func TestAFobThenAPressChecksInAtThatHalf(t *testing.T) {
	zw, registry, fake := checkedInTable(t)
	r := rig(t, zw, nil)

	r.HoldFob(fob(t, "Anna's fob"))
	require.Equal(t, "Anna's fob", r.FobHeld())
	press(t, r, "A", 100*time.Millisecond)
	require.Empty(t, r.FobHeld(), "the press used the fob up")

	r.HoldFob(fob(t, "Bernd's fob"))
	press(t, r, "B", 100*time.Millisecond)

	m, ok := registry.Occupied()
	require.True(t, ok, "two halves checked in start the match")
	require.Equal(t, [2]string{"Anna", "Bernd"}, m.Scorer.State().Players)
	require.Len(t, fake.Checkins(), 2)
	require.Zero(t, m.Scorer.State().Points, "a check-in press is not a point")
}

func TestARetriedCheckInFromTheHubResolvesOnce(t *testing.T) {
	zw, _, fake := checkedInTable(t)
	r := rig(t, zw, func(s *Settings) { s.APIResponseLost = true })

	r.HoldFob(fob(t, "Anna's fob"))
	press(t, r, "A", 100*time.Millisecond)

	require.Len(t, fake.Checkins(), 1)
	var log strings.Builder
	for _, e := range r.Entries() {
		log.WriteString(e.Text + "\n")
	}
	require.Contains(t, log.String(), "resolved nothing")
}

func TestTheMockNeverLogsAUID(t *testing.T) {
	zw, _, _ := checkedInTable(t)
	r := rig(t, zw, nil)

	fobs := FobsFor(roster())
	for _, f := range fobs {
		r.HoldFob(f)
		press(t, r, "A", 100*time.Millisecond)
	}
	var log strings.Builder
	for _, e := range r.Entries() {
		log.WriteString(e.Text + "\n")
	}
	require.Contains(t, log.String(), "unknown fob")
	for _, f := range fobs {
		require.NotContains(t, log.String(), f.UID)
	}
}

func TestAFobWithoutAPressIsDropped(t *testing.T) {
	zw, registry, fake := checkedInTable(t)
	r := rig(t, zw, nil)

	r.HoldFob(fob(t, "Clara's fob"))
	r.mu.Lock()
	r.fob.timer.Reset(10 * time.Millisecond)
	r.mu.Unlock()
	require.Eventually(t, func() bool { return r.FobHeld() == "" }, time.Second, 5*time.Millisecond)

	// The press after the window is an ordinary press again; with no match
	// running it goes nowhere, and nothing checked in.
	require.NoError(t, r.Press(context.Background(), "A", 100*time.Millisecond))
	_, ok := registry.Occupied()
	require.False(t, ok)
	require.Empty(t, fake.Checkins())
}

func TestTheReaderSaysWhatTheCheckInAnswered(t *testing.T) {
	zw, _, _ := checkedInTable(t)
	r := rig(t, zw, nil)
	anna := fob(t, "Anna's fob")

	r.HoldFob(anna)
	reader := r.Reader()
	require.True(t, reader.Waiting, "the LED blinks while the fob waits")
	require.Equal(t, "Anna's fob", reader.Label)
	require.Greater(t, reader.Left, time.Duration(0))
	require.LessOrEqual(t, reader.Left, FobWindow)

	press(t, r, "A", 100*time.Millisecond)
	reader = r.Reader()
	require.False(t, reader.Waiting)
	require.True(t, reader.OK, "green: %s", reader.Result)
	require.Contains(t, reader.Result, "Anna checked in")
	require.Equal(t, FobAnswer{Owner: "Anna", Outcome: "claimed"}, r.FobAnswers()[anna.Key])

	r.HoldFob(StrangerFob)
	press(t, r, "B", 100*time.Millisecond)
	reader = r.Reader()
	require.False(t, reader.OK, "red: %s", reader.Result)
	require.Contains(t, reader.Result, "unknown fob")
	require.Equal(t, FobAnswer{Outcome: "unknown"}, r.FobAnswers()[StrangerFob.Key])
}

func TestAFobLeftWaitingTurnsTheLEDRed(t *testing.T) {
	zw, _, _ := checkedInTable(t)
	r := rig(t, zw, nil)

	r.HoldFob(SpareFob)
	r.mu.Lock()
	r.fob.timer.Reset(10 * time.Millisecond)
	r.mu.Unlock()
	require.Eventually(t, func() bool { return !r.Reader().Waiting }, time.Second, 5*time.Millisecond)
	reader := r.Reader()
	require.False(t, reader.OK)
	require.Contains(t, reader.Result, "no press in time")
	require.Empty(t, r.FobAnswers(), "nothing was asked, so nothing is known about the fob")
}

// rosterFunc is a Roster from a function.
type rosterFunc func(context.Context) ([]schmetterpause.Player, error)

func (f rosterFunc) Players(ctx context.Context) ([]schmetterpause.Player, error) { return f(ctx) }

func getPage(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode, string(body)
}

func postFob(t *testing.T, page, key string) (int, string) {
	t.Helper()
	resp, err := http.PostForm(page+"/fob", url.Values{"fob": {key}})
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode, string(body)
}

func TestThePageOffersTheRostersFobsAndHoldsOne(t *testing.T) {
	zw, _, _ := checkedInTable(t)
	r := rig(t, zw, nil)
	players := []schmetterpause.Player{{ID: "5a1e5a1e-0000-4000-8000-000000000001", DisplayName: "sthings"}}
	page := httptest.NewServer(NewServer(r, zw.URL+"/ui", "", quiet).
		WithRoster(rosterFunc(func(context.Context) ([]schmetterpause.Player, error) { return players, nil })))
	t.Cleanup(page.Close)

	status, body := getPage(t, page.URL+"/")
	require.Equal(t, http.StatusOK, status)
	for _, want := range []string{`data-fob="5a1e5a1e-0000-4000-8000-000000000001"`, "sthings&#39;s fob", "a new fob", "a stranger&#39;s badge"} {
		require.Contains(t, body, want)
	}
	require.NotContains(t, body, "Anna", "the fake's names are not on a page for another roster")

	status, body = postFob(t, page.URL, players[0].ID)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, `data-waiting="1"`)
	require.Contains(t, body, "press a half now")
	require.Equal(t, "sthings's fob", r.FobHeld())

	// Against the fake, this player's fob is nobody's: what the list says
	// comes from the answer.
	press(t, r, "A", 100*time.Millisecond)
	_, body = getPage(t, page.URL+"/reader")
	require.Contains(t, body, "Schmetterpause: unknown")
	require.NotContains(t, body, `data-waiting`)
}

func TestAFobThatIsNotOnThePageIsRefused(t *testing.T) {
	zw, _, _ := checkedInTable(t)
	r := rig(t, zw, nil)
	page := httptest.NewServer(NewServer(r, zw.URL+"/ui", "", quiet))
	t.Cleanup(page.Close)

	status, _ := postFob(t, page.URL, "11111111-1111-4111-8111-111111111111")
	require.Equal(t, http.StatusBadRequest, status, "no roster, so no player's fob")
	require.Empty(t, r.FobHeld())

	status, _ = postFob(t, page.URL, SpareFob.Key)
	require.Equal(t, http.StatusOK, status)
}

func TestThePageSaysWhyThereAreNoPlayersFobs(t *testing.T) {
	zw, _, _ := checkedInTable(t)
	broken := rosterFunc(func(context.Context) ([]schmetterpause.Player, error) {
		return nil, errors.New("schmetterpause: fetching players: 401")
	})
	for name, s := range map[string]*Server{
		"unset":  NewServer(rig(t, zw, nil), zw.URL+"/ui", "", quiet),
		"broken": NewServer(rig(t, zw, nil), zw.URL+"/ui", "", quiet).WithRoster(broken),
	} {
		t.Run(name, func(t *testing.T) {
			page := httptest.NewServer(s)
			t.Cleanup(page.Close)
			_, body := getPage(t, page.URL+"/")
			require.Contains(t, body, "no player list")
			require.Contains(t, body, "a new fob")
		})
	}
}

func TestNoUIDIsEverOnThePage(t *testing.T) {
	zw, _, _ := checkedInTable(t)
	r := rig(t, zw, nil)
	page := httptest.NewServer(NewServer(r, zw.URL+"/ui", "", quiet).
		WithRoster(rosterFunc(func(context.Context) ([]schmetterpause.Player, error) { return roster(), nil })))
	t.Cleanup(page.Close)

	_, body := getPage(t, page.URL+"/")
	for _, f := range FobsFor(roster()) {
		_, held := postFob(t, page.URL, f.Key)
		press(t, r, "A", 100*time.Millisecond)
		_, reader := getPage(t, page.URL+"/reader")
		for _, html := range []string{body, held, reader} {
			require.NotContains(t, strings.ToUpper(html), f.UID)
		}
	}
}
