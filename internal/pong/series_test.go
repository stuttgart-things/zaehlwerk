package pong

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// startSeries posts POST /series and returns the status and the decoded body.
func (f *pongFixture) startSeries(body any) (int, SeriesView) {
	f.t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(f.t, err)
	resp, err := http.Post(f.url+"/series", "application/json", bytes.NewReader(raw))
	require.NoError(f.t, err)
	defer func() { _ = resp.Body.Close() }()
	var v SeriesView
	if resp.StatusCode == http.StatusCreated {
		require.NoError(f.t, json.NewDecoder(resp.Body).Decode(&v))
	}
	return resp.StatusCode, v
}

func (f *pongFixture) getSeries(id string) SeriesView {
	f.t.Helper()
	resp, err := http.Get(f.url + "/series/" + id)
	require.NoError(f.t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(f.t, http.StatusOK, resp.StatusCode)
	var v SeriesView
	require.NoError(f.t, json.NewDecoder(resp.Body).Decode(&v))
	return v
}

// seriesOver waits for a series to stop and returns how it ended.
func (f *pongFixture) seriesOver(id string) SeriesView {
	f.t.Helper()
	var v SeriesView
	require.Eventually(f.t, func() bool {
		v = f.getSeries(id)
		return v.Status != SeriesPlaying
	}, 30*time.Second, 10*time.Millisecond)
	return v
}

func TestASeriesCountsEachFaultItInjects(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)

	status, v := p.startSeries(seriesBody{Games: 3, Faults: &Faults{WrongHalf: 0.1, Resend: 0.1, Lost: 0.1}})
	require.Equal(t, http.StatusCreated, status)
	v = p.seriesOver(v.ID)

	require.Equal(t, SeriesDone, v.Status, v.Reason)
	require.Equal(t, 3, v.Played)
	require.Equal(t, 3, v.Clean, "%+v", v.Findings)
	require.Empty(t, v.Findings)
	require.Zero(t, v.Unattributed)
	require.Len(t, v.Kinds, 3, "only the faults the series injects")
	for i, kind := range []FaultKind{FaultWrongHalf, FaultResend, FaultLost} {
		require.Equal(t, kind, v.Kinds[i].Kind, "in the order of the fault table")
		require.Positive(t, v.Kinds[i].Injected, kind)
		require.Zero(t, v.Kinds[i].Findings, kind)
		require.Equal(t, 1.0, v.Kinds[i].Accuracy, kind)
	}
}

// Without the quiet minute zaehlwerk rightly discards a counter reset, which
// the game calls a finding: the series counts it against the reset, the
// presses it discards after it as unattributed, and names the game to play
// again.
func TestASeriesCountsAFindingAgainstTheFaultAtItsPress(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)
	p.pong.resetQuiet = time.Millisecond

	status, v := p.startSeries(seriesBody{Games: 2, Faults: &Faults{CounterReset: 0.05}})
	require.Equal(t, http.StatusCreated, status)
	v = p.seriesOver(v.ID)

	require.Equal(t, SeriesDone, v.Status, v.Reason)
	require.Len(t, v.Kinds, 1)
	reset := v.Kinds[0]
	require.Equal(t, FaultCounterReset, reset.Kind)
	require.Positive(t, reset.Findings)
	require.Equal(t, float64(reset.Injected-reset.Findings)/float64(reset.Injected), reset.Accuracy)
	require.Less(t, reset.Accuracy, 1.0)
	require.Positive(t, v.Unattributed, "the presses after a discarded reset are discarded too")
	require.NotEmpty(t, v.Findings)

	g := p.get(v.Findings[0].Game)
	require.Equal(t, v.Findings[0].Seed, g.Seed)
	require.Equal(t, v.Findings[0].Unexplained, g.Unexplained)
}

func TestASeriesWithASeedPlaysTheSameGamesAgain(t *testing.T) {
	faults := Faults{WrongHalf: 0.1, Ghost: 0.1}
	seed := int64(40)
	play := func() []View {
		p := newPong(t, newTable(t).url, 1)
		status, v := p.startSeries(seriesBody{Games: 2, Faults: &faults, Seed: &seed})
		require.Equal(t, http.StatusCreated, status)
		require.Equal(t, SeriesDone, p.seriesOver(v.ID).Status)
		p.pong.mu.Lock()
		ids := append([]string(nil), p.pong.order...)
		p.pong.mu.Unlock()
		var games []View
		for _, g := range ids {
			games = append(games, p.get(g))
		}
		return games
	}

	a, b := play(), play()

	require.Len(t, a, 2)
	for i := range a {
		require.Equal(t, seed+int64(i), a[i].Seed)
		require.Equal(t, a[i].Seed, b[i].Seed)
		require.Equal(t, a[i].Faults, b[i].Faults)
		require.Equal(t, a[i].State.CompletedSets, b[i].State.CompletedSets)
	}
}

func TestOneSeriesAtATime(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)

	status, v := p.startSeries(seriesBody{Games: maxSeriesGames})
	require.Equal(t, http.StatusCreated, status)
	status, _ = p.startSeries(seriesBody{Games: 1})
	require.Equal(t, http.StatusConflict, status)

	require.Equal(t, SeriesPlaying, p.getSeries(v.ID).Status)
	status, _, _ = p.start("0s")
	require.Equal(t, http.StatusConflict, status, "the series has the table")
}

func TestPOSTSeriesRefusesWhatItCannotPlay(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)

	for _, body := range []string{
		`{}`,
		`{"games": 0}`,
		`{"games": 1001}`,
		`{"games": 1, "faults": {"lost": 1.5}}`,
		`{"games": 1, "pace": "1s"}`,
	} {
		resp, err := http.Post(p.url+"/series", "application/json", strings.NewReader(body))
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
	}
	resp, err := http.Get(p.url + "/series/nope")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// A game GET /games/{id} showed is played again by its seed: the same rallies
// and the same faults.
func TestAGameIsPlayedAgainByItsSeed(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)
	faults := Faults{WrongHalf: 0.1, Lost: 0.1}
	first := p.faulty(faults)
	p.faulty(faults) // the service's next seed is another game

	seed := first.Seed
	status, v, raw := p.startWith(startBody{Pace: "0s", Faults: &faults, Seed: &seed})
	require.Equal(t, http.StatusCreated, status, raw)
	again := p.finished(v.ID)

	require.Equal(t, first.Seed, again.Seed)
	require.Equal(t, first.Faults, again.Faults)
	require.Equal(t, first.State.CompletedSets, again.State.CompletedSets)

	for _, driver := range []string{"browser", "check"} {
		status, _, raw := p.startWith(map[string]any{"driver": driver, "seed": 3})
		require.Equal(t, http.StatusBadRequest, status, raw)
	}
}
