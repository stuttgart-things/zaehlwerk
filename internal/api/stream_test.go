package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/live"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// These run against a real http.Server rather than httptest.NewRecorder,
// because everything under test here — flushing, heartbeats, a client hanging
// up — only exists on a real connection.

type streamFixture struct {
	t        *testing.T
	hub      *live.Hub
	registry *match.Registry
	server   *httptest.Server
	match    *match.Match
}

func newStreamFixture(t *testing.T, hubOpts []live.Option, opts ...Option) *streamFixture {
	t.Helper()

	hub := live.New(hubOpts...)
	registry := match.NewRegistry(match.WithObserver(hub.Observe))
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	api := New(registry, append([]Option{WithLogger(quiet), WithHub(hub)}, opts...)...)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	m, err := registry.Create(scorer.Config{Players: [2]string{"Anna", "Bernd"}})
	require.NoError(t, err)

	return &streamFixture{t: t, hub: hub, registry: registry, server: srv, match: m}
}

// connect opens a stream and returns a reader over it plus a cancel that hangs
// up the way a browser tab closing does.
func (f *streamFixture) connect(headers map[string]string) (*bufio.Reader, *http.Response, context.CancelFunc) {
	f.t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.server.URL+"/matches/"+f.match.ID+"/stream", nil)
	require.NoError(f.t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := f.server.Client().Do(req)
	if err != nil {
		cancel()
		f.t.Fatalf("connecting: %v", err)
	}
	f.t.Cleanup(func() { cancel(); _ = resp.Body.Close() })

	return bufio.NewReader(resp.Body), resp, cancel
}

func (f *streamFixture) point(p scorer.Player, id uint64) {
	f.t.Helper()
	_, err := f.match.Scorer.Apply(scorer.ScoreEvent{
		MatchID: f.match.ID, Player: p, Delta: 1, EventID: id, Source: "test",
	})
	require.NoError(f.t, err)
}

// readEvent reads until one `data:` line arrives, skipping heartbeat comments.
func readEvent(t *testing.T, r *bufio.Reader) live.Event {
	t.Helper()

	type result struct {
		ev  live.Event
		err error
	}
	ch := make(chan result, 1)

	go func() {
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				ch <- result{err: err}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if !strings.HasPrefix(line, "data: ") {
				continue // heartbeat comment or the blank separator line
			}
			var ev live.Event
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				ch <- result{err: err}
				return
			}
			ch <- result{ev: ev}
			return
		}
	}()

	select {
	case res := <-ch:
		require.NoError(t, res.err)
		return res.ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an event on the stream")
		return live.Event{}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
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

func TestTheStreamAnnouncesItselfAsEventStream(t *testing.T) {
	f := newStreamFixture(t, nil)
	_, resp, _ := f.connect(nil)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	require.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
	require.Equal(t, "no", resp.Header.Get("X-Accel-Buffering"))
}

// A client joining mid-match must render immediately, without a second request.
func TestTheCurrentStateArrivesOnConnect(t *testing.T) {
	f := newStreamFixture(t, nil)
	f.point(scorer.PlayerA, 1)
	f.point(scorer.PlayerB, 2)

	r, _, _ := f.connect(nil)
	ev := readEvent(t, r)

	require.Equal(t, live.KindSnapshot, ev.Kind)
	require.Equal(t, [2]int{1, 1}, ev.State.Points)
	require.Equal(t, f.match.ID, ev.State.MatchID)
	require.Equal(t, [2]string{"Anna", "Bernd"}, ev.State.Players)
}

func TestOneEventPerTransition(t *testing.T) {
	f := newStreamFixture(t, nil)
	r, _, _ := f.connect(nil)
	require.Equal(t, live.KindSnapshot, readEvent(t, r).Kind)

	waitFor(t, "the subscription", func() bool { return f.hub.Subscribers(f.match.ID) == 1 })

	f.point(scorer.PlayerA, 1)
	f.point(scorer.PlayerA, 2)
	_, err := f.match.Scorer.Undo()
	require.NoError(t, err)

	first, second, third := readEvent(t, r), readEvent(t, r), readEvent(t, r)

	require.Equal(t, "point", first.Kind)
	require.Equal(t, [2]int{1, 0}, first.State.Points)
	require.Equal(t, "point", second.Kind)
	require.Equal(t, [2]int{2, 0}, second.State.Points)
	require.Equal(t, "undo", third.Kind)
	require.Equal(t, [2]int{1, 0}, third.State.Points)
}

func TestASetAndMatchWinArriveWithTheirKind(t *testing.T) {
	f := newStreamFixture(t, nil)
	r, _, _ := f.connect(nil)
	require.Equal(t, live.KindSnapshot, readEvent(t, r).Kind)
	waitFor(t, "the subscription", func() bool { return f.hub.Subscribers(f.match.ID) == 1 })

	var kinds []string
	for i := uint64(1); !f.match.Scorer.State().Complete; i++ {
		f.point(scorer.PlayerA, i)
		require.Less(t, i, uint64(100), "match did not finish")
	}
	for range 33 {
		kinds = append(kinds, readEvent(t, r).Kind)
		if kinds[len(kinds)-1] == "match_won" {
			break
		}
	}

	require.Contains(t, kinds, "set_won")
	require.Equal(t, "match_won", kinds[len(kinds)-1])
}

func TestSeveralClientsWatchTheSameMatch(t *testing.T) {
	f := newStreamFixture(t, nil)

	readers := make([]*bufio.Reader, 4)
	for i := range readers {
		readers[i], _, _ = f.connect(nil)
		require.Equal(t, live.KindSnapshot, readEvent(t, readers[i]).Kind)
	}
	waitFor(t, "four subscriptions", func() bool { return f.hub.Subscribers(f.match.ID) == 4 })

	f.point(scorer.PlayerB, 1)

	for i, r := range readers {
		ev := readEvent(t, r)
		require.Equal(t, "point", ev.Kind, "client %d", i)
		require.Equal(t, [2]int{0, 1}, ev.State.Points, "client %d", i)
	}
}

// The leak the issue asks to be tested for: a phone that sleeps and wakes all
// afternoon opens and drops a lot of connections.
func TestManyConnectionsOpenedAndDroppedReleaseTheirSubscriptions(t *testing.T) {
	f := newStreamFixture(t, nil)

	for range 60 {
		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.server.URL+"/matches/"+f.match.ID+"/stream", nil)
		require.NoError(t, err)

		resp, err := f.server.Client().Do(req)
		require.NoError(t, err)
		require.Equal(t, live.KindSnapshot, readEvent(t, bufio.NewReader(resp.Body)).Kind)

		cancel()
		_ = resp.Body.Close()
	}

	waitFor(t, "every subscription to be released", func() bool { return f.hub.Subscribers(f.match.ID) == 0 })
	waitFor(t, "the hub to forget the match", func() bool { return f.hub.Matches() == 0 })
}

