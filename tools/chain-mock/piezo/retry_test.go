package piezo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// outage sits between the board and zaehlwerk and fails the requests its
// rule picks, the two ways a rollout fails them (#133):
//   - down: zaehlwerk never sees the request, the gateway answers 503
//   - lost: zaehlwerk handles it, then the connection drops before the answer
type outage struct {
	next http.Handler

	mu    sync.Mutex
	n     int // requests so far
	rule  func(n int, r *http.Request) string
	fails int
}

func (o *outage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	o.n++
	what := o.rule(o.n, r)
	if what != "" {
		o.fails++
	}
	o.mu.Unlock()

	switch what {
	case "down":
		http.Error(w, "no healthy upstream", http.StatusServiceUnavailable)
	case "lost":
		o.next.ServeHTTP(httptest.NewRecorder(), r)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	default:
		o.next.ServeHTTP(w, r)
	}
}

func (o *outage) failed() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.fails
}

func behind(t *testing.T, zw *httptest.Server, rule func(n int, r *http.Request) string) (*httptest.Server, *outage) {
	t.Helper()
	o := &outage{next: zw.Config.Handler, rule: rule}
	srv := httptest.NewServer(o)
	t.Cleanup(srv.Close)
	return srv, o
}

func fast(b *Board) *Board {
	b.retryFirst, b.retryMax = time.Millisecond, 5*time.Millisecond
	return b
}

// zaehlwerk goes away in the middle of a match, as it does while it rolls
// out: the board waits, comes back, and finishes the match into
// Schmetterpause instead of stopping.
func TestABoardRidesOutAZaehlwerkRollout(t *testing.T) {
	f, sp := fake(t)
	zw := chain(t, sp)
	front, o := behind(t, zw, func(n int, r *http.Request) string {
		switch {
		case n >= 10 && n < 20:
			return "down"
		case n >= 20 && n < 23:
			return "lost"
		}
		return ""
	})

	b := fast(New(Config{API: front.URL, Source: "piezo-test", Matches: 1, Seed: 5, BestOf: 1, Roster: sp}, quiet))
	require.NoError(t, b.Run(context.Background()))

	require.Eventually(t, func() bool { return len(f.Results()) == 1 }, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, 13, o.failed(), "the outage has to hit the match, or the test proves nothing")
}

// The rally reached zaehlwerk and only the answer got lost. The board sends
// the same event again; zaehlwerk knows its event_id, so it stays one point.
func TestALostAnswerIsNotASecondPoint(t *testing.T) {
	_, sp := fake(t)
	zw := chain(t, sp)
	lost := false
	front, o := behind(t, zw, func(_ int, r *http.Request) string {
		if r.Method == http.MethodPost && r.URL.Path == "/ingest/piezo" && !lost {
			lost = true
			return "lost"
		}
		return ""
	})

	b := fast(New(Config{API: front.URL, Source: "piezo-test", Seed: 9, BestOf: 3, Roster: sp}, quiet))
	ctx := context.Background()
	_, err := b.create(ctx)
	require.NoError(t, err)
	st, err := b.join(ctx)
	require.NoError(t, err)

	require.NoError(t, b.rally(ctx, st))
	require.Equal(t, 1, o.failed())

	var now state
	now, _, err = b.call(ctx, http.MethodGet, "/matches/current", nil)
	require.NoError(t, err)
	require.Equal(t, 1, now.Points[0]+now.Points[1], "one rally, sent twice, is one point")
}

// Switched off while zaehlwerk is away: the board stops on the context, which
// cmd/piezo treats as a clean stop, not as an error.
func TestABoardWaitingForZaehlwerkStopsWhenSwitchedOff(t *testing.T) {
	zw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no healthy upstream", http.StatusBadGateway)
	}))
	t.Cleanup(zw.Close)

	b := fast(New(Config{API: zw.URL, Source: "piezo-test", Join: true}, quiet))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, b.Run(ctx), context.DeadlineExceeded)
}

// A 4xx is an answer, not an outage: the board does not sit on it.
func TestABoardDoesNotRetryARefusal(t *testing.T) {
	calls := 0
	zw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, `{"error":"no"}`, http.StatusBadRequest)
	}))
	t.Cleanup(zw.Close)

	b := fast(New(Config{API: zw.URL, Source: "piezo-test", Join: true}, quiet))
	require.ErrorContains(t, b.Run(context.Background()), "GET /matches/current: 400")
	require.Equal(t, 1, calls)
}
