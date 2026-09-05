package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// createRequest is the body of POST /matches. Every field is optional; the
// defaults are a best of five to eleven with a serving first.
type createRequest struct {
	Players      []string `json:"players"`
	BestOf       int      `json:"best_of"`
	PointsPerSet int      `json:"points_per_set"`
	FirstServer  string   `json:"first_server"`
}

func (req createRequest) config() (scorer.Config, error) {
	cfg := scorer.Config{
		BestOf:       req.BestOf,
		PointsPerSet: req.PointsPerSet,
		FirstServer:  scorer.Player(req.FirstServer),
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

	m, err := s.registry.Create(cfg)
	if err != nil {
		// The scorer rejects an even best-of or an unknown first server, which
		// is the caller's doing. A failed id draw is ours.
		status := http.StatusBadRequest
		if errors.Is(err, match.ErrIDGeneration) {
			status = http.StatusInternalServerError
		}
		s.fail(w, r, status, err, nil)
		return
	}

	s.log.Info("match created", "match_id", m.ID)
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

// undo is not gated on the match still running. Taking back a wrongly awarded
// match point is exactly when it is needed.
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

// endMatch finishes a match that will not finish itself — abandoned, or the
// scorekeeper walked away. It is idempotent.
func (s *Server) endMatch(w http.ResponseWriter, r *http.Request) {
	m, ok := s.lookup(w, r)
	if !ok {
		return
	}

	st := m.Scorer.State()
	endedAt := m.End(s.now())

	s.log.Info("match ended", "match_id", m.ID, "ended_at", endedAt,
		"complete", st.Complete, "sets", st.Sets)
	s.writeState(w, http.StatusOK, st)
}