func TestAClientHangingUpReleasesItsSubscription(t *testing.T) {
	f := newStreamFixture(t, nil)
	r, _, cancel := f.connect(nil)
	require.Equal(t, live.KindSnapshot, readEvent(t, r).Kind)
	waitFor(t, "the subscription", func() bool { return f.hub.Subscribers(f.match.ID) == 1 })

	cancel()

	waitFor(t, "the subscription to be released", func() bool { return f.hub.Subscribers(f.match.ID) == 0 })
}

// A stalled client must not hold up the scorer or anybody else watching.
func TestAClientThatStopsReadingBlocksNothing(t *testing.T) {
	f := newStreamFixture(t, []live.Option{live.WithBuffer(2)})

	// Connected and then never read from again.
	_, stalledResp, _ := f.connect(nil)
	defer func() { _ = stalledResp.Body.Close() }()

	healthy, _, _ := f.connect(nil)
	require.Equal(t, live.KindSnapshot, readEvent(t, healthy).Kind)
	waitFor(t, "both subscriptions", func() bool { return f.hub.Subscribers(f.match.ID) == 2 })

	start := time.Now()
	for i := uint64(1); i <= 20; i++ {
		f.point(scorer.PlayerA, i)
	}
	require.Less(t, time.Since(start), 2*time.Second, "the scorer was held up by a stalled client")

	// And the healthy client is still current.
	waitFor(t, "the healthy client to catch up", func() bool {
		return readEvent(t, healthy).State.Points[0] >= 1
	})
}

func TestAnIdleStreamGetsAHeartbeat(t *testing.T) {
	f := newStreamFixture(t, nil, WithHeartbeat(50*time.Millisecond))
	r, _, _ := f.connect(nil)
	require.Equal(t, live.KindSnapshot, readEvent(t, r).Kind)

	// Read raw, because readEvent deliberately skips comments.
	lines := make(chan string, 1)
	go func() {
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, ":") {
				lines <- strings.TrimRight(line, "\r\n")
				return
			}
		}
	}()

	select {
	case line := <-lines:
		require.Equal(t, ": ping", line)
	case <-time.After(3 * time.Second):
		t.Fatal("no heartbeat on an idle stream")
	}
}

