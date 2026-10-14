package buttons

import (
	"context"
	"net/http/httptest"
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
		api.WithCheckin(checkin.NewDesk(sp, checkin.WithLogger(quiet)))))
	t.Cleanup(srv.Close)
	return srv, registry, fake
}

func TestAFobThenAPressChecksInAtThatHalf(t *testing.T) {
	zw, registry, fake := checkedInTable(t)
	r := rig(t, zw, nil)

	require.NoError(t, r.HoldFob("Anna's fob"))
	require.Equal(t, "Anna's fob", r.FobHeld())
	press(t, r, "A", 100*time.Millisecond)
	require.Empty(t, r.FobHeld(), "the press used the fob up")

	require.NoError(t, r.HoldFob("Bernd's fob"))
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

	require.NoError(t, r.HoldFob("Anna's fob"))
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

	for _, f := range Fobs {
		require.NoError(t, r.HoldFob(f.Label))
		press(t, r, "A", 100*time.Millisecond)
	}
	var log strings.Builder
	for _, e := range r.Entries() {
		log.WriteString(e.Text + "\n")
	}
	require.Contains(t, log.String(), "unknown fob")
	for _, f := range Fobs {
		require.NotContains(t, log.String(), f.UID)
	}
}

func TestAFobWithoutAPressIsDropped(t *testing.T) {
	zw, registry, fake := checkedInTable(t)
	r := rig(t, zw, nil)

	require.NoError(t, r.HoldFob("Clara's fob"))
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
