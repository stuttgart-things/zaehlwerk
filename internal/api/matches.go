package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/stuttgart-things/zaehlwerk/internal/live"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// createRequest is the body of POST /matches. Every field is optional; the
// defaults are a best of five to eleven with a serving first, no warm-up and no
// change of ends — the match as it was before phases existed (ADR-0005).
type createRequest struct {
	Players      []string `json:"players"`
	BestOf       int      `json:"best_of"`
	PointsPerSet int      `json:"points_per_set"`
	// FirstServer is "a", "b", or "rally" to play for it.
	FirstServer string `json:"first_server"`
	// Start is empty, "serve_rally" or "warmup".
	Start      string `json:"start"`
	ChangeEnds bool   `json:"change_ends"`
	// SetOpener is "alternate" or "loser". Empty takes the server's
	// SET_OPENER.
	SetOpener string `json:"set_opener"`
	// Demo marks a match that is no result and gives way to one, as
	// zaehlwerk-pong's are (ADR-0008). Absent is a real match, which is what
	// every caller that never heard of the field keeps getting.
	Demo bool `json:"demo"`
}

// firstServerRally is the first_server value that asks for a serve rally,
// so a caller that only knows the one field can still ask for one.
const firstServerRally = "rally"

func (req createRequest) config() (scorer.Config, error) {
	cfg := scorer.Config{
		BestOf:       req.BestOf,
		PointsPerSet: req.PointsPerSet,
		FirstServer:  scorer.Player(req.FirstServer),
		Start:        scorer.Start(req.Start),
		ChangeEnds:   req.ChangeEnds,
		SetOpener:    scorer.SetOpener(req.SetOpener),
	}
	if req.FirstServer == firstServerRally {
		cfg.FirstServer = ""
		if cfg.Start == scorer.StartPlaying {
			cfg.Start = scorer.StartServeRally
		}
	}

	switch len(req.Players) {
	case 0:
	case 2:
		cfg.Players = [2]string{req.Players[0], req.Players[1]}
	default:
		return cfg, fmt.Errorf("players: need two names, got %d", len(req.Players))
	}

	return cfg, nil
}

func (s *Server) createMatch(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if r.ContentLength != 0 {
		if err := decode(w, r, &req); err != nil {
			s.fail(w, r, http.StatusBadRequest, fmt.Errorf("decoding the body: %w", err), nil)
			return
		}
	}

	cfg, err := req.config()
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, err, nil)
		return
	}

	// The JSON API names players as free text, so a match started through it
	// is reported nowhere. Choosing from the real player list is something the
	// page does, because it is the only caller that can show a list to pick
	// from (ADR-0004).
	m, err := s.start(cfg, match.Setup{Demo: req.Demo})
	var taken *match.TableTakenError
	switch {
	case errors.As(err, &taken):
		// The demo waits; the answer says for what, the way an ingest into a
		// match that is over answers with where it stands.
		st := taken.Running.Scorer.State()
		s.fail(w, r, http.StatusConflict, err, &st)
		return
	case errors.Is(err, match.ErrIDGeneration):
		s.fail(w, r, http.StatusInternalServerError, err, nil)
		return
	case err != nil:
		// The scorer rejects an even best-of or an unknown first server, and
		// the registry a demo with ids: the caller's doing. A failed id draw,
		// above, is ours.
		s.fail(w, r, http.StatusBadRequest, err, nil)
		return
	}

	w.Header().Set("Location", "/matches/"+m.ID)
	s.writeState(w, http.StatusCreated, m.Scorer.State())
}

func (s *Server) getMatch(w http.ResponseWriter, r *http.Request) {
	m, ok := s.lookup(w, r)
	if !ok {
		return
	}
	s.writeState(w, http.StatusOK, m.Scorer.State())
}

