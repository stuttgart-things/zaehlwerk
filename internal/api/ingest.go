package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// ingestEvent is one point as it arrives on the wire.
//
// The field names are the ones the hardware already speaks: the ESP-NOW payload
// carries a source id, a player, a delta and an event counter
// (zaehlwerk-firmware ADR-0001), and the hub forwards what it received. Keeping
// one vocabulary across firmware, adapter and scorer is worth more than three
// differently spelled versions of the same four fields.
//
// Half is the alternative to Player for a source that only knows where it is
// mounted: a button under the left half sends "left", and the scorer resolves
// which player stands there, ends changed or not (ADR-0005). Exactly one of the
// two is set.
//
// Action "undo" takes the last point back instead of scoring, counted on the
// same source and event id as a point so that its retry is a duplicate
// (ADR-0012). It names no side, so Player and Half are not read for it.
type ingestEvent struct {
	Source  string `json:"source"`
	Player  string `json:"player"`
	Half    string `json:"half"`
	Delta   *int   `json:"delta"`
	EventID uint64 `json:"event_id"`
	Action  string `json:"action"`
}

// actionUndo is the one action an ingest event can name besides scoring.
const actionUndo = "undo"

func (e ingestEvent) undo() bool { return e.Action == actionUndo }

// batchRequest is POST /ingest/button. The hub queues presses while a request
// is in flight and drains several at once, so a burst during a rally arrives as
// one body.
type batchRequest struct {
	MatchID string        `json:"match_id"`
	Events  []ingestEvent `json:"events"`
}

// singleRequest is POST /ingest/piezo and POST /ingest/web. Same four fields,
// one event per request.
type singleRequest struct {
	MatchID string `json:"match_id"`
	ingestEvent
}

// scoreEvent normalises one wire event into the shape the scorer takes.
//
// deltaRequired is the piezo's quirk: a sensor knows which side of the table it
// sits under but not whether the hit was a point, so it says so explicitly. A
// missing delta defaulting to one would turn every unattributed knock into a
// point, and a duplicate point is not cosmetic — it changes who wins the set.
func (e ingestEvent) scoreEvent(matchID string, deltaRequired bool) (scorer.ScoreEvent, error) {
	ev := scorer.ScoreEvent{
		MatchID: matchID,
		Player:  scorer.Player(e.Player),
		Half:    scorer.Half(e.Half),
		EventID: e.EventID,
		Source:  e.Source,
	}

	switch {
	case e.Source == "":
		return ev, errors.New("source is required")
	case e.Action != "" && !e.undo():
		return ev, fmt.Errorf("action %q: the only action is %q", e.Action, actionUndo)
	case e.undo() && e.Delta != nil:
		return ev, errors.New("an undo takes no delta: it takes back the last point, whoever it went to")
	case e.undo():
		return ev, nil
	case e.Player != "" && e.Half != "":
		return ev, errors.New("player and half: name one of them, not both")
	case e.Half != "" && ev.Half != scorer.HalfLeft && ev.Half != scorer.HalfRight:
		return ev, fmt.Errorf("half %q: must be %q or %q", e.Half, scorer.HalfLeft, scorer.HalfRight)
	case e.Half == "" && ev.Player != scorer.PlayerA && ev.Player != scorer.PlayerB:
		return ev, fmt.Errorf("player %q: must be %q or %q", e.Player, scorer.PlayerA, scorer.PlayerB)
	case e.Delta == nil && deltaRequired:
		return ev, errors.New("delta is required: 1 for a point, 0 for a hit that is not one")
	case e.Delta != nil && *e.Delta != 0 && *e.Delta != 1:
		// The scorer clamps anything else at zero, which reads like a
		// correction and is not one: it does not reopen a set, and at 0 it
		// is lost without a word (#68).
		return ev, fmt.Errorf("delta %d: must be 1 for a point or 0 for a hit that is not one; "+
			"take a point back with \"action\": %q", *e.Delta, actionUndo)
	}

	ev.Delta = 1
	if e.Delta != nil {
		ev.Delta = *e.Delta
	}
	return ev, nil
}

