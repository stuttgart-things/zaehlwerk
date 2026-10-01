package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/live"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// KindNoMatch is the event the table stream sends when nothing is running: on
// connect between matches, and after the running match was won or ended.
const KindNoMatch = "no_match"

// tableEvent is one event on the table stream. State is absent from a
// no_match event; the ids are present exactly when the match is being
// reported to Schmetterpause (its Handover is wanted), and added here, at the
// API layer -- the scorer stays rule-only and knows players by name
// (invariant 5).
type tableEvent struct {
	Kind   string        `json:"kind"`
	State  *scorer.State `json:"state,omitempty"`
	HomeID string        `json:"home_id,omitempty"`
	AwayID string        `json:"away_id,omitempty"`
}

// liveStream serves the table rather than one match, as Server-Sent Events.
//
//	GET /live/stream
//
// A page that stays open all afternoon (Schmetterpause's running score,
// stuttgart-things/schmetterpause#188) cannot know a match id in advance, so
// this follows whatever match is current -- the same rule hardware ingest uses
// (Registry.Current):
//
//   - on connect, the current match's snapshot, or no_match;
//   - when another match becomes current, its snapshot, then its transitions,
//     without the client reconnecting;
//   - when the current match is won or ended, that final event, then the next
//     running match's snapshot if there is one, else no_match.
//
// Same guarantees as GET /matches/{id}/stream: CORS from ALLOWED_ORIGINS, a
// heartbeat, a deadline on every write, a slow client never blocks the scorer
// (the hub drops its oldest event), and the subscription is released on
// disconnect.
func (s *Server) liveStream(w http.ResponseWriter, r *http.Request) {
	if !s.cors(w, r) {
		return
	}
	if s.hub == nil {
		s.fail(w, r, http.StatusServiceUnavailable, errors.New("live streaming is not enabled"), nil)
		return
	}

	// Subscribe before looking up the current match, so a match started or a
	// point scored in between is queued rather than missed.
	sub := s.hub.SubscribeTable()
	defer sub.Close()

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	current := ""
	// follow points the stream at whatever is current now: its snapshot, or
	// no_match. It is what connect does, and what every end of a match does.
	follow := func() error {
		m, err := s.registry.Current()
		if err != nil {
			current = ""
			return s.writeTableEvent(rc, w, tableEvent{Kind: KindNoMatch})
		}
		current = m.ID
		return s.writeTableEvent(rc, w, s.withIDs(m, live.KindSnapshot, m.Scorer.State()))
	}

	if err := follow(); err != nil {
		s.log.DebugContext(r.Context(), "table stream ended on connect", "error", err)
		return
	}

	heartbeat := time.NewTicker(s.heartbeat)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case ev := <-sub.Events():
			if err := s.tableTransition(rc, w, ev, &current, follow); err != nil {
				s.log.DebugContext(r.Context(), "table stream ended", "match", ev.State.MatchID, "error", err)
				return
			}

		case <-heartbeat.C:
			if err := s.writeRaw(rc, w, []byte(": ping\n\n")); err != nil {
				s.log.DebugContext(r.Context(), "table stream heartbeat failed", "error", err)
				return
			}
		}
	}
}

// tableTransition handles one hub event for a table stream currently on match
// *current.
func (s *Server) tableTransition(rc *http.ResponseController, w http.ResponseWriter, ev live.Event, current *string, follow func() error) error {
	if ev.State.MatchID != *current {
		// Another match. It is the table's only if it is now current -- a match
		// just started, or a point on one that took over. Anything else is a
		// late event of a match the table has left behind.
		m, err := s.registry.Current()
		if err != nil || m.ID != ev.State.MatchID {
			return nil
		}
		// The fresh state already contains ev, so the snapshot replaces it.
		*current = m.ID
		return s.writeTableEvent(rc, w, s.withIDs(m, live.KindSnapshot, m.Scorer.State()))
	}

	m, err := s.registry.Get(ev.State.MatchID)
	if err != nil {
		// Evicted under us; nothing left to name the players by.
		m = nil
	}
	if err := s.writeTableEvent(rc, w, s.withIDs(m, ev.Kind, ev.State)); err != nil {
		return err
	}
	if ev.State.Complete || ev.Kind == live.KindEnded {
		return follow()
	}
	return nil
}

// withIDs builds a table event, adding the players' Schmetterpause ids when the
// match is being reported. m may be nil.
func (s *Server) withIDs(m *match.Match, kind string, st scorer.State) tableEvent {
	ev := tableEvent{Kind: kind, State: &st}
	if m != nil && m.Handover.Wanted() {
		ev.HomeID, ev.AwayID = m.Handover.HomeID, m.Handover.AwayID
	}
	return ev
}

func (s *Server) writeTableEvent(rc *http.ResponseController, w http.ResponseWriter, ev tableEvent) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshalling event: %w", err)
	}
	return s.writeRaw(rc, w, append(append([]byte("data: "), payload...), '\n', '\n'))
}
