package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/live"
)

// Defaults for the live stream.
const (
	// DefaultHeartbeat is how often a comment is written on an idle stream.
	// Proxies and load balancers close a connection that has been silent for
	// a minute or so, and a long rally is easily that.
	DefaultHeartbeat = 25 * time.Second

	// writeTimeout bounds one write to one client. The server has no
	// WriteTimeout of its own — it cannot, or it would cut off every stream —
	// so the bound has to be per write, or a client whose TCP window has
	// filled up holds its goroutine forever.
	writeTimeout = 10 * time.Second
)

// stream serves the live score of one match as Server-Sent Events.
//
//	GET /matches/{id}/stream
//
// The current state goes out on connect, so a client that joins mid-match
// renders immediately without a second request, and one event follows per
// transition. There is no polling and no periodic resend: a heartbeat comment
// keeps the connection open, and carries no data.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	if !s.cors(w, r) {
		return
	}

	m, ok := s.lookup(w, r)
	if !ok {
		return
	}

	if s.hub == nil {
		s.fail(w, r, http.StatusServiceUnavailable, errors.New("live streaming is not enabled"), nil)
		return
	}

	// Subscribe before reading the state, so a point landing between the two
	// is queued rather than missed. The client may then see the same state
	// twice, which is harmless — the state is complete, not a delta.
	sub := s.hub.Subscribe(m.ID)
	defer sub.Close()

	rc := http.NewResponseController(w)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Nginx buffers proxied responses by default, which holds events until the
	// buffer fills — for a score that is indistinguishable from a broken feed.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	snapshot := live.Event{Kind: live.KindSnapshot, State: m.Scorer.State()}
	if err := s.writeEvent(rc, w, snapshot); err != nil {
		s.log.DebugContext(r.Context(), "live stream ended on the snapshot", "match", m.ID, "error", err)
		return
	}

	heartbeat := time.NewTicker(s.heartbeat)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			// The client went away. The deferred Close is what actually
			// releases the subscription; a stream left in the hub here is the
			// goroutine leak the issue asks to be tested for.
			return

		case ev := <-sub.Events():
			if err := s.writeEvent(rc, w, ev); err != nil {
				s.log.DebugContext(r.Context(), "live stream ended", "match", m.ID, "error", err)
				return
			}

		case <-heartbeat.C:
			if err := s.writeRaw(rc, w, []byte(": ping\n\n")); err != nil {
				s.log.DebugContext(r.Context(), "live stream heartbeat failed", "match", m.ID, "error", err)
				return
			}
		}
	}
}

func (s *Server) writeEvent(rc *http.ResponseController, w http.ResponseWriter, ev live.Event) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshalling event: %w", err)
	}
	return s.writeRaw(rc, w, append(append([]byte("data: "), payload...), '\n', '\n'))
}

func (s *Server) writeRaw(rc *http.ResponseController, w http.ResponseWriter, b []byte) error {
	// A deadline per write rather than per connection: the connection is meant
	// to last a whole match, one write is not.
	//
	// time.Now, not s.now: this is a deadline on a socket, and a test clock
	// frozen at some fixed instant would put it in the past and fail every
	// write. The injected clock is for what goes into responses.
	if err := rc.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return fmt.Errorf("setting the write deadline: %w", err)
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return fmt.Errorf("flushing: %w", err)
	}
	return nil
}

// streamPreflight answers the CORS preflight for the stream route.
//
// EventSource itself never sends one — it is a plain GET with no custom
// headers — but a client reading the stream with fetch does, and answering it
// costs a route.
func (s *Server) streamPreflight(w http.ResponseWriter, r *http.Request) {
	if !s.cors(w, r) {
		return
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

// cors applies the origin policy, reporting whether the request may proceed.
//
// Allowed origins are configured, never `*`. This is a write-adjacent service
// on an internal network: Schmetterpause is served from another origin and
// needs to read the score, but that is a list, not everyone.
func (s *Server) cors(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Not a browser cross-origin request — curl, a server-side consumer,
		// or a same-origin fetch. Nothing to decide.
		return true
	}

	// Vary regardless of the outcome: the response differs by origin, and a
	// cache that missed that would serve one origin's answer to another.
	w.Header().Add("Vary", "Origin")

	if !slices.Contains(s.allowedOrigins, origin) {
		s.fail(w, r, http.StatusForbidden, fmt.Errorf("origin not allowed: %q", origin), nil)
		return false
	}

	w.Header().Set("Access-Control-Allow-Origin", origin)
	return true
}
