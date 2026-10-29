package pong

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
	return f.startWith(startBody{Pace: pace})
}

func (f *pongFixture) startWith(sb any) (int, View, string) {
	f.t.Helper()
	body, err := json.Marshal(sb)
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

// cuttable forwards to zaehlwerk and can close every /live/stream it carries,
// as a proxy or a network that drops long-lived connections does.
type cuttable struct {
	next http.Handler

	mu      sync.Mutex
	streams []context.CancelFunc
}

func (c *cuttable) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/live/stream" {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		c.mu.Lock()
		c.streams = append(c.streams, cancel)
		c.mu.Unlock()
		r = r.WithContext(ctx)
	}
	c.next.ServeHTTP(w, r)
}

func (c *cuttable) cut() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cancel := range c.streams {
		cancel()
	}
}

func TestAGameThatLosesTheTableEndsItsMatch(t *testing.T) {
	tbl := newTable(t)
	proxy := &cuttable{next: tbl.zaehlwerk}
	srv := httptest.NewServer(proxy)
	t.Cleanup(srv.Close)
	p := newPong(t, srv.URL, 1)

	status, v, _ := p.start("20ms")
	require.Equal(t, http.StatusCreated, status)
	require.Eventually(t, func() bool { return p.get(v.ID).State.Phase == scorer.PhasePlaying },
		5*time.Second, 5*time.Millisecond)

	proxy.cut()

	v = p.finished(v.ID)
	require.Equal(t, StatusEnded, v.Status)
	require.Equal(t, errTableLost.Error(), v.Reason)
	ours, err := tbl.registry.Get(v.MatchID)
	require.NoError(t, err)
	require.False(t, ours.Running(), "the game's match was ended, not played on blind")
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

// faulty starts one game with these faults and waits for it to end.
func (f *pongFixture) faulty(faults Faults) View {
	f.t.Helper()
	status, v, raw := f.startWith(startBody{Pace: "0s", Faults: &faults})
	require.Equal(f.t, http.StatusCreated, status, raw)
	return f.finished(v.ID)
}

func kinds(v View) map[FaultKind]int {
	n := map[FaultKind]int{}
	for _, f := range v.Faults {
		n[f.Kind]++
	}
	return n
}

func TestAGameWithFaultsEndsWithNoUnexplainedDivergence(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)

	v := p.faulty(Faults{WrongHalf: 0.1, Resend: 0.1, Bounce: 0.1, Lost: 0.1, Ghost: 0.1, WrongPhase: 0.5})

	require.Equal(t, StatusWon, v.Status, v.Reason)
	require.Zero(t, v.Unexplained, "%+v", v.Divergences)
	require.NotEmpty(t, v.Divergences, "faults that move the score are seen")
	for _, kind := range []FaultKind{FaultWrongHalf, FaultResend, FaultBounce, FaultLost, FaultGhost, FaultWrongPhase} {
		require.Positive(t, kinds(v)[kind], "no %s was injected", kind)
	}
	require.Equal(t, [2]int{v.Truth[0] + v.Explained[0], v.Truth[1] + v.Explained[1]}, v.Zaehlwerk,
		"zaehlwerk is exactly where the faults put it")
}

func TestTheTruthIsUntouchedByTheFaults(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)

	v := p.faulty(Faults{Lost: 0.2})

	require.Equal(t, v.Rallies, v.Truth[0]+v.Truth[1], "a lost press is still a rally won")
	require.Equal(t, kinds(v)[FaultLost], v.Rallies-(v.Zaehlwerk[0]+v.Zaehlwerk[1]),
		"zaehlwerk is short by exactly the lost presses")
}

func TestAWrongHalfIsTakenBackWhereverItLeftTheMatch(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)

	// Every rally goes to the wrong half first, the serve rally, the points
	// that end a set and the one that ends the match included.
	v := p.faulty(Faults{WrongHalf: 1, Correct: true})

	require.Equal(t, StatusWon, v.Status, v.Reason)
	require.Empty(t, v.Divergences, "every wrong half was undone and the right one counted")
	require.Equal(t, v.Truth, v.Zaehlwerk)

	undoneIn := map[scorer.Phase]bool{}
	servedRally := false
	for _, f := range v.Faults {
		require.Equal(t, FaultWrongHalf, f.Kind)
		undoneIn[f.UndoneIn] = true
		servedRally = servedRally || f.Phase == scorer.PhaseServeRally
	}
	require.True(t, servedRally, "an undo of the serve rally")
	require.True(t, undoneIn[scorer.PhaseChangeEnds], "an undo of a point that won a set")
	require.True(t, undoneIn[scorer.PhaseComplete], "an undo of the point that won the match")
}

func TestAnUncorrectedWrongHalfIsAnExpectedDivergence(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)

	v := p.faulty(Faults{WrongHalf: 0.2})

	require.Equal(t, StatusWon, v.Status, v.Reason)
	require.Zero(t, v.Unexplained, "%+v", v.Divergences)
	require.NotEmpty(t, v.Divergences)
	for _, d := range v.Divergences {
		require.True(t, d.Expected)
	}
}

func TestTheSameSeedInjectsTheSameFaults(t *testing.T) {
	faults := Faults{WrongHalf: 0.1, Resend: 0.1, Lost: 0.1, Ghost: 0.1}
	a := newPong(t, newTable(t).url, 3).faulty(faults)
	b := newPong(t, newTable(t).url, 3).faulty(faults)

	require.Equal(t, a.Faults, b.Faults)
	require.Equal(t, a.State.CompletedSets, b.State.CompletedSets)
}

