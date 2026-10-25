package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/stuttgart-things/zaehlwerk/internal/checkin"
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
	// HomeID, AwayID and OperatorID are Schmetterpause ids, the handover the
	// scoring page sends from its lists. All three or none, and only behind
	// MATCHES_TOKEN (ADR-0015). Absent is a match reported nowhere, as before.
	HomeID     string `json:"home_id"`
	AwayID     string `json:"away_id"`
	OperatorID string `json:"operator_id"`
	// TournamentID and Round name a place in a Schmetterpause draw, both or
	// neither, and only next to the three ids above (ADR-0017). zaehlwerk
	// passes them on with the result and checks nothing about the place.
	TournamentID string `json:"tournament_id"`
	Round        int    `json:"round"`
	// Final marks the place as the tournament's final, for the panel's tags.
	// Only with tournament_id and round.
	Final bool `json:"final"`
}

// errHandoverOff is the answer to a body with an id while MATCHES_TOKEN or
// SCHMETTERPAUSE_URL is not configured. A 503 like check-in's: the route
// exists, taking a handover does not, and the start log says which is
// missing.
var errHandoverOff = errors.New("a match with player ids is off on this zaehlwerk")

// errHandoverToken is a body with an id without MATCHES_TOKEN, or with
// another.
var errHandoverToken = errors.New("a match with player ids needs the matches token")

// names reports whether the body names anybody by id.
func (req createRequest) names() bool {
	return req.HomeID != "" || req.AwayID != "" || req.OperatorID != "" ||
		req.TournamentID != "" || req.Round != 0 || req.Final
}

// handover checks the ids for their shape only. Whether they are players,
// and whether the operator plays, Schmetterpause decides when the result
// arrives, as for a phone claim (ADR-0011).
func (req createRequest) handover() (match.Handover, error) {
	if req.HomeID == "" || req.AwayID == "" || req.OperatorID == "" {
		return match.Handover{}, errors.New("home_id, away_id and operator_id go together")
	}
	var ids [3]string
	for i, f := range []struct{ name, id string }{
		{"home_id", req.HomeID}, {"away_id", req.AwayID}, {"operator_id", req.OperatorID},
	} {
		id, err := checkin.CanonicalUUID(f.id)
		if err != nil {
			return match.Handover{}, fmt.Errorf("%s: %w", f.name, err)
		}
		ids[i] = id
	}
	if ids[0] == ids[1] {
		return match.Handover{}, errors.New("home_id and away_id must differ")
	}
	h := match.Handover{HomeID: ids[0], AwayID: ids[1], OperatorID: ids[2]}

	if (req.TournamentID == "") != (req.Round == 0) {
		return match.Handover{}, errors.New("tournament_id and round go together")
	}
	if req.TournamentID != "" {
		id, err := checkin.CanonicalUUID(req.TournamentID)
		if err != nil {
			return match.Handover{}, fmt.Errorf("tournament_id: %w", err)
		}
		if req.Round < 1 {
			return match.Handover{}, errors.New("round counts from 1")
		}
		h.TournamentID, h.Round = id, req.Round
	}
	if req.Final && h.TournamentID == "" {
		return match.Handover{}, errors.New("final needs tournament_id and round")
	}
	h.Final = req.Final
	return h, nil
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

	// Without ids, the JSON API names players as free text and a match
	// started through it is reported nowhere; choosing from the real player
	// list is the page's (ADR-0004). With ids it is a handover, and a handover
	// names two players to Schmetterpause, so it is taken only from the one
	// caller holding MATCHES_TOKEN: the sandbox's Pong (ADR-0015). The token
	// is checked after the body is read, because only the body says whether
	// one is needed; the body is bounded by maxBodyBytes either way.
	setup := match.Setup{Demo: req.Demo}
	if req.names() {
		if s.handoverToken == "" {
			s.fail(w, r, http.StatusServiceUnavailable, errHandoverOff, nil)
			return
		}
		if !bearerIs(r, s.handoverToken) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="matches"`)
			s.fail(w, r, http.StatusUnauthorized, errHandoverToken, nil)
			return
		}
		h, err := req.handover()
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, err, nil)
			return
		}
		setup.Handover = h
	}

	m, err := s.start(cfg, setup)
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
