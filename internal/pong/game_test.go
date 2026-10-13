package pong

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/api"
	"github.com/stuttgart-things/zaehlwerk/internal/live"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// The game plays against the real zaehlwerk handlers, in this process. Only
// the tests import them: the game itself knows zaehlwerk by its HTTP API, and
// these tests are what notices when that API and the game stop agreeing.

type table struct {
	t         *testing.T
	registry  *match.Registry
	zaehlwerk *api.Server
	url       string
}

func newTable(t *testing.T) *table {
	t.Helper()
	hub := live.New()
	registry := match.NewRegistry(match.WithObserver(hub.Observe))
	zw := api.New(registry, api.WithLogger(quiet()), api.WithHub(hub))
	srv := httptest.NewServer(zw)
	t.Cleanup(srv.Close)
	return &table{t: t, registry: registry, zaehlwerk: zw, url: srv.URL}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type pongFixture struct {
	t    *testing.T
	pong *Server
	url  string
}

func newPong(t *testing.T, zaehlwerkURL string, seed int64) *pongFixture {
	t.Helper()
	p := New(Config{
		ZaehlwerkURL: zaehlwerkURL,
		BestOf:       3,
		Skills:       [2]Skill{skills["mittel"], skills["mittel"]},
		Seed:         seed,
	}, quiet())
	p.client.retryAfter = time.Millisecond
	srv := httptest.NewServer(p)
	t.Cleanup(func() {
		srv.Close()
		// Not t.Context(): it is already cancelled when cleanups run.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, p.Close(ctx))
	})
	return &pongFixture{t: t, pong: p, url: srv.URL}
}

// start posts POST /games and returns the status and the decoded body.
func (f *pongFixture) start(pace string) (int, View, string) {
	f.t.Helper()
	body, err := json.Marshal(startBody{Pace: pace})
	require.NoError(f.t, err)
	resp, err := http.Post(f.url+"/games", "application/json", bytes.NewReader(body))
	require.NoError(f.t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(f.t, err)

	var v View
	_ = json.Unmarshal(raw, &v)
	return resp.StatusCode, v, string(raw)
}

func (f *pongFixture) get(id string) View {
	f.t.Helper()
	resp, err := http.Get(f.url + "/games/" + id)
	require.NoError(f.t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(f.t, http.StatusOK, resp.StatusCode)
	var v View
	require.NoError(f.t, json.NewDecoder(resp.Body).Decode(&v))
	return v
}

// finished waits for a game to stop and returns how it ended.
func (f *pongFixture) finished(id string) View {
	f.t.Helper()
	var v View
	require.Eventually(f.t, func() bool {
		v = f.get(id)
		return v.Status != StatusPlaying
	}, 10*time.Second, 5*time.Millisecond)
	return v
}

// playUntilADecidingSet plays games until one goes the distance, so the
// change of ends at five is played whatever the bots draw. The seed decides,
// so it is the same game every run.
func playUntilADecidingSet(t *testing.T, p *pongFixture) []View {
	t.Helper()
	var played []View
	for range 10 {
		status, v, raw := p.start("0s")
		require.Equal(t, http.StatusCreated, status, raw)
		v = p.finished(v.ID)
		played = append(played, v)
		if v.State.SetNumber == 3 {
			return played
		}
	}
	t.Fatalf("ten best of threes and none went to a third set: %+v", played)
	return nil
}

func TestABotMatchAgreesWithZaehlwerkAtEveryRally(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)

	for _, v := range playUntilADecidingSet(t, p) {
		require.Equal(t, StatusWon, v.Status, v.Reason)
		require.Empty(t, v.Divergences)
		require.Equal(t, v.Truth, v.Zaehlwerk)
		require.Equal(t, v.Rallies, v.Truth[0]+v.Truth[1])
		require.True(t, v.State.Complete)
		require.Equal(t, scorer.PhaseComplete, v.State.Phase)
	}
}

func TestADecidingSetChangesEndsAtFive(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)

	played := playUntilADecidingSet(t, p)
	decider := played[len(played)-1]

	// After the first set, after the second, and at five in the third.
	require.Len(t, decider.EndsChanges, 3)
	require.Equal(t, EndsChange{Set: 2, Points: [2]int{0, 0}}, decider.EndsChanges[0])
	require.Equal(t, EndsChange{Set: 3, Points: [2]int{0, 0}}, decider.EndsChanges[1])
	atFive := decider.EndsChanges[2]
	require.Equal(t, 3, atFive.Set)
	require.Equal(t, 5, max(atFive.Points[0], atFive.Points[1]))
}

func TestTheSameSeedPlaysTheSameMatch(t *testing.T) {
	first := newPong(t, newTable(t).url, 7)
	second := newPong(t, newTable(t).url, 7)

	_, a, _ := first.start("0s")
	_, b, _ := second.start("0s")
	require.Equal(t, first.finished(a.ID).State.CompletedSets, second.finished(b.ID).State.CompletedSets)
}

func TestAPongMatchIsNeverReported(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)

	status, v, raw := p.start("0s")
	require.Equal(t, http.StatusCreated, status, raw)

	m, err := tbl.registry.Get(v.MatchID)
	require.NoError(t, err)
	require.False(t, m.Handover.Wanted())
	require.Equal(t, BotNames, m.Scorer.State().Players)
	p.finished(v.ID)
}