// resolve finds the match an ingest request is for.
//
// Hardware may leave match_id out: a button wakes, sends and sleeps, and the
// ESP-NOW payload has no room and no source for a match id. Those sources are
// bound to the table rather than to a match, so the API resolves the running
// one. A browser knows the id of the match it created and must send it — a tab
// left open from yesterday should not score into today's match.
func (s *Server) resolve(w http.ResponseWriter, r *http.Request, matchID string, required bool) (*match.Match, bool) {
	if matchID == "" && required {
		s.fail(w, r, http.StatusBadRequest, errors.New("match_id is required"), nil)
		return nil, false
	}

	var (
		m   *match.Match
		err error
	)
	if matchID == "" {
		m, err = s.registry.Current()
	} else {
		m, err = s.registry.Get(matchID)
	}
	if err != nil {
		s.fail(w, r, http.StatusNotFound, err, nil)
		return nil, false
	}
	return m, true
}

// ingest validates the whole body before applying any of it, so a malformed
// event in a batch cannot leave half a burst on the scoreboard.
func (s *Server) ingest(w http.ResponseWriter, r *http.Request, m *match.Match, events []ingestEvent, deltaRequired bool) {
	if len(events) == 0 {
		s.fail(w, r, http.StatusBadRequest, errors.New("no events in the request"), nil)
		return
	}

	scoreEvents := make([]scorer.ScoreEvent, 0, len(events))
	hasUndo := false
	for i, e := range events {
		ev, err := e.scoreEvent(m.ID, deltaRequired)
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, fmt.Errorf("event %d: %w", i, err), nil)
			return
		}
		scoreEvents = append(scoreEvents, ev)
		hasUndo = hasUndo || e.undo()
	}

	// A won match still takes an undo: taking back the match point is the
	// mis-press it exists for, and POST /matches/{id}/undo takes it too
	// (ADR-0007, ADR-0012). A match ended by hand takes nothing.
	_, ended := m.EndedAt()
	if ended || (!m.Running() && !hasUndo) {
		st := m.Scorer.State()
		s.fail(w, r, http.StatusConflict, errors.New("the match is over"), &st)
		return
	}

	st := m.Scorer.State()
	for i, ev := range scoreEvents {
		var (
			res scorer.Result
			err error
		)
		if events[i].undo() {
			res, err = m.Scorer.UndoEvent(ev)
		} else {
			res, err = m.Scorer.Apply(ev)
		}
		st = res.State

		if errors.Is(err, scorer.ErrMatchComplete) {
			// The match was won before this press: earlier in this burst, or
			// before the request. It has nowhere to go, and the request may
			// still do something, so this is not a failure. The next event is
			// still tried, because it may be the undo of the match point.
			s.log.Info("event dropped, the match is won",
				"match_id", m.ID, "source", ev.Source, "event_id", ev.EventID)
			continue
		}
		if err != nil {
			// Everything was validated above and the match was running, so
			// nothing should reach here. If it does it is our bug, not the
			// sender's, and it must not read as a rejected press.
			s.fail(w, r, http.StatusInternalServerError, err, &st)
			return
		}

		s.log.Info("event ingested", "match_id", m.ID, "source", ev.Source,
			"event_id", ev.EventID, "undo", events[i].undo(), "outcome", res.Outcome.String(),
			"points", st.Points, "sets", st.Sets, "serving", st.Serving, "phase", st.Phase)
	}

	s.writeState(w, http.StatusOK, st)
}

func (s *Server) ingestButton(w http.ResponseWriter, r *http.Request) {
	var req batchRequest
	if err := decode(w, r, &req); err != nil {
		s.fail(w, r, http.StatusBadRequest, fmt.Errorf("decoding the body: %w", err), nil)
		return
	}

	m, ok := s.resolve(w, r, req.MatchID, false)
	if !ok {
		return
	}
	s.ingest(w, r, m, req.Events, false)
}

func (s *Server) ingestPiezo(w http.ResponseWriter, r *http.Request) {
	s.ingestSingle(w, r, false, true)
}

func (s *Server) ingestWeb(w http.ResponseWriter, r *http.Request) {
	s.ingestSingle(w, r, true, false)
}

func (s *Server) ingestSingle(w http.ResponseWriter, r *http.Request, matchIDRequired, deltaRequired bool) {
	var req singleRequest
	if err := decode(w, r, &req); err != nil {
		s.fail(w, r, http.StatusBadRequest, fmt.Errorf("decoding the body: %w", err), nil)
		return
	}

	m, ok := s.resolve(w, r, req.MatchID, matchIDRequired)
	if !ok {
		return
	}
	s.ingest(w, r, m, []ingestEvent{req.ingestEvent}, deltaRequired)
}