// currentMatch is how a board learns which match it is scoring into before it
// scores anything.
//
// A piezo joins the match the page created rather than creating one
// (zaehlwerk-firmware ADR-0006), and it needs the names to label its halves and
// who serves to judge a rally. Hardware ingest already resolves the match
// through the same Registry.Current, so what this answers is the match the
// board's next point will land in — asking by any other rule would let the two
// disagree. Player ids are not in the state and stay on the match (ADR-0004).
func (s *Server) currentMatch(w http.ResponseWriter, r *http.Request) {
	m, err := s.registry.Current()
	if err != nil {
		s.fail(w, r, http.StatusNotFound, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, currentBody{State: m.Scorer.State(), Demo: m.Demo})
}

// currentBody is the state with whether the match is a demo, flat beside it
// and absent for a real match, so a caller that never heard of the field reads
// exactly what it read before. A board on the table has no use for it: a
// person hitting the ball into a demo is somebody playing, and that ending the
// demo is the point (ADR-0008). A simulated board does: it only rolls rallies,
// and rolled into a demo they are nothing but noise in the bots' game.
type currentBody struct {
	scorer.State
	Demo bool `json:"demo,omitempty"`
}

// undo is not gated on the match still running. Taking back a wrongly awarded
// match point is exactly when it is needed.
//
// It is not safe to retry: it carries no source and no event id, so a second
// request after a lost answer takes back a second point. It is for the page
// and for people; a device that retries sends "action": "undo" through ingest
// instead (ADR-0012).
func (s *Server) undo(w http.ResponseWriter, r *http.Request) {
	m, ok := s.lookup(w, r)
	if !ok {
		return
	}

	st, err := m.Scorer.Undo()
	if err != nil {
		// ErrNothingToUndo is the only thing Undo returns.
		s.fail(w, r, http.StatusConflict, err, &st)
		return
	}

	s.log.Info("point taken back", "match_id", m.ID,
		"points", st.Points, "sets", st.Sets, "set", st.SetNumber)
	s.writeState(w, http.StatusOK, st)
}

// endMatch is the JSON API's door to [Server.EndMatch].
func (s *Server) endMatch(w http.ResponseWriter, r *http.Request) {
	m, ok := s.lookup(w, r)
	if !ok {
		return
	}
	s.writeState(w, http.StatusOK, s.EndMatch(m))
}

// StartMatch creates a match and hands it the LED panel.
//
// The panel is given on creation rather than on the first point, so the catcher
// has switched over before that point is pitched. A match created and never
// played is picked up by the switcher's inactivity failsafe.
//
// Exported because the browser UI starts matches too (internal/ui), and where
// the panel changes hands is ADR-0003's decision rather than a detail of one
// handler — one caller of the switcher, not two.
func (s *Server) StartMatch(cfg scorer.Config, handover match.Handover) (*match.Match, error) {
	return s.start(cfg, match.Setup{Handover: handover})
}

// start is StartMatch with the whole setup. Only the JSON API starts demos,
// so only it needs more than the handover.
//
// A real match started here, on the page or through the API, releases every
// check-in claim: the table is that match's, and a lone claim made before it
// would otherwise wait it out and pair with whoever checks in after it (#114).
// The desk is told once the registry's lock is free again, so the two locks
// are never taken in the order the desk takes them in, desk then registry
// (#102). A demo leaves the claims: a checked-in match takes the table from
// it (ADR-0008).
func (s *Server) start(cfg scorer.Config, setup match.Setup) (*match.Match, error) {
	m, err := s.create(cfg, setup)
	if err != nil {
		return nil, err
	}
	if s.desk != nil && !m.Demo {
		s.desk.Drop(m.ID)
	}
	return m, nil
}

// create makes the match and hands it the panel. The desk starts its matches
// through this rather than start: it holds its own lock while it does, and
// ends its claims itself.
func (s *Server) create(cfg scorer.Config, setup match.Setup) (*match.Match, error) {
	if cfg.SetOpener == "" {
		cfg.SetOpener = s.setOpener
	}
	m, ended, err := s.registry.CreateFor(cfg, setup)
	if err != nil {
		return nil, err
	}

	if s.switcher != nil {
		s.switcher.Start(m.ID)
	}

	// The demos a real match ended inside the registry (ADR-0008), with what
	// EndMatch does around an end, now that the registry lock is free. The
	// panel went to the new match first: a release after that finds it held
	// by another match and leaves it, so the idle streams never flash up in
	// between (ADR-0003).
	for _, d := range ended {
		if s.hub != nil {
			s.hub.Announce(live.Event{Kind: live.KindEnded, State: d.Scorer.State()})
		}
		if s.switcher != nil {
			s.switcher.Release(d.ID)
		}
		s.log.Info("demo ended, a real match took the table", "match_id", d.ID, "by", m.ID)
	}

	if s.hub != nil {
		s.hub.Announce(live.Event{Kind: live.KindStarted, State: m.Scorer.State()})
	}
	s.log.Info("match created", "match_id", m.ID, "demo", m.Demo)
	return m, nil
}

// EndMatch finishes a match that will not finish itself — abandoned, or the
// scorekeeper walked away — and gives the panel back, returning the state it
// ended in. It is idempotent, and it is the other half of what the browser UI
// shares with the JSON API; see [Server.StartMatch].
func (s *Server) EndMatch(m *match.Match) scorer.State {
	st := m.Scorer.State()
	endedAt := m.End(s.now())

	if s.switcher != nil {
		// A match that is played out to its final point releases the panel from
		// the scorer observer instead; this is the path for one that is ended
		// early, and releasing twice is harmless.
		s.switcher.Release(m.ID)
	}

	if s.hub != nil {
		s.hub.Announce(live.Event{Kind: live.KindEnded, State: m.Scorer.State()})
	}
	s.log.Info("match ended", "match_id", m.ID, "ended_at", endedAt,
		"complete", st.Complete, "sets", st.Sets)
	return st
}