func TestStreamingAnUnknownMatchIs404(t *testing.T) {
	f := newStreamFixture(t, nil)

	resp, err := f.server.Client().Get(f.server.URL + "/matches/nope/stream")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// The match exists, the feature does not — which is a different answer to a
// client than "no such match".
func TestStreamingWithoutAHubIs503(t *testing.T) {
	registry := match.NewRegistry()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(New(registry, WithLogger(quiet)))
	defer srv.Close()

	m, err := registry.Create(scorer.Config{Players: [2]string{"Anna", "Bernd"}})
	require.NoError(t, err)

	resp, err := srv.Client().Get(srv.URL + "/matches/" + m.ID + "/stream")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// --- CORS ----------------------------------------------------------------

func TestAnAllowedOriginMayRead(t *testing.T) {
	f := newStreamFixture(t, nil, WithAllowedOrigins([]string{"https://schmetterpause.example"}))
	_, resp, _ := f.connect(map[string]string{"Origin": "https://schmetterpause.example"})

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "https://schmetterpause.example", resp.Header.Get("Access-Control-Allow-Origin"))
	require.Contains(t, resp.Header.Values("Vary"), "Origin")
}

func TestAnUnknownOriginIsRefused(t *testing.T) {
	f := newStreamFixture(t, nil, WithAllowedOrigins([]string{"https://schmetterpause.example"}))

	req, err := http.NewRequest(http.MethodGet, f.server.URL+"/matches/"+f.match.ID+"/stream", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://somewhere.else")

	resp, err := f.server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))
	require.Zero(t, f.hub.Subscribers(f.match.ID), "a refused request still subscribed")
}

// The allow-list is the point: with none configured, no browser origin gets in.
func TestWithNoOriginsConfiguredEveryBrowserOriginIsRefused(t *testing.T) {
	f := newStreamFixture(t, nil)

	req, err := http.NewRequest(http.MethodGet, f.server.URL+"/matches/"+f.match.ID+"/stream", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://schmetterpause.example")

	resp, err := f.server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestARequestWithNoOriginIsNotACrossOriginRequest(t *testing.T) {
	f := newStreamFixture(t, nil)
	_, resp, _ := f.connect(nil)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))
}

// Never "*", whatever is configured — this is write-adjacent on an internal
// network.
func TestTheWildcardIsNeverSent(t *testing.T) {
	f := newStreamFixture(t, nil, WithAllowedOrigins([]string{"*"}))

	req, err := http.NewRequest(http.MethodGet, f.server.URL+"/matches/"+f.match.ID+"/stream", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://anything.example")

	resp, err := f.server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusForbidden, resp.StatusCode,
		`"*" in the list must not act as a wildcard`)
}

func TestThePreflightAnswersForAnAllowedOrigin(t *testing.T) {
	f := newStreamFixture(t, nil, WithAllowedOrigins([]string{"https://schmetterpause.example"}))

	req, err := http.NewRequest(http.MethodOptions, f.server.URL+"/matches/"+f.match.ID+"/stream", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://schmetterpause.example")

	resp, err := f.server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.Equal(t, "https://schmetterpause.example", resp.Header.Get("Access-Control-Allow-Origin"))
	require.Contains(t, resp.Header.Get("Access-Control-Allow-Methods"), "GET")
}

func TestThePreflightRefusesAnUnknownOrigin(t *testing.T) {
	f := newStreamFixture(t, nil, WithAllowedOrigins([]string{"https://schmetterpause.example"}))

	req, err := http.NewRequest(http.MethodOptions, f.server.URL+"/matches/"+f.match.ID+"/stream", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://somewhere.else")

	resp, err := f.server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// --- ingest and the stream together --------------------------------------

// The path a phone actually takes: it posts a point and watches the stream.
func TestAPointPostedOverIngestReachesTheStream(t *testing.T) {
	f := newStreamFixture(t, nil)
	r, _, _ := f.connect(nil)
	require.Equal(t, live.KindSnapshot, readEvent(t, r).Kind)
	waitFor(t, "the subscription", func() bool { return f.hub.Subscribers(f.match.ID) == 1 })

	body := `{"match_id":"` + f.match.ID + `","source":"browser-1","player":"a","delta":1,"event_id":1}`
	resp, err := f.server.Client().Post(f.server.URL+"/ingest/web", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	ev := readEvent(t, r)
	require.Equal(t, "point", ev.Kind)
	require.Equal(t, [2]int{1, 0}, ev.State.Points)
}

func TestManyClientsAndManyPointsUnderRace(t *testing.T) {
	f := newStreamFixture(t, []live.Option{live.WithBuffer(4)})

	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _, cancel := f.connect(nil)
			readEvent(t, r)
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := uint64(1); i <= 40; i++ {
			_, _ = f.match.Scorer.Apply(scorer.ScoreEvent{
				MatchID: f.match.ID, Player: scorer.PlayerA, Delta: 1, EventID: i, Source: "race",
			})
		}
	}()

	wg.Wait()
	waitFor(t, "every subscription to be released", func() bool { return f.hub.Subscribers(f.match.ID) == 0 })
}