// recorder forwards to zaehlwerk and keeps every ingest body it saw.
type recorder struct {
	next http.Handler
	mu   sync.Mutex
	sent []map[string]any
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/ingest/web" {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		rec.mu.Lock()
		rec.sent = append(rec.sent, body)
		rec.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(raw))
	}
	rec.next.ServeHTTP(w, r)
}

func TestAGameReportsHalvesUnderItsOwnSourceWithItsOwnCounter(t *testing.T) {
	tbl := newTable(t)
	rec := &recorder{next: tbl.zaehlwerk}
	proxy := httptest.NewServer(rec)
	t.Cleanup(proxy.Close)
	p := newPong(t, proxy.URL, 1)

	_, v, _ := p.start("0s")
	v = p.finished(v.ID)
	require.Equal(t, "pong-"+v.ID, v.Source)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.NotEmpty(t, rec.sent)
	for i, body := range rec.sent {
		require.Equal(t, v.MatchID, body["match_id"])
		require.Equal(t, v.Source, body["source"])
		require.Contains(t, []any{"left", "right"}, body["half"])
		require.NotContains(t, body, "player", "a half, never a player")
		require.EqualValues(t, 1, body["delta"])
		require.EqualValues(t, i+1, body["event_id"], "every report takes the next number")
	}
}

func TestAGameDoesNotStartWhileAReportedMatchIsRunning(t *testing.T) {
	tbl := newTable(t)
	real, err := tbl.zaehlwerk.StartMatch(scorer.Config{Players: [2]string{"Anna", "Bernd"}},
		match.Handover{HomeID: "h1", AwayID: "a1", OperatorID: "o1"})
	require.NoError(t, err)
	p := newPong(t, tbl.url, 1)

	status, _, raw := p.start("0s")
	require.Equal(t, http.StatusConflict, status)
	require.Contains(t, raw, "reported to Schmetterpause")

	cur, err := tbl.registry.Current()
	require.NoError(t, err)
	require.Equal(t, real.ID, cur.ID, "the game created no match of its own")
}

func TestAGameDoesNotStartWhileSomebodyIsScoringAMatch(t *testing.T) {
	tbl := newTable(t)
	m, err := tbl.zaehlwerk.StartMatch(scorer.Config{}, match.Handover{})
	require.NoError(t, err)
	p := newPong(t, tbl.url, 1)

	status, _, raw := p.start("0s")
	require.Equal(t, http.StatusConflict, status)
	require.Contains(t, raw, m.ID+" is running")

	cur, err := tbl.registry.Current()
	require.NoError(t, err)
	require.Equal(t, m.ID, cur.ID, "the table still belongs to the match that was there")
}

