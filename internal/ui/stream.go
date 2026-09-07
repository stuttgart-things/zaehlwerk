package ui

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/live"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

const (
	// writeTimeout bounds one write to one browser. There is no WriteTimeout on
	// the server — it would cut off every stream — so the bound has to be per
	// write, or a tab whose TCP window has filled up holds a goroutine for as
	// long as it stays open.
	writeTimeout = 10 * time.Second

	// logRows is how much of the timeline a connection keeps. It is per
	// connection deliberately: nothing here stores history, so the page shows
	// what has happened since it was opened, which is also what the person in
	// front of it saw.
	logRows = 20
)

// stream feeds one page: rendered HTML over Server-Sent Events.
//
//	GET /ui/matches/{id}/stream
//
// Two events per transition — the scoreboard and the timeline — because htmx
// swaps HTML here, exactly as the led-catcher's simulator does. That is the
// opposite call from GET /matches/{id}/stream, and deliberately so: the JSON
// stream serves clients that each render differently (a spectator view,
// Schmetterpause), while this one serves one page whose markup this package
// owns. Nothing about the JSON stream changes for a client that already reads
// it.
//
// A page that connects gets the current score straight away, so a tab opened
// mid-match renders without a second request.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	m, err := s.registry.Get(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if s.hub == nil {
		http.Error(w, "live updates are not enabled", http.StatusServiceUnavailable)
		return
	}

	// Subscribed before the state is read, so a point landing between the two
	// is queued rather than missed.
	sub := s.hub.Subscribe(m.ID)
	defer sub.Close()

	rc := http.NewResponseController(w)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	var rows []logRow
	if err := s.sendBoard(rc, w, s.liveView(m, kindOf(m.Scorer.State()))); err != nil {
		s.log.DebugContext(r.Context(), "ui stream ended on the snapshot", "match", m.ID, "error", err)
		return
	}

	heartbeat := time.NewTicker(s.heartbeat)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			// The tab was closed. The deferred Close releases the subscription;
			// leaving it in the hub is the leak the JSON stream is tested for.
			return

		case ev := <-sub.Events():
			kind := scorer.TransitionKind(ev.Kind)
			v := view{Match: s.stateView(ev.State, m.Running(), kind), Stream: true}

			if ev.Kind != live.KindSnapshot {
				rows = append([]logRow{s.logRow(kind, ev.State)}, rows...)
				if len(rows) > logRows {
					rows = rows[:logRows]
				}
			}
			v.Log = rows

			if err := s.sendBoard(rc, w, v); err != nil {
				s.log.DebugContext(r.Context(), "ui stream ended", "match", m.ID, "error", err)
				return
			}
			if err := s.send(rc, w, "log", v); err != nil {
				s.log.DebugContext(r.Context(), "ui stream ended", "match", m.ID, "error", err)
				return
			}

		case <-heartbeat.C:
			if err := s.writeRaw(rc, w, []byte(": ping\n\n")); err != nil {
				s.log.DebugContext(r.Context(), "ui stream heartbeat failed", "match", m.ID, "error", err)
				return
			}
		}
	}
}

func (s *Server) sendBoard(rc *http.ResponseController, w http.ResponseWriter, v view) error {
	return s.send(rc, w, "board", v)
}

// send renders one partial into one named SSE event.
//
// The payload is HTML and therefore has newlines in it, and a newline ends an
// SSE field — so every line goes out as its own `data:`. htmx joins them back
// with newlines before it swaps, which is exactly the markup that was rendered.
func (s *Server) send(rc *http.ResponseController, w http.ResponseWriter, name string, v view) error {
	html, err := partial(name, v)
	if err != nil {
		return fmt.Errorf("rendering %s: %w", name, err)
	}

	var b strings.Builder
	b.WriteString("event: ")
	b.WriteString(name)
	b.WriteString("\n")
	for _, line := range strings.Split(html, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n")

	return s.writeRaw(rc, w, []byte(b.String()))
}

func (s *Server) writeRaw(rc *http.ResponseController, w http.ResponseWriter, b []byte) error {
	// A deadline per write rather than per connection: the connection is meant
	// to last a whole match, one write is not. time.Now rather than the
	// injected clock — this is a deadline on a socket, and a test clock frozen
	// in the past would fail every write.
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
