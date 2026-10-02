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
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/live"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// tableFixture is a server with a hub and no match yet, for GET /live/stream.
type tableFixture struct {
	t        *testing.T
	hub      *live.Hub
	registry *match.Registry
	api      *Server
	server   *httptest.Server
}

func newTableFixture(t *testing.T) *tableFixture {
	t.Helper()
	hub := live.New()
	registry := match.NewRegistry(match.WithObserver(hub.Observe))
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := New(registry, WithLogger(quiet), WithHub(hub))
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	return &tableFixture{t: t, hub: hub, registry: registry, api: api, server: srv}
}

func (f *tableFixture) connect() (*bufio.Reader, context.CancelFunc) {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.server.URL+"/live/stream", nil)
	require.NoError(f.t, err)
	resp, err := f.server.Client().Do(req)
	if err != nil {
		cancel()
		f.t.Fatalf("connecting: %v", err)
	}
	f.t.Cleanup(func() { cancel(); _ = resp.Body.Close() })
	require.Equal(f.t, http.StatusOK, resp.StatusCode)
	require.Equal(f.t, "text/event-stream", resp.Header.Get("Content-Type"))
	return bufio.NewReader(resp.Body), cancel
}

// start begins a best of one through the server, as the UI and the JSON API do.
func (f *tableFixture) start(handover match.Handover) *match.Match {
	f.t.Helper()
	m, err := f.api.StartMatch(scorer.Config{Players: [2]string{"Anna", "Bernd"}, BestOf: 1}, handover)
	require.NoError(f.t, err)
	return m
}

func point(t *testing.T, m *match.Match, p scorer.Player, id uint64) {
	t.Helper()
	_, err := m.Scorer.Apply(scorer.ScoreEvent{MatchID: m.ID, Player: p, Delta: 1, EventID: id, Source: "test"})
	require.NoError(t, err)
}

func readTableEvent(t *testing.T, r *bufio.Reader) tableEvent {
	t.Helper()
	ch := make(chan tableEvent, 1)
	errs := make(chan error, 1)
	go func() {
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				errs <- err
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev tableEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				errs <- err
				return
			}
			ch <- ev
			return
		}
	}()
	select {
	case ev := <-ch:
		return ev
	case err := <-errs:
		t.Fatalf("reading the table stream: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an event on the table stream")
	}
	return tableEvent{}
}

func TestTheTableStreamSaysNoMatchOnConnectBetweenMatches(t *testing.T) {
	f := newTableFixture(t)
	r, _ := f.connect()

	ev := readTableEvent(t, r)
	require.Equal(t, KindNoMatch, ev.Kind)
	require.Nil(t, ev.State)
}

func TestTheTableStreamSendsTheRunningMatchOnConnect(t *testing.T) {
	f := newTableFixture(t)
	m := f.start(match.Handover{})
	point(t, m, scorer.PlayerA, 1)

	r, _ := f.connect()
	ev := readTableEvent(t, r)
	require.Equal(t, live.KindSnapshot, ev.Kind)
	require.Equal(t, m.ID, ev.State.MatchID)
	require.Equal(t, [2]int{1, 0}, ev.State.Points)
}

func TestANewMatchReachesTheTableStreamWithoutReconnecting(t *testing.T) {
	f := newTableFixture(t)
	r, _ := f.connect()
	require.Equal(t, KindNoMatch, readTableEvent(t, r).Kind)

	m := f.start(match.Handover{})
	ev := readTableEvent(t, r)
	require.Equal(t, live.KindSnapshot, ev.Kind, "a started match arrives as its snapshot")
	require.Equal(t, m.ID, ev.State.MatchID)

	point(t, m, scorer.PlayerB, 1)
	ev = readTableEvent(t, r)
	require.Equal(t, string(scorer.TransitionPoint), ev.Kind)
	require.Equal(t, [2]int{0, 1}, ev.State.Points)
}

func TestAWonMatchEndsInItsFinalStateThenNoMatch(t *testing.T) {
	f := newTableFixture(t)
	m := f.start(match.Handover{})
	r, _ := f.connect()
	require.Equal(t, live.KindSnapshot, readTableEvent(t, r).Kind)

	for i := uint64(1); i <= 11; i++ {
		point(t, m, scorer.PlayerA, i)
	}

	var last tableEvent
	for {
		last = readTableEvent(t, r)
		if last.Kind == string(scorer.TransitionMatchWon) {
			break
		}
	}
	require.True(t, last.State.Complete)
	require.Equal(t, KindNoMatch, readTableEvent(t, r).Kind)
}

func TestAMatchEndedEarlyEndsThenNoMatch(t *testing.T) {
	f := newTableFixture(t)
	m := f.start(match.Handover{})
	r, _ := f.connect()
	require.Equal(t, live.KindSnapshot, readTableEvent(t, r).Kind)

	f.api.EndMatch(m)
	require.Equal(t, live.KindEnded, readTableEvent(t, r).Kind)
	require.Equal(t, KindNoMatch, readTableEvent(t, r).Kind)
}

func TestTheTableStreamCarriesPlayerIDsExactlyWhenTheMatchIsReported(t *testing.T) {
	f := newTableFixture(t)
	r, _ := f.connect()
	require.Equal(t, KindNoMatch, readTableEvent(t, r).Kind)

	reported := f.start(match.Handover{HomeID: "id-anna", AwayID: "id-bernd", OperatorID: "id-olga"})
	ev := readTableEvent(t, r)
	require.Equal(t, "id-anna", ev.HomeID)
	require.Equal(t, "id-bernd", ev.AwayID)
	point(t, reported, scorer.PlayerA, 1)
	ev = readTableEvent(t, r)
	require.Equal(t, "id-anna", ev.HomeID, "every event of a reported match carries the ids")

	f.api.EndMatch(reported)
	require.Equal(t, live.KindEnded, readTableEvent(t, r).Kind)
	require.Equal(t, KindNoMatch, readTableEvent(t, r).Kind)

	f.start(match.Handover{})
	ev = readTableEvent(t, r)
	require.Equal(t, live.KindSnapshot, ev.Kind)
	require.Empty(t, ev.HomeID, "a match nobody reports has no ids")
	require.Empty(t, ev.AwayID)
}

func TestTheTableSubscriptionIsReleasedOnDisconnect(t *testing.T) {
	f := newTableFixture(t)
	r, cancel := f.connect()
	readTableEvent(t, r)
	waitFor(t, "the table subscription", func() bool { return f.hub.TableSubscribers() == 1 })

	cancel()
	waitFor(t, "the table subscription to be released", func() bool { return f.hub.TableSubscribers() == 0 })
}

func TestTheTableStreamWithoutAHubIs503(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(New(match.NewRegistry(), WithLogger(quiet)))
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/live/stream")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}
