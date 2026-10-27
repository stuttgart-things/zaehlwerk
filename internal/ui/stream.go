package ui

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/face"
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
//
// ?view=watch is the spectator view's feed: the same transitions, rendered
// without buttons and sent as a "spectator" event, and no timeline, which
// that view does not show.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	board, withLog := "board", true
	if r.URL.Query().Get("view") == "watch" {
		board, withLog = "spectator", false
	}

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

	rc := openEvents(w)

	var rows []logRow
	// streak is the faces' memory, this connection's like the timeline: the
	// points in a row a side has lost since the page opened (ADR-0018).
	var streak face.Streak
	if err := s.send(rc, w, board, s.liveView(m, kindOf(m.Scorer.State()))); err != nil {
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
			// The handover is the match's, not the transition's, so it is
			// added here: without it the card would go at match_won, the
			// moment it has something to say.
			v.Match.Reported = s.reportView(m, ev.State)
			stake(v.Match, m, ev.State)
			s.paint(v.Match, m, ev.State)
			var faces face.Faces
			faces, streak = face.Read(moment(m, ev.Kind, ev.Side, ev.State), streak)
			s.dress(v.Match, m, faces)

			if ev.Kind != live.KindSnapshot {
				rows = append([]logRow{s.logRow(kind, ev.State)}, rows...)
				if len(rows) > logRows {
					rows = rows[:logRows]
				}
			}
			v.Log = rows

			if err := s.send(rc, w, board, v); err != nil {
				s.log.DebugContext(r.Context(), "ui stream ended", "match", m.ID, "error", err)
				return
			}
			if !withLog {
				continue
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

// table tells a following page that the match at the table may have changed,
// so that it asks its /next at once rather than at its next poll.
//
//	GET /ui/table
//
// It says no more than that, as an event named "table" whose data is the
// kind: which match the page should show stays with [Server.next] and
// [Server.watchNext], which know what the page does not — a screen that
// leaves demos out, the side select's hold, a result still on its way to
// Schmetterpause. The stream is only the moment to ask.
//
// So it passes on the moments a page might move: a match started, one ended,
// and one won, which is where the side select's hold begins. Every point
// would ask /next for nothing. And it sends one on connect, because a match
// that started between the page being rendered and this connection opening
// would otherwise wait for the poll.
//
// A page holds this next to the stream of the match it shows, which is one
// match's and ends with it — that one cannot say another has begun.
func (s *Server) table(w http.ResponseWriter, r *http.Request) {
	if s.hub == nil {
		http.Error(w, "live updates are not enabled", http.StatusServiceUnavailable)
		return
	}

	sub := s.hub.SubscribeTable()
	defer sub.Close()

	rc := openEvents(w)
	if err := s.writeRaw(rc, w, tableEvent(live.KindSnapshot)); err != nil {
		s.log.DebugContext(r.Context(), "ui table stream ended on connect", "error", err)
		return
	}

	heartbeat := time.NewTicker(s.heartbeat)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case ev := <-sub.Events():
			if !movesOn(ev.Kind) {
				continue
			}
			if err := s.writeRaw(rc, w, tableEvent(ev.Kind)); err != nil {
				s.log.DebugContext(r.Context(), "ui table stream ended", "error", err)
				return
			}

		case <-heartbeat.C:
			if err := s.writeRaw(rc, w, []byte(": ping\n\n")); err != nil {
				s.log.DebugContext(r.Context(), "ui table stream heartbeat failed", "error", err)
				return
			}
		}
	}
}

// movesOn says an event on the table may change what a following page shows.
func movesOn(kind string) bool {
	switch kind {
	case live.KindStarted, live.KindEnded, string(scorer.TransitionMatchWon):
		return true
	}
	return false
}

// tableEvent is one "table" event. The kind is its data because an event
// without data is never dispatched by the browser.
func tableEvent(kind string) []byte {
	return []byte("event: table\ndata: " + kind + "\n\n")
}

// openEvents answers with the headers of an event stream.
func openEvents(w http.ResponseWriter) *http.ResponseController {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	return http.NewResponseController(w)
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