func TestOneGameAtATime(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)

	status, first, _ := p.start("50ms")
	require.Equal(t, http.StatusCreated, status)

	status, _, raw := p.start("0s")
	require.Equal(t, http.StatusConflict, status)
	require.Contains(t, raw, first.ID+" is still playing")
}

func TestAGameEndsItsMatchWhenAnotherMatchTakesTheTable(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)

	status, v, _ := p.start("20ms")
	require.Equal(t, http.StatusCreated, status)
	require.Eventually(t, func() bool { return p.get(v.ID).State.Phase == scorer.PhasePlaying },
		5*time.Second, 5*time.Millisecond)

	_, err := tbl.zaehlwerk.StartMatch(scorer.Config{Players: [2]string{"Anna", "Bernd"}},
		match.Handover{HomeID: "h1", AwayID: "a1", OperatorID: "o1"})
	require.NoError(t, err)

	v = p.finished(v.ID)
	require.Equal(t, StatusEnded, v.Status)
	require.Equal(t, errDisplaced.Error(), v.Reason)
	ours, err := tbl.registry.Get(v.MatchID)
	require.NoError(t, err)
	require.False(t, ours.Running(), "the game's match was ended, not left behind")
}

func TestAGameEndedOnThePageStopsReporting(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)

	_, v, _ := p.start("20ms")
	m, err := tbl.registry.Get(v.MatchID)
	require.NoError(t, err)
	tbl.zaehlwerk.EndMatch(m)

	v = p.finished(v.ID)
	require.Equal(t, StatusEnded, v.Status)
}

func TestAShutdownEndsTheRunningGamesMatch(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)

	_, v, _ := p.start("20ms")
	require.NoError(t, p.pong.Close(t.Context()))

	m, err := tbl.registry.Get(v.MatchID)
	require.NoError(t, err)
	require.False(t, m.Running())
	require.Equal(t, StatusEnded, p.get(v.ID).Status)
}

// lossy forwards to zaehlwerk and loses the answer to the first ingest, as a
// flaky network does: zaehlwerk applied the event, the game never heard.
type lossy struct {
	next http.Handler
	lost atomic.Bool
}

func (l *lossy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/ingest/web" && l.lost.CompareAndSwap(false, true) {
		l.next.ServeHTTP(httptest.NewRecorder(), r)
		http.Error(w, "upstream went away", http.StatusBadGateway)
		return
	}
	l.next.ServeHTTP(w, r)
}

func TestAReportWhoseAnswerWasLostCountsOnce(t *testing.T) {
	tbl := newTable(t)
	m, err := tbl.zaehlwerk.StartMatch(scorer.Config{BestOf: 1}, match.Handover{})
	require.NoError(t, err)

	proxy := httptest.NewServer(&lossy{next: tbl.zaehlwerk})
	t.Cleanup(proxy.Close)
	client := NewClient(proxy.URL, quiet())
	client.retryAfter = time.Millisecond

	st, err := client.Ingest(t.Context(), scorer.ScoreEvent{
		MatchID: m.ID, Source: "pong-test", Half: scorer.HalfLeft, Delta: 1, EventID: 1})
	require.NoError(t, err)
	require.Equal(t, [2]int{1, 0}, st.Points)
	require.Equal(t, [2]int{1, 0}, m.Scorer.State().Points)
}

func TestHealthzSaysTheServiceIsUp(t *testing.T) {
	p := newPong(t, "http://127.0.0.1:1", 1)
	resp, err := http.Get(p.url + "/healthz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Equal(t, "ok", body["status"])
}

func TestAGameWithoutAZaehlwerkIsRefusedAtOnce(t *testing.T) {
	p := newPong(t, "http://127.0.0.1:1", 1)
	status, _, raw := p.start("0s")
	require.Equal(t, http.StatusBadGateway, status)
	require.Contains(t, raw, "reading the table from zaehlwerk")
}
