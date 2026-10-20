package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/checkin"
	"github.com/stuttgart-things/zaehlwerk/internal/live"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// The pages that follow the table move on when the table stream says a match
// started or ended, and only poll slowly besides. A way of starting a match
// that does not say so leaves them on the old one for the length of that
// poll, so every way there is is pinned here.

// announcingClient is a server with a hub and check-in on, and a table
// subscription that sees what it announces.
func announcingClient(t *testing.T) (*client, *live.Subscription) {
	t.Helper()

	sp := &spStub{}
	spSrv := httptest.NewServer(sp.handler(t))
	t.Cleanup(spSrv.Close)
	spClient, err := schmetterpause.New(schmetterpause.Config{BaseURL: spSrv.URL, Token: "tok"})
	require.NoError(t, err)

	hub := live.New()
	registry := match.NewRegistry(match.WithObserver(hub.Observe))
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	desk := checkin.NewDesk(spClient, checkin.WithLogger(quiet))

	sub := hub.SubscribeTable()
	t.Cleanup(sub.Close)

	return &client{
		t:        t,
		server:   New(registry, WithLogger(quiet), WithHub(hub), WithCheckin(desk, hubToken)),
		registry: registry,
		bearer:   hubToken,
	}, sub
}

// announced reads the table subscription up to the next match_started or
// match_ended, skipping the scorer's own transitions.
func announced(t *testing.T, sub *live.Subscription) live.Event {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-sub.Events():
			if ev.Kind == live.KindStarted || ev.Kind == live.KindEnded {
				return ev
			}
		case <-timeout:
			t.Fatal("nothing was announced to the table")
			return live.Event{}
		}
	}
}

func TestEveryWayAMatchStartsTellsTheTable(t *testing.T) {
	t.Run("through the JSON API", func(t *testing.T) {
		c, sub := announcingClient(t)
		id := c.newMatch(`{"players":["Anna","Bernd"]}`)

		ev := announced(t, sub)
		require.Equal(t, live.KindStarted, ev.Kind)
		require.Equal(t, id, ev.State.MatchID)
	})

	// The demo, the browser game, the bot game and the button check of
	// zaehlwerk-pong are all this request (ADR-0008, ADR-0014).
	t.Run("as a demo, the way zaehlwerk-pong starts every game", func(t *testing.T) {
		c, sub := announcingClient(t)
		id := c.newMatch(`{"players":["Pong A","Pong B"],"start":"warmup","change_ends":true,"demo":true}`)

		ev := announced(t, sub)
		require.Equal(t, live.KindStarted, ev.Kind)
		require.Equal(t, id, ev.State.MatchID)
	})

	t.Run("from the form on the scoring page", func(t *testing.T) {
		c, sub := announcingClient(t)
		m, err := c.server.StartMatch(scorer.Config{Players: [2]string{"Anna", "Bernd"}}, match.Handover{})
		require.NoError(t, err)

		ev := announced(t, sub)
		require.Equal(t, live.KindStarted, ev.Kind)
		require.Equal(t, m.ID, ev.State.MatchID)
	})

	t.Run("by a check-in completing the second half", func(t *testing.T) {
		c, sub := announcingClient(t)
		c.checkIn(http.StatusOK, checkinBody(1, "left", "04:a3:1b:22"))
		a := c.checkIn(http.StatusOK, checkinBody(2, "right", "04:5c:7e:91"))
		require.Equal(t, checkin.Started, a.Outcome)

		ev := announced(t, sub)
		require.Equal(t, live.KindStarted, ev.Kind)
		require.Equal(t, a.MatchID, ev.State.MatchID)
	})
}

// A real match ends the demo it displaces inside the registry, where EndMatch
// is never called: the table hears the end first, then the start.
func TestARealMatchTakingTheTableFromADemoTellsTheTableBoth(t *testing.T) {
	c, sub := announcingClient(t)
	demo := c.newMatch(`{"players":["Pong A","Pong B"],"demo":true}`)
	require.Equal(t, live.KindStarted, announced(t, sub).Kind)

	played := c.newMatch(`{"players":["Anna","Bernd"]}`)

	ev := announced(t, sub)
	require.Equal(t, live.KindEnded, ev.Kind)
	require.Equal(t, demo, ev.State.MatchID)
	ev = announced(t, sub)
	require.Equal(t, live.KindStarted, ev.Kind)
	require.Equal(t, played, ev.State.MatchID)
}

func TestEndingAMatchTellsTheTable(t *testing.T) {
	c, sub := announcingClient(t)
	id := c.newMatch(`{"players":["Anna","Bernd"]}`)
	announced(t, sub)

	c.state(http.StatusOK, http.MethodPost, "/matches/"+id+"/end", "")

	ev := announced(t, sub)
	require.Equal(t, live.KindEnded, ev.Kind)
	require.Equal(t, id, ev.State.MatchID)
}