func TestACounterResetZaehlwerkDiscardsIsAFinding(t *testing.T) {
	// Without the quiet minute zaehlwerk rightly reads the reset as a
	// duplicate. That the game calls it a finding is what shows it would
	// notice a zaehlwerk that discarded a genuine one.
	p := newPong(t, newTable(t).url, 1)
	p.pong.resetQuiet = time.Millisecond

	status, v, raw := p.startWith(startBody{Pace: "0s", Faults: &Faults{CounterReset: 0.05}})
	require.Equal(t, http.StatusCreated, status, raw)

	require.Eventually(t, func() bool {
		for _, d := range p.get(v.ID).Divergences {
			if d.What == string(FaultCounterReset) && !d.Expected {
				return true
			}
		}
		return false
	}, 10*time.Second, 5*time.Millisecond)
}

func TestPOSTGamesReplacesTheConfiguredFaults(t *testing.T) {
	tbl := newTable(t)
	p := New(Config{ZaehlwerkURL: tbl.url, BestOf: 3, Skills: [2]Skill{skills["mittel"], skills["mittel"]},
		Seed: 1, Faults: Faults{Lost: 0.5}}, quiet())
	srv := httptest.NewServer(p)
	t.Cleanup(func() {
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, p.Close(ctx))
	})
	f := &pongFixture{t: t, pong: p, url: srv.URL}

	configured := f.finished(func() string { _, v, _ := f.start("0s"); return v.ID }())
	require.Equal(t, Faults{Lost: 0.5}, configured.FaultShares)

	replaced := f.faulty(Faults{Bounce: 0.1})
	require.Equal(t, Faults{Bounce: 0.1}, replaced.FaultShares, "replaced as a whole, not merged")
}

func TestPOSTGamesRefusesFaultsItCannotRun(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)

	for _, body := range []string{
		`{"faults": {"wrong-half": 0.1}}`,
		`{"faults": {"lost": 1.5}}`,
		`{"faults": {"ghost": -0.2}}`,
	} {
		resp, err := http.Post(p.url+"/games", "application/json", strings.NewReader(body))
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
	}
}

func TestAHeadlessGameEndsWhenZaehlwerkEndsItsMatch(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)

	// A pace this long means no report is refused in time: only the table
	// can tell the game its match is over.
	status, v, raw := p.start("1m")
	require.Equal(t, http.StatusCreated, status, raw)
	m, err := tbl.registry.Get(v.MatchID)
	require.NoError(t, err)
	tbl.zaehlwerk.EndMatch(m)

	v = p.finished(v.ID)
	require.Equal(t, StatusEnded, v.Status)
	require.Equal(t, errMatchEnded.Error(), v.Reason)
}

// slowWin holds back zaehlwerk's answer to the point that wins the match, so
// the table's no_match after the win reaches the game before play has its
// answer.
type slowWin struct {
	next http.Handler
	held atomic.Bool
}

func (s *slowWin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ingest/web" {
		s.next.ServeHTTP(w, r)
		return
	}
	rec := httptest.NewRecorder()
	s.next.ServeHTTP(rec, r)
	if bytes.Contains(rec.Body.Bytes(), []byte(`"complete":true`)) {
		s.held.Store(true)
		time.Sleep(200 * time.Millisecond)
	}
	for k, vs := range rec.Header() {
		w.Header()[k] = vs
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

func TestAWonGameStaysWon(t *testing.T) {
	tbl := newTable(t)
	proxy := &slowWin{next: tbl.zaehlwerk}
	srv := httptest.NewServer(proxy)
	t.Cleanup(srv.Close)
	p := newPong(t, srv.URL, 1)

	status, v, raw := p.start("0s")
	require.Equal(t, http.StatusCreated, status, raw)

	v = p.finished(v.ID)
	require.True(t, proxy.held.Load(), "the winning answer was held back")
	require.Equal(t, StatusWon, v.Status, v.Reason)
	require.True(t, v.State.Complete)
}

// A late press reaches zaehlwerk after the press that overtook it, with its
// older event id, and zaehlwerk must discard it: the match is one point short
// for each, as the faults explain.
func TestALatePressArrivesAfterTheNextAndIsDiscarded(t *testing.T) {
	tbl := newTable(t)
	rec := &recorder{next: tbl.zaehlwerk}
	proxy := httptest.NewServer(rec)
	t.Cleanup(proxy.Close)
	p := newPong(t, proxy.URL, 1)

	v := p.faulty(Faults{Latency: 0.2})

	require.Equal(t, StatusWon, v.Status, v.Reason)
	require.Zero(t, v.Unexplained, "%+v", v.Divergences)
	late := kinds(v)[FaultLatency]
	require.Positive(t, late)
	require.Equal(t, late, v.Rallies-(v.Zaehlwerk[0]+v.Zaehlwerk[1]), "zaehlwerk is short by exactly the late presses")

	rec.mu.Lock()
	defer rec.mu.Unlock()
	overtaken := 0
	for i := 1; i < len(rec.sent); i++ {
		id, before := rec.sent[i]["event_id"].(float64), rec.sent[i-1]["event_id"].(float64)
		if id < before {
			require.Equal(t, before-1, id, "the press just before the one that overtook it")
			overtaken++
		}
	}
	// One late press can be in flight when the match is won, and is then
	// never sent.
	require.InDelta(t, late, overtaken, 1)
	for _, f := range v.Faults {
		require.NotZero(t, f.EventID, "a late press has its number from when it was pressed")
	}
}
