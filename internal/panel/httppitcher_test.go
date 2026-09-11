package panel

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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	homerun "github.com/stuttgart-things/homerun-library/v4"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// omniPitcher stubs homerun2-omni-pitcher's /pitch, closely enough that the
// tests exercise the request the real one accepts: the same JSON body, the
// same bearer auth, the same response shape.
type omniPitcher struct {
	mu       sync.Mutex
	received []homerun.Message
	tokens   []string
	paths    []string
	stream   string // reported back as streamId
	status   int    // answered when non-zero
	body     string // answered instead of JSON when non-empty
	server   *httptest.Server
}

func newOmniPitcher(t *testing.T) *omniPitcher {
	t.Helper()

	o := &omniPitcher{stream: "tabletennis"}
	o.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.paths = append(o.paths, r.URL.Path)
		o.tokens = append(o.tokens, r.Header.Get("Authorization"))
		status, body, stream := o.status, o.body, o.stream
		o.mu.Unlock()

		if status != 0 {
			w.WriteHeader(status)
			if body != "" {
				_, _ = io.WriteString(w, body)
			}
			return
		}

		var msg homerun.Message
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// The real one requires both.
		if msg.Title == "" || msg.Message == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"Title is required"}`)
			return
		}

		o.mu.Lock()
		o.received = append(o.received, msg)
		o.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(pitchResponse{
			ObjectID: "obj-1-" + msg.System,
			StreamID: stream,
			Status:   "success",
			Message:  "Message successfully enqueued",
		})
	}))
	t.Cleanup(o.server.Close)
	return o
}

func (o *omniPitcher) messages() []homerun.Message {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]homerun.Message(nil), o.received...)
}

func (o *omniPitcher) lastToken() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.tokens) == 0 {
		return ""
	}
	return o.tokens[len(o.tokens)-1]
}

func (o *omniPitcher) lastPath() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.paths) == 0 {
		return ""
	}
	return o.paths[len(o.paths)-1]
}

func (o *omniPitcher) fail(status int, body string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.status, o.body = status, body
}

func (o *omniPitcher) reportStream(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stream = s
}

func testMessage() homerun.Message {
	return Message(
		scorer.Transition{Kind: scorer.TransitionPoint, State: state([2]int{7, 5}, [2]int{1, 0})},
		DefaultSystem, DefaultAuthor, time.Now(),
	)
}

func TestAPitchArrivesAsTheMessageWeBuilt(t *testing.T) {
	o := newOmniPitcher(t)
	p := NewHTTPPitcher(HTTPPitcherConfig{BaseURL: o.server.URL, Logger: quiet()})
	require.NotNil(t, p)
	defer func() { require.NoError(t, p.Close()) }()

	objectID, streamID, err := p.Enqueue(context.Background(), testMessage())
	require.NoError(t, err)
	require.Equal(t, "obj-1-tabletennis", objectID)
	require.Equal(t, "tabletennis", streamID)

	got := o.messages()
	require.Len(t, got, 1)
	require.Equal(t, "tabletennis", got[0].System)
	require.Equal(t, "7:5", got[0].Title)
	require.Equal(t, "Anna 7 : 5 Bernd", got[0].Message)
	require.Equal(t, "INFO", got[0].Severity)
	require.Equal(t, "zaehlwerk", got[0].Author)
	require.Equal(t, "match=m1,set=2,transition=point", got[0].Tags)
	require.NotEmpty(t, got[0].Timestamp)
}

// Both are required by the real endpoint; a message missing either is a 400,
// not a lost point that nobody notices.
func TestEveryTransitionSendsATitleAndAMessage(t *testing.T) {
	o := newOmniPitcher(t)
	p := NewHTTPPitcher(HTTPPitcherConfig{BaseURL: o.server.URL, Logger: quiet()})
	require.NotNil(t, p)

	for _, kind := range []scorer.TransitionKind{
		scorer.TransitionPoint, scorer.TransitionUndo,
		scorer.TransitionSetWon, scorer.TransitionMatchWon,
	} {
		msg := Message(
			scorer.Transition{Kind: kind, State: state([2]int{0, 0}, [2]int{0, 0})},
			DefaultSystem, DefaultAuthor, time.Now(),
		)
		_, _, err := p.Enqueue(context.Background(), msg)
		require.NoError(t, err, "kind %s was rejected", kind)
	}
	require.Len(t, o.messages(), 4)
}

func TestTheBearerTokenIsSent(t *testing.T) {
	o := newOmniPitcher(t)
	p := NewHTTPPitcher(HTTPPitcherConfig{BaseURL: o.server.URL, Token: "s3cret", Logger: quiet()})
	require.NotNil(t, p)

	_, _, err := p.Enqueue(context.Background(), testMessage())
	require.NoError(t, err)
	require.Equal(t, "Bearer s3cret", o.lastToken())
}

func TestWithNoTokenNoAuthorizationHeaderIsSent(t *testing.T) {
	o := newOmniPitcher(t)
	p := NewHTTPPitcher(HTTPPitcherConfig{BaseURL: o.server.URL, Logger: quiet()})
	require.NotNil(t, p)

	_, _, err := p.Enqueue(context.Background(), testMessage())
	require.NoError(t, err)
	require.Empty(t, o.lastToken())
}

func TestThePathDefaultsToPitchAndIsOverridable(t *testing.T) {
	o := newOmniPitcher(t)

	p := NewHTTPPitcher(HTTPPitcherConfig{BaseURL: o.server.URL, Logger: quiet()})
	_, _, err := p.Enqueue(context.Background(), testMessage())
	require.NoError(t, err)
	require.Equal(t, "/pitch", o.lastPath())

	q := NewHTTPPitcher(HTTPPitcherConfig{BaseURL: o.server.URL, Path: "other", Logger: quiet()})
	_, _, err = q.Enqueue(context.Background(), testMessage())
	require.NoError(t, err)
	require.Equal(t, "/other", o.lastPath())
}

// A trailing slash on the URL and a leading one on the path must not produce
// //pitch, which some ingresses answer with a redirect the client will not
// repost to.
func TestTheURLIsJoinedWithoutDoubleSlashes(t *testing.T) {
	o := newOmniPitcher(t)
	p := NewHTTPPitcher(HTTPPitcherConfig{BaseURL: o.server.URL + "/", Path: "/pitch", Logger: quiet()})

	_, _, err := p.Enqueue(context.Background(), testMessage())
	require.NoError(t, err)
	require.Equal(t, "/pitch", o.lastPath())
}

// A 401 is a configuration mistake, not a transient fault, and the message
// should say so rather than read as "the bus is down".
func TestARejectedTokenSaysSo(t *testing.T) {
	o := newOmniPitcher(t)
	o.fail(http.StatusUnauthorized, "Unauthorized")
	p := NewHTTPPitcher(HTTPPitcherConfig{BaseURL: o.server.URL, Token: "wrong", Logger: quiet()})

	_, _, err := p.Enqueue(context.Background(), testMessage())
	require.Error(t, err)
	require.Contains(t, err.Error(), "token")
}

func TestARejectedMessageCarriesTheServersReason(t *testing.T) {
	o := newOmniPitcher(t)
	o.fail(http.StatusBadRequest, `{"error":"Title is required"}`)
	p := NewHTTPPitcher(HTTPPitcherConfig{BaseURL: o.server.URL, Logger: quiet()})

	_, _, err := p.Enqueue(context.Background(), testMessage())
	require.Error(t, err)
	require.Contains(t, err.Error(), "Title is required")
}

func TestAServerErrorIsAnError(t *testing.T) {
	o := newOmniPitcher(t)
	o.fail(http.StatusServiceUnavailable, "")
	p := NewHTTPPitcher(HTTPPitcherConfig{BaseURL: o.server.URL, Logger: quiet()})

	_, _, err := p.Enqueue(context.Background(), testMessage())
	require.Error(t, err)
	require.Contains(t, err.Error(), "503")
}

func TestAnUnreachablePitcherIsAnError(t *testing.T) {
	// Port 1 is reserved and nothing listens there.
	p := NewHTTPPitcher(HTTPPitcherConfig{
		BaseURL: "http://127.0.0.1:1", Timeout: 100 * time.Millisecond, Logger: quiet(),
	})
	require.NotNil(t, p)

	start := time.Now()
	_, _, err := p.Enqueue(context.Background(), testMessage())
	require.Error(t, err)
	require.Less(t, time.Since(start), 2*time.Second)
}

// The failure that is otherwise invisible: without a route for our system,
// omni-pitcher puts the score on its default stream. The pitch succeeds, and
// the panel — switched to the match stream — shows nothing.
func TestALandingOnTheWrongStreamIsReported(t *testing.T) {
	o := newOmniPitcher(t)
	o.reportStream("messages")

	var logged bytes.Buffer
	p := NewHTTPPitcher(HTTPPitcherConfig{
		BaseURL:      o.server.URL,
		ExpectStream: "tabletennis",
		Logger:       slog.New(slog.NewTextHandler(&logged, nil)),
	})

	_, streamID, err := p.Enqueue(context.Background(), testMessage())
	require.NoError(t, err, "a pitch that succeeded must not be reported as failed")
	require.Equal(t, "messages", streamID)

	out := logged.String()
	require.Contains(t, out, "different stream")
	require.Contains(t, out, "tabletennis")
	require.Contains(t, out, "messages")
}

// Once, not per point: a missing route would otherwise write a line for every
// rally of every match.
func TestTheWrongStreamIsReportedOnceNotPerPoint(t *testing.T) {
	o := newOmniPitcher(t)
	o.reportStream("messages")

	var logged bytes.Buffer
	p := NewHTTPPitcher(HTTPPitcherConfig{
		BaseURL:      o.server.URL,
		ExpectStream: "tabletennis",
		Logger:       slog.New(slog.NewTextHandler(&logged, nil)),
	})

	for range 20 {
		_, _, err := p.Enqueue(context.Background(), testMessage())
		require.NoError(t, err)
	}
	require.Equal(t, 1, strings.Count(logged.String(), "different stream"))
}

func TestTheRightStreamIsNotReported(t *testing.T) {
	o := newOmniPitcher(t)
	o.reportStream("tabletennis")

	var logged bytes.Buffer
	p := NewHTTPPitcher(HTTPPitcherConfig{
		BaseURL:      o.server.URL,
		ExpectStream: "tabletennis",
		Logger:       slog.New(slog.NewTextHandler(&logged, nil)),
	})

	_, _, err := p.Enqueue(context.Background(), testMessage())
	require.NoError(t, err)
	require.NotContains(t, logged.String(), "different stream")
}

// Sink passes its configured stream as the override; with no ExpectStream set
// that is what the check should compare against.
func TestTheOverrideActsAsTheExpectationWhenNoneIsConfigured(t *testing.T) {
	o := newOmniPitcher(t)
	o.reportStream("messages")

	var logged bytes.Buffer
	p := NewHTTPPitcher(HTTPPitcherConfig{
		BaseURL: o.server.URL,
		Logger:  slog.New(slog.NewTextHandler(&logged, nil)),
	})

	_, _, err := p.Enqueue(context.Background(), testMessage(), "tabletennis")
	require.NoError(t, err)
	require.Contains(t, logged.String(), "different stream")
}

func TestNoURLMeansNoPitcher(t *testing.T) {
	require.Nil(t, NewHTTPPitcher(HTTPPitcherConfig{}))
}

func TestHTTPConfigDefaults(t *testing.T) {
	cfg := HTTPPitcherConfig{BaseURL: "http://x"}.withDefaults()
	require.Equal(t, DefaultPitchPath, cfg.Path)
	require.Equal(t, DefaultPitchTimeoutHTTP, cfg.Timeout)
	require.NotNil(t, cfg.Logger)
}

func TestCloseIsIdempotent(t *testing.T) {
	o := newOmniPitcher(t)
	p := NewHTTPPitcher(HTTPPitcherConfig{BaseURL: o.server.URL, Logger: quiet()})

	require.NoError(t, p.Close())
	require.NoError(t, p.Close())
}

// The whole point of implementing Pitcher: Sink works with either backend
// unchanged.
func TestTheSinkWorksThroughTheHTTPPitcher(t *testing.T) {
	o := newOmniPitcher(t)
	p := NewHTTPPitcher(HTTPPitcherConfig{BaseURL: o.server.URL, Logger: quiet()})

	sink := New(p, Config{Stream: "tabletennis", Logger: quiet()})
	sc := newScorer(t, sink)

	for i := range uint64(3) {
		point(t, sc, scorer.PlayerA, i+1)
	}
	require.NoError(t, sink.Close())

	got := o.messages()
	require.Len(t, got, 3)
	titles := []string{got[0].Title, got[1].Title, got[2].Title}
	require.Equal(t, []string{"1:0", "2:0", "3:0"}, titles)
	require.Zero(t, sink.Failed())
	require.Zero(t, sink.Dropped())
}

func TestAFailingPitcherDoesNotStopTheMatchOverHTTP(t *testing.T) {
	o := newOmniPitcher(t)
	o.fail(http.StatusServiceUnavailable, "")
	p := NewHTTPPitcher(HTTPPitcherConfig{BaseURL: o.server.URL, Logger: quiet()})

	sink := New(p, Config{Logger: quiet()})
	sc := newScorer(t, sink)
	for i := range uint64(5) {
		point(t, sc, scorer.PlayerA, i+1)
	}

	waitFor(t, "five failed pitches", func() bool { return sink.Failed() == 5 })
	require.Equal(t, 5, sc.State().Points[0])
	require.NoError(t, sink.Close())
}
