package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/live"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/panel"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

const demoBody = `{"players":["Pong A","Pong B"],"demo":true}`

func TestPOSTMatchesTakesTheDemoField(t *testing.T) {
	c := newClient(t)

	demo, err := c.registry.Get(c.newMatch(demoBody))
	require.NoError(t, err)
	require.True(t, demo.Demo)

	demo.End(time.Now())
	real, err := c.registry.Get(c.newMatch(`{"players":["Anna","Bernd"]}`))
	require.NoError(t, err)
	require.False(t, real.Demo, "a body without the field is a real match")
}

// ADR-0008 (c), at the door: the demo waits, and the answer says what for.
func TestADemoIsRefusedWithTheRunningMatchWhileARealOneRuns(t *testing.T) {
	c := newClient(t)
	real := c.newMatch(`{"players":["Anna","Bernd"]}`)

	body := c.failure(http.StatusConflict, http.MethodPost, "/matches", demoBody)
	require.Contains(t, body.Error, "a real match is running")
	require.NotNil(t, body.State)
	require.Equal(t, real, body.State.MatchID)

	current := c.state(http.StatusOK, http.MethodGet, "/matches/current", "")
	require.Equal(t, real, current.MatchID)
}

// ADR-0008 (a), at the door: the demo is over at once, and the hub says so,
// so a stream on the demo follows the table to the real match.
func TestARealMatchEndsTheDemoAndTheTableStreamFollows(t *testing.T) {
	f := newTableFixture(t)
	demo, err := f.api.start(scorer.Config{Players: [2]string{"Pong A", "Pong B"}, BestOf: 1}, match.Setup{Demo: true})
	require.NoError(t, err)
	r, _ := f.connect()
	require.Equal(t, demo.ID, readTableEvent(t, r).State.MatchID)

	real := f.start(match.Handover{})
	require.False(t, demo.Running())

	ev := readTableEvent(t, r)
	require.Equal(t, live.KindEnded, ev.Kind, "the demo is announced as ended")
	require.Equal(t, demo.ID, ev.State.MatchID)
	require.True(t, ev.Demo)

	ev = readTableEvent(t, r)
	require.Equal(t, live.KindSnapshot, ev.Kind)
	require.Equal(t, real.ID, ev.State.MatchID)
	require.False(t, ev.Demo)
}

func TestTheStreamSaysAMatchIsADemo(t *testing.T) {
	f := newTableFixture(t)
	demo, err := f.api.start(scorer.Config{Players: [2]string{"Pong A", "Pong B"}, BestOf: 1}, match.Setup{Demo: true})
	require.NoError(t, err)

	r, _ := f.connect()
	ev := readTableEvent(t, r)
	require.Equal(t, demo.ID, ev.State.MatchID)
	require.True(t, ev.Demo)

	point(t, demo, scorer.PlayerA, 1)
	require.True(t, readTableEvent(t, r).Demo, "on every event, not only the snapshot")

	// And on the wire it is absent for a real match, not false.
	raw, err := json.Marshal(f.api.withIDs(nil, live.KindSnapshot, scorer.State{}))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "demo")
}

// catcher records every switch the panel is told to make.
type catcher struct {
	mu      sync.Mutex
	streams []string
	posts   [][]string
}

func newCatcher(t *testing.T) (*catcher, string) {
	t.Helper()
	c := &catcher{streams: []string{"messages"}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /streams", func(w http.ResponseWriter, _ *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"streams": c.streams})
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
		c.streams, c.posts = body.Streams, append(c.posts, body.Streams)
		_ = json.NewEncoder(w).Encode(map[string]any{"streams": body.Streams})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return c, srv.URL
}

func (c *catcher) switches() [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.posts)
}

// The demo's panel is let go of after the real match has taken it, so the
// release finds the panel held by the real match and leaves it: the panel goes
// from the demo's score to the real one's without the idle streams between.
func TestTheRealMatchHasThePanelBeforeTheDemoLetsGo(t *testing.T) {
	cat, url := newCatcher(t)
	sw := panel.NewSwitcher(panel.StreamConfig{BaseURL: url})
	t.Cleanup(func() { _ = sw.Close() })
	c := newClient(t)
	c.server = New(c.registry, WithLogger(c.server.log), WithPanelSwitcher(sw))

	c.newMatch(demoBody)
	require.Eventually(t, func() bool { return len(cat.switches()) == 1 }, 2*time.Second, 5*time.Millisecond)
	c.newMatch(`{"players":["Anna","Bernd"]}`)
	require.Eventually(t, func() bool { return len(cat.switches()) == 2 }, 2*time.Second, 5*time.Millisecond)

	// The release has been submitted after the start; give the worker the
	// time to get it wrong.
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, [][]string{{"tabletennis"}, {"tabletennis"}}, cat.switches(),
		"the panel never went back to the idle streams in between")
}

// GET /matches/current says a demo is one, and says nothing for a real match,
// so a caller that never heard of the field reads what it always read.
func TestTheCurrentMatchSaysWhenItIsADemo(t *testing.T) {
	c := newClient(t)
	c.newMatch(demoBody)
	_, body := c.do(http.MethodGet, "/matches/current", "")
	require.Contains(t, string(body), `"demo":true`)

	c.newMatch(`{"players":["Anna","Bernd"]}`)
	_, body = c.do(http.MethodGet, "/matches/current", "")
	require.NotContains(t, string(body), `"demo"`)
	require.Contains(t, string(body), `"match_id"`, "the state itself is unchanged")
}
