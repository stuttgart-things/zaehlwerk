package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// catcher stubs homerun2-led-catcher's /streams endpoints, closely enough that
// the tests exercise the same request shapes the real one answers.
type catcher struct {
	mu       sync.Mutex
	streams  []string
	posts    [][]string
	gets     int
	postFail int // status to answer POST with, when non-zero
	getFail  int
	server   *httptest.Server
}

func newCatcher(t *testing.T, initial ...string) *catcher {
	t.Helper()

	c := &catcher{streams: initial}
	if len(initial) == 0 {
		c.streams = []string{"messages"}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /streams", func(w http.ResponseWriter, _ *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.gets++
		if c.getFail != 0 {
			w.WriteHeader(c.getFail)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Shaped like the real one, extra fields included, so a decoder that
		// only tolerates exactly our fields would fail here.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"streams":       c.streams,
			"consumerGroup": "homerun2-led-catcher",
			"consumerName":  "stub",
		})
	})
	mux.HandleFunc("POST /streams", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Streams []string `json:"streams"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		c.mu.Lock()
		defer c.mu.Unlock()
		c.posts = append(c.posts, body.Streams)
		if c.postFail != 0 {
			w.WriteHeader(c.postFail)
			return
		}
		c.streams = body.Streams
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"streams": body.Streams})
	})

	c.server = httptest.NewServer(mux)
	t.Cleanup(c.server.Close)
	return c
}

func (c *catcher) current() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.streams...)
}

func (c *catcher) switches() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.posts)
}

func (c *catcher) setStreams(streams ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.streams = streams
}

func (c *catcher) failPosts(status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.postFail = status
}

func (c *catcher) failGets(status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.getFail = status
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newSwitcher(t *testing.T, c *catcher, mutate ...func(*StreamConfig)) (*Switcher, *clock) {
	t.Helper()

	clk := newClock()
	cfg := StreamConfig{
		BaseURL: c.server.URL,
		Logger:  quiet(),
		Now:     clk.now,
	}
	for _, m := range mutate {
		m(&cfg)
	}

	s := NewSwitcher(cfg)
	require.NotNil(t, s)
	t.Cleanup(func() { _ = s.Close() })
	return s, clk
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// requireHolds waits until the switch has landed *and* been recorded as ours.
//
// Waiting only for the catcher's state is the wrong signal: `held` is set after
// the switch returns, deliberately, so that a switch which failed is never
// recorded as ours to revert. Three tests raced on that before this existed.
func requireHolds(t *testing.T, s *Switcher, c *catcher, matchID string, streams ...string) {
	t.Helper()
	eventually(t, "the panel to be held by "+matchID, func() bool {
		id, held := s.Held()
		return held && id == matchID && sameSet(c.current(), streams)
	})
}

// requireReleased is requireHolds's other half: the revert has landed and the
// override has been let go. Same race, other direction.
func requireReleased(t *testing.T, s *Switcher, c *catcher, streams ...string) {
	t.Helper()
	eventually(t, "the panel to be given back", func() bool {
		_, held := s.Held()
		return !held && sameSet(c.current(), streams)
	})
}

func requireStable(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		require.True(t, cond(), what)
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAMatchTakesThePanelAndGivesItBack(t *testing.T) {
	c := newCatcher(t)
	s, _ := newSwitcher(t, c)

	s.Start("m1")
	requireHolds(t, s, c, "m1", "tabletennis")

	s.Release("m1")
	requireReleased(t, s, c, "messages")
}

func TestAWonMatchGivesThePanelBackWithoutAnExplicitEnd(t *testing.T) {
	c := newCatcher(t)
	s, _ := newSwitcher(t, c)

	s.Start("m1")
	requireHolds(t, s, c, "m1", "tabletennis")

	s.Observe(scorer.Transition{
		Kind:  scorer.TransitionMatchWon,
		State: scorer.State{MatchID: "m1", Complete: true},
	})

	requireReleased(t, s, c, "messages")
}

func TestAnOrdinaryPointDoesNotGiveThePanelBack(t *testing.T) {
	c := newCatcher(t)
	s, _ := newSwitcher(t, c)

	s.Start("m1")
	requireHolds(t, s, c, "m1", "tabletennis")

	for _, kind := range []scorer.TransitionKind{
		scorer.TransitionPoint, scorer.TransitionSetWon, scorer.TransitionUndo,
	} {
		s.Observe(scorer.Transition{Kind: kind, State: scorer.State{MatchID: "m1"}})
	}

	requireStable(t, "the panel stays with the match", func() bool {
		return sameSet(c.current(), []string{"tabletennis"})
	})
}

// ADR-0003's whole point: a set we did not establish belongs to whoever did.
func TestAPanelSwitchedBySomeoneElseIsLeftAlone(t *testing.T) {
	c := newCatcher(t)
	s, _ := newSwitcher(t, c)

	s.Start("m1")
	requireHolds(t, s, c, "m1", "tabletennis")

	// Someone opens the catcher's UI and picks something else.
	c.setStreams("messages", "scale")
	switchesBefore := c.switches()

	s.Release("m1")
	eventually(t, "the override to be given up", func() bool {
		_, held := s.Held()
		return !held
	})

	require.Equal(t, []string{"messages", "scale"}, c.current(),
		"a deliberate manual override was undone")
	require.Equal(t, switchesBefore, c.switches(), "the switcher posted anyway")
}

func TestAnEndForAMatchThatNoLongerOwnsThePanelIsIgnored(t *testing.T) {
	c := newCatcher(t)
	s, _ := newSwitcher(t, c)

	s.Start("m1")
	requireHolds(t, s, c, "m1", "tabletennis")
	s.Start("m2")
	eventually(t, "the second match to own it", func() bool {
		id, held := s.Held()
		return held && id == "m2"
	})

	// A late end for the match that has been superseded.
	s.Release("m1")

	requireStable(t, "the running match keeps the panel", func() bool {
		id, held := s.Held()
		return held && id == "m2" && sameSet(c.current(), []string{"tabletennis"})
	})
}

func TestTheIdleTimeoutGivesThePanelBack(t *testing.T) {
	c := newCatcher(t)
	s, clk := newSwitcher(t, c, func(cfg *StreamConfig) {
		cfg.IdleTimeout = 20 * time.Minute
	})

	s.Start("m1")
	requireHolds(t, s, c, "m1", "tabletennis")

	clk.advance(21 * time.Minute)
	s.checkIdle() // what the ticker calls

	requireReleased(t, s, c, "messages")
}

func TestAPointResetsTheIdleClock(t *testing.T) {
	c := newCatcher(t)
	s, clk := newSwitcher(t, c, func(cfg *StreamConfig) {
		cfg.IdleTimeout = 20 * time.Minute
	})

	s.Start("m1")
	requireHolds(t, s, c, "m1", "tabletennis")

	// Nineteen minutes, a point, nineteen more: never twenty in a row.
	clk.advance(19 * time.Minute)
	s.Observe(scorer.Transition{Kind: scorer.TransitionPoint, State: scorer.State{MatchID: "m1"}})
	clk.advance(19 * time.Minute)
	s.checkIdle()

	requireStable(t, "the match keeps the panel", func() bool {
		return sameSet(c.current(), []string{"tabletennis"})
	})
}

// A point in some other match must not keep this one's override alive.
func TestAPointInAnotherMatchDoesNotResetTheIdleClock(t *testing.T) {
	c := newCatcher(t)
	s, clk := newSwitcher(t, c, func(cfg *StreamConfig) {
		cfg.IdleTimeout = 20 * time.Minute
	})

	s.Start("m1")
	requireHolds(t, s, c, "m1", "tabletennis")

	clk.advance(19 * time.Minute)
	s.Observe(scorer.Transition{Kind: scorer.TransitionPoint, State: scorer.State{MatchID: "somewhere-else"}})
	clk.advance(2 * time.Minute)
	s.checkIdle()

	requireReleased(t, s, c, "messages")
}

func TestTheIdleTimeoutDoesNothingWhenThePanelIsNotHeld(t *testing.T) {
	c := newCatcher(t)
	s, clk := newSwitcher(t, c)

	clk.advance(time.Hour)
	s.checkIdle()

	require.Zero(t, c.switches())
}

// The failure behaviour the issue and ADR-0003 both call for: the panel is the
// least important consumer, and a switch that fails costs it nothing else.
func TestAFailedSwitchIsNotRecordedAsHeld(t *testing.T) {
	c := newCatcher(t)
	c.failPosts(http.StatusServiceUnavailable)
	s, _ := newSwitcher(t, c)

	s.Start("m1")
	eventually(t, "the attempt", func() bool { return c.switches() > 0 })

	_, held := s.Held()
	require.False(t, held, "a switch that failed must not be recorded as ours to revert")

	// And so the end does not try to revert something we never set.
	before := c.switches()
	s.Release("m1")
	requireStable(t, "no revert of a switch we never made", func() bool { return c.switches() == before })
}

func TestAnUnreachableCatcherIsSurvivable(t *testing.T) {
	// Port 1 is reserved and nothing listens there.
	s := NewSwitcher(StreamConfig{
		BaseURL:        "http://127.0.0.1:1",
		RequestTimeout: 100 * time.Millisecond,
		Logger:         quiet(),
	})
	require.NotNil(t, s)
	defer func() { _ = s.Close() }()

	start := time.Now()
	s.Start("m1")
	s.Observe(scorer.Transition{Kind: scorer.TransitionPoint, State: scorer.State{MatchID: "m1"}})
	s.Release("m1")

	require.Less(t, time.Since(start), time.Second, "an unreachable catcher blocked the caller")
	_, held := s.Held()
	require.False(t, held)
}

// If the read-back fails we cannot know whose set is on the panel, so the
// override is given up rather than held and retried forever.
func TestAFailedReadBackGivesUpTheOverride(t *testing.T) {
	c := newCatcher(t)
	s, _ := newSwitcher(t, c)

	s.Start("m1")
	requireHolds(t, s, c, "m1", "tabletennis")

	c.failGets(http.StatusInternalServerError)
	s.Release("m1")

	eventually(t, "the override to be given up", func() bool {
		_, held := s.Held()
		return !held
	})
}

func TestAFailedRevertStillGivesUpTheOverride(t *testing.T) {
	c := newCatcher(t)
	s, _ := newSwitcher(t, c)

	s.Start("m1")
	requireHolds(t, s, c, "m1", "tabletennis")

	c.failPosts(http.StatusServiceUnavailable)
	s.Release("m1")

	eventually(t, "the override to be given up", func() bool {
		_, held := s.Held()
		return !held
	})
}

func TestNoCatcherConfiguredMeansNoSwitcher(t *testing.T) {
	require.Nil(t, NewSwitcher(StreamConfig{}))
}

// A nil switcher is the local-development case, and every method has to be
// callable on it so callers do not have to check.
func TestEveryMethodIsSafeOnANilSwitcher(t *testing.T) {
	var s *Switcher

	require.NotPanics(t, func() {
		s.Start("m1")
		s.Observe(scorer.Transition{Kind: scorer.TransitionMatchWon, State: scorer.State{MatchID: "m1"}})
		s.Release("m1")
		require.NoError(t, s.Close())
	})

	id, held := s.Held()
	require.Empty(t, id)
	require.False(t, held)
}

func TestShutdownGivesThePanelBack(t *testing.T) {
	c := newCatcher(t)
	clk := newClock()
	s := NewSwitcher(StreamConfig{BaseURL: c.server.URL, Logger: quiet(), Now: clk.now})
	require.NotNil(t, s)

	s.Start("m1")
	requireHolds(t, s, c, "m1", "tabletennis")

	require.NoError(t, s.Close())
	require.True(t, sameSet(c.current(), []string{"messages"}),
		"shutting down left the panel on a score that has stopped moving")
}

func TestShutdownLeavesAForeignOverrideAlone(t *testing.T) {
	c := newCatcher(t)
	clk := newClock()
	s := NewSwitcher(StreamConfig{BaseURL: c.server.URL, Logger: quiet(), Now: clk.now})
	require.NotNil(t, s)

	s.Start("m1")
	requireHolds(t, s, c, "m1", "tabletennis")
	c.setStreams("scale")

	require.NoError(t, s.Close())
	require.Equal(t, []string{"scale"}, c.current())
}

func TestCloseIsSafeMoreThanOnce(t *testing.T) {
	c := newCatcher(t)
	s, _ := newSwitcher(t, c)
	s.Start("m1")

	require.NoError(t, s.Close())
	require.NoError(t, s.Close())
}

func TestTheStreamSetsAreConfigurable(t *testing.T) {
	c := newCatcher(t, "notifications")
	s, _ := newSwitcher(t, c, func(cfg *StreamConfig) {
		cfg.MatchStreams = []string{"tt", "tt-extra"}
		cfg.IdleStreams = []string{"notifications"}
	})

	s.Start("m1")
	requireHolds(t, s, c, "m1", "tt", "tt-extra")

	s.Release("m1")
	requireReleased(t, s, c, "notifications")
}

// The catcher reports its set in whatever order it holds it, and a reordering
// is not somebody else's override.
func TestOwnershipDoesNotDependOnStreamOrder(t *testing.T) {
	c := newCatcher(t)
	s, _ := newSwitcher(t, c, func(cfg *StreamConfig) {
		cfg.MatchStreams = []string{"a", "b"}
	})

	s.Start("m1")
	requireHolds(t, s, c, "m1", "a", "b")

	c.setStreams("b", "a") // same set, other order
	s.Release("m1")

	requireReleased(t, s, c, "messages")
}

func TestConfigDefaults(t *testing.T) {
	cfg := StreamConfig{BaseURL: "http://x"}.withDefaults()

	require.Equal(t, DefaultMatchStreams, cfg.MatchStreams)
	require.Equal(t, DefaultIdleStreams, cfg.IdleStreams)
	require.Equal(t, DefaultIdleTimeout, cfg.IdleTimeout)
	require.Equal(t, DefaultRequestTimeout, cfg.RequestTimeout)
	require.NotNil(t, cfg.Logger)
	require.NotNil(t, cfg.Now)
}

// The defaults are package-level slices; a config that took them must not be
// able to write through into them.
func TestDefaultsAreNotAliasedIntoTheConfig(t *testing.T) {
	cfg := StreamConfig{BaseURL: "http://x"}.withDefaults()
	cfg.MatchStreams[0] = "clobbered"

	require.Equal(t, []string{"tabletennis"}, DefaultMatchStreams)
}

func TestStartAndReleaseUnderRace(t *testing.T) {
	c := newCatcher(t)
	s, _ := newSwitcher(t, c)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := string(rune('a' + i))
			for range 20 {
				s.Start(id)
				s.Observe(scorer.Transition{Kind: scorer.TransitionPoint, State: scorer.State{MatchID: id}})
				s.Release(id)
			}
		}()
	}
	wg.Wait()
}

// Close used to read `held` on the calling goroutine while the worker was
// still inside acquire, so a shutdown during a match start left the panel on
// the match's stream with nobody recorded as owning it. Found because three
// tests raced on the same gap.
func TestShutdownDuringAMatchStartStillGivesThePanelBack(t *testing.T) {
	slow := make(chan struct{})
	c := newCatcher(t)

	// A catcher that answers the first POST slowly, which is the window.
	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			<-slow
		}
		c.server.Config.Handler.ServeHTTP(w, r)
	}))
	defer blocking.Close()

	s := NewSwitcher(StreamConfig{BaseURL: blocking.URL, Logger: quiet()})
	require.NotNil(t, s)

	s.Start("m1")
	// Let the worker get into the POST, then release it and close immediately.
	time.Sleep(20 * time.Millisecond)

	closed := make(chan error, 1)
	go func() {
		close(slow)
		closed <- s.Close()
	}()

	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung")
	}

	require.True(t, sameSet(c.current(), []string{"messages"}),
		"shutting down mid-start left the panel on the match's stream")
}
