package ui

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/live"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// These run against a real http.Server rather than a recorder: flushing, a
// heartbeat and a browser hanging up only exist on a real connection.

type streamFixture struct {
	t        *testing.T
	hub      *live.Hub
	registry *match.Registry
	server   *httptest.Server
	match    *match.Match
	event    uint64
}

func newStreamFixture(t *testing.T, opts ...Option) *streamFixture {
	t.Helper()

	hub := live.New()
	registry := match.NewRegistry(match.WithObserver(hub.Observe))
	life := &lifecycle{registry: registry}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := func() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) }

	base := []Option{WithLogger(quiet), WithClock(now), WithHub(hub)}
	srv := httptest.NewServer(New(registry, life, append(base, opts...)...))
	t.Cleanup(srv.Close)

	m, err := registry.Create(scorer.Config{Players: [2]string{"Anna", "Bernd"}})
	require.NoError(t, err)

	return &streamFixture{t: t, hub: hub, registry: registry, server: srv, match: m}
}

func (f *streamFixture) connect() (*bufio.Reader, context.CancelFunc) {
	f.t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		f.server.URL+"/ui/matches/"+f.match.ID+"/stream", nil)
	require.NoError(f.t, err)

	resp, err := f.server.Client().Do(req)
	if err != nil {
		cancel()
		f.t.Fatalf("connecting: %v", err)
	}
	require.Equal(f.t, http.StatusOK, resp.StatusCode)
	require.Equal(f.t, "text/event-stream", resp.Header.Get("Content-Type"))
	f.t.Cleanup(func() { cancel(); _ = resp.Body.Close() })

	return bufio.NewReader(resp.Body), cancel
}

func (f *streamFixture) point(p scorer.Player) {
	f.t.Helper()

	f.event++
	_, err := f.match.Scorer.Apply(scorer.ScoreEvent{
		MatchID: f.match.ID, Player: p, Delta: 1, EventID: f.event, Source: "button-a",
	})
	require.NoError(f.t, err)
}

type sseEvent struct {
	name string
	data string
}

// readEvent reads until one complete event arrives, skipping heartbeats.
func readEvent(t *testing.T, r *bufio.Reader) sseEvent {
	t.Helper()

	type result struct {
		ev  sseEvent
		err error
	}
	ch := make(chan result, 1)

	go func() {
		var ev sseEvent
		var data []string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				ch <- result{err: err}
				return
			}
			line = strings.TrimRight(line, "\n")

			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			case line == "" && ev.name != "":
				ev.data = strings.Join(data, "\n")
				ch <- result{ev: ev}
				return
			}
		}
	}()

	select {
	case res := <-ch:
		require.NoError(t, res.err)
		return res.ev
	case <-time.After(2 * time.Second):
		t.Fatal("no event arrived")
		return sseEvent{}
	}
}

func TestTheBoardArrivesOnConnect(t *testing.T) {
	f := newStreamFixture(t)
	f.point(scorer.PlayerA)

	r, _ := f.connect()

	ev := readEvent(t, r)
	require.Equal(t, "board", ev.name, "a tab opened mid-match renders without a second request")
	require.Contains(t, ev.data, "Anna")
	require.Contains(t, ev.data, `<div class="points">1</div>`)
}

// A point from a button, from `task demo` or from somebody else's phone reaches
// every page watching the match — that is the whole reason the page has a
// stream rather than only its own clicks.
func TestAPointReachesTheBoardAndTheTimeline(t *testing.T) {
	f := newStreamFixture(t)
	r, _ := f.connect()
	readEvent(t, r) // the board on connect

	f.point(scorer.PlayerB)

	board := readEvent(t, r)
	require.Equal(t, "board", board.name)
	require.Contains(t, board.data, `<div class="points">1</div>`)
	require.Contains(t, board.data, "<strong>0:1</strong>", "the title the panel is given")

	timeline := readEvent(t, r)
	require.Equal(t, "log", timeline.name)
	require.Contains(t, timeline.data, "12:00:00")
	require.Contains(t, timeline.data, "0:1")
	require.Contains(t, timeline.data, "Anna 0 : 1 Bernd")
}

func TestASetWinIsSaidInThePanelsWords(t *testing.T) {
	f := newStreamFixture(t)
	r, _ := f.connect()
	readEvent(t, r)

	for range 11 {
		f.point(scorer.PlayerA)
	}

	var board, timeline sseEvent
	for range 24 {
		ev := readEvent(t, r)
		if ev.name == "board" {
			board = ev
			continue
		}
		timeline = ev
		if strings.Contains(ev.data, "SET 1:0") {
			break
		}
	}

	require.Contains(t, board.data, "<strong>SET 1:0</strong>")
	require.Contains(t, timeline.data, "SET 1:0")
	require.Contains(t, timeline.data, "sev-SUCCESS", "a set win is the one thing that stands out")
}

func TestAClosedTabReleasesItsSubscription(t *testing.T) {
	f := newStreamFixture(t)
	r, cancel := f.connect()
	readEvent(t, r)

	require.Equal(t, 1, f.hub.Subscribers(f.match.ID))
	cancel()

	require.Eventually(t, func() bool { return f.hub.Matches() == 0 }, time.Second, 10*time.Millisecond,
		"a subscription left in the hub outlives the tab that opened it")
}

func TestTheStreamOfAnUnknownMatchIsNotFound(t *testing.T) {
	f := newStreamFixture(t)

	resp, err := f.server.Client().Get(f.server.URL + "/ui/matches/deadbeef/stream")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestAnIdleStreamIsKeptAlive(t *testing.T) {
	f := newStreamFixture(t, WithHeartbeat(20*time.Millisecond))
	r, _ := f.connect()
	readEvent(t, r)

	// The comment carries no data; it is there so that a proxy does not close a
	// connection that is quiet between rallies.
	line, err := r.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, ": ping\n", line)
}
