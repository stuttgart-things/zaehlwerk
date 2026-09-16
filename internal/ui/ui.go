// Package ui serves the browser scoring mock: an htmx page that scores the
// same match the buttons under the table score.
//
// It is a fourth adapter next to the three in package api. A phone was always
// one of the point sources (README, ADR-0002); until now it needed curl or
// `task demo` to be one, and neither of those shows a scoreboard. This is the
// same match, through the same scorer, with the score on the screen and the
// panel title next to it — no hardware, no terminal.
//
// The routes here are HTML, not JSON, and they exist rather than the page
// calling POST /ingest/web because htmx posts form-encoded and swaps HTML
// partials back. That is the split homerun2-led-catcher makes between its
// /streams API and its /ui/streams control, and it keeps the JSON contract of
// ADR-0002 exactly as it is: the machine callers are unaffected by anything the
// page needs.
//
// Points and undo go straight to the scorer, the way ingest does. Starting and
// ending a match go through [Lifecycle] instead, because that is where the LED
// panel changes hands (ADR-0003) and the policy for it belongs in one place.
package ui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/live"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// DefaultHeartbeat is how often an idle UI stream writes a keepalive comment.
// Same reasoning, and same default, as the JSON stream's.
const DefaultHeartbeat = 25 * time.Second

// maxFormBytes caps a posted form. The largest thing sent here is two player
// names, and this is an open endpoint on the office network.
const maxFormBytes = 16 << 10

// Lifecycle starts and ends matches. [api.Server] implements it.
//
// Creating a match hands the LED panel to it and ending one gives the panel
// back, and that is a decision with a whole ADR behind it (0003). Rather than
// repeat it here, the UI drives the same code the JSON API does. The interface
// is declared on this side so that package api stays unaware that a UI exists.
type Lifecycle interface {
	StartMatch(cfg scorer.Config, handover match.Handover) (*match.Match, error)
	EndMatch(m *match.Match) scorer.State
}

// Roster is where the page gets the players it may name.
//
// An interface rather than the client itself, so this package does not depend
// on the coupling: nil means the coupling is off and the form asks for two
// free-text names, which is what ADR-0004 keeps as a first-class case.
type Roster interface {
	Players(ctx context.Context) ([]schmetterpause.Player, error)
}

// Handover posts a finished result, for the retry button.
type Handover interface {
	Send(ctx context.Context, m *match.Match) error
}

// Server serves the scoring page and the partials it swaps. Use [New].
type Server struct {
	registry  *match.Registry
	lifecycle Lifecycle
	// roster and handover are nil when SCHMETTERPAUSE_URL is unset. Every
	// place they are used checks, because "off" is a supported way to run
	// (invariant 4) rather than a degraded one.
	roster   Roster
	handover Handover
	log      *slog.Logger
	now      func() time.Time
	mux      *http.ServeMux

	// hub is nil when live streaming is not configured. The page then renders
	// and scores exactly as it does otherwise, minus the SSE connection: every
	// swap is the answer to the click that caused it, so a browser sees its own
	// points and nobody else's.
	hub       *live.Hub
	heartbeat time.Duration
}

// Option configures a Server.
type Option func(*Server)

// WithLogger replaces the logger.
func WithLogger(l *slog.Logger) Option {
	return func(s *Server) { s.log = l }
}

// WithClock replaces the clock, for tests. It stamps the timeline.
func WithClock(now func() time.Time) Option {
	return func(s *Server) { s.now = now }
}

// WithHub enables the live updates, so a page shows points that arrive from a
// button, from `task demo` or from somebody else's browser.
func WithHub(h *live.Hub) Option {
	return func(s *Server) { s.hub = h }
}

// WithHeartbeat sets how often an idle stream writes a keepalive comment.
func WithHeartbeat(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.heartbeat = d
		}
	}
}

// WithSchmetterpause turns the handover on: the form offers the players that
// application knows, and a finished match is reported to it. Both nil leaves
// the page exactly as it is without the coupling.
func WithSchmetterpause(roster Roster, handover Handover) Option {
	return func(s *Server) {
		s.roster = roster
		s.handover = handover
	}
}

func New(registry *match.Registry, lifecycle Lifecycle, opts ...Option) *Server {
	s := &Server{
		registry:  registry,
		lifecycle: lifecycle,
		log:       slog.Default(),
		now:       time.Now,
		mux:       http.NewServeMux(),
		heartbeat: DefaultHeartbeat,
	}
	for _, opt := range opts {
		opt(s)
	}

	// Both spellings of the page: /ui is what a person types, /ui/ is what a
	// link ends up as.
	s.mux.HandleFunc("GET /ui", s.index)
	s.mux.HandleFunc("GET /ui/{$}", s.index)

	s.mux.HandleFunc("POST /ui/matches", s.createMatch)
	s.mux.HandleFunc("POST /ui/matches/{id}/point", s.point)
	s.mux.HandleFunc("POST /ui/matches/{id}/undo", s.undo)
	s.mux.HandleFunc("POST /ui/matches/{id}/end", s.endMatch)
	s.mux.HandleFunc("POST /ui/matches/{id}/report", s.report)
	s.mux.HandleFunc("GET /ui/matches/{id}/stream", s.stream)

	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// index renders the whole page.
//
// Without ?match it shows the match a point would land on right now — the one
// the buttons under the table are scoring — so opening the page during a match
// joins it rather than asking which one. With ?match it shows that one, running
// or not, which is what makes the id printed by `task demo` and by
// POST /matches worth printing.
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	v := view{}

	if id := r.URL.Query().Get("match"); id != "" {
		m, err := s.registry.Get(id)
		if err != nil {
			v.Error = err.Error()
		} else {
			v.Match = s.matchView(m, kindOf(m.Scorer.State()))
		}
	} else if m, err := s.registry.Current(); err == nil {
		v.Match = s.matchView(m, kindOf(m.Scorer.State()))
	}

	v.Stream = v.Match != nil && s.hub != nil
	s.fillRoster(r.Context(), &v)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page carries its own script and the SSE wiring, so a tab left open
	// across a deploy would otherwise keep swapping into markup the release no
	// longer serves.
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "page", v)
}

// createMatch starts a match from the form at the bottom of the page and
// answers with the whole live panel, so the browser reconnects its stream to
// the new match.
func (s *Server) createMatch(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		s.renderLive(w, view{Error: err.Error()})
		return
	}

	cfg, err := config(r.PostForm)
	if err != nil {
		s.renderLive(w, view{Error: err.Error()})
		return
	}

	handover, err := s.handoverFrom(r.PostForm)
	if err != nil {
		s.renderLive(w, view{Error: err.Error()})
		return
	}

	// With the coupling on, the names on the panel are the ones Schmetterpause
	// holds. Typing a third spelling of somebody's name onto the matrix while
	// the result goes to their real account is the kind of small confusion
	// that makes a scoreboard untrustworthy.
	if handover.Wanted() {
		if names, err := s.namesFor(r.Context(), handover); err == nil {
			cfg.Players = names
		}
	}

	m, err := s.lifecycle.StartMatch(cfg, handover)
	if err != nil {
		s.renderLive(w, view{Error: err.Error()})
		return
	}

	s.renderLive(w, s.liveView(m, kindOf(m.Scorer.State())))
}

// report retries a handover that did not get through.
//
// ADR-0004 keeps the match in the registry until retention drops it precisely
// so this button has something to press. After that it is a hand entry like
// any other, and the page says so.
func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	m, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if s.handover == nil {
		s.renderLive(w, view{Error: "this instance does not report to schmetterpause"})
		return
	}

	if err := s.handover.Send(r.Context(), m); err != nil {
		// The failure is already recorded on the match, so the live view shows
		// it; nothing extra to say here that the card will not.
		s.log.Warn("retrying the handover failed", "match_id", m.ID, "error", err)
	}
	s.renderLive(w, s.liveView(m, kindOf(m.Scorer.State())))
}

// fillRoster asks Schmetterpause for the players, every time the page renders.
//
// Not cached, per ADR-0004: this service holds no copy of anybody, so a player
// who joined a minute ago is offered and one who was merged away is not.
func (s *Server) fillRoster(ctx context.Context, v *view) {
	if s.roster == nil {
		return
	}

	players, err := s.roster.Players(ctx)
	if err != nil {
		s.log.Warn("fetching the player list failed", "error", err)
		v.RosterErr = err.Error()
		return
	}

	v.Roster = make([]RosterPlayer, 0, len(players))
	for _, p := range players {
		v.Roster = append(v.Roster, RosterPlayer{ID: p.ID, Name: p.DisplayName})
	}
}

// handoverFrom reads the three ids the form names, and refuses the combinations
// Schmetterpause would refuse anyway.
//
// Checked here rather than left to the post at match end, because the post is
// twenty minutes later: a match started with the scorekeeper as one of the
// players would be scored to the last point and only then turn out to be
// unreportable.
func (s *Server) handoverFrom(form url.Values) (match.Handover, error) {
	h := match.Handover{
		HomeID:     strings.TrimSpace(form.Get("home_id")),
		AwayID:     strings.TrimSpace(form.Get("away_id")),
		OperatorID: strings.TrimSpace(form.Get("operator_id")),
	}

	// None named is a match nobody reports, which is allowed and ordinary.
	if h.HomeID == "" && h.AwayID == "" && h.OperatorID == "" {
		return match.Handover{}, nil
	}
	if !h.Wanted() {
		return match.Handover{}, errors.New("name both players and whoever is keeping score, or none of the three")
	}
	if h.HomeID == h.AwayID {
		return match.Handover{}, errors.New("two different players, please")
	}
	if h.OperatorID == h.HomeID || h.OperatorID == h.AwayID {
		return match.Handover{}, errors.New("whoever keeps score may not be playing")
	}
	return h, nil
}

// namesFor resolves the chosen ids to display names for the panel.
func (s *Server) namesFor(ctx context.Context, h match.Handover) ([2]string, error) {
	if s.roster == nil {
		return [2]string{}, errors.New("no roster")
	}
	players, err := s.roster.Players(ctx)
	if err != nil {
		return [2]string{}, err
	}

	byID := make(map[string]string, len(players))
	for _, p := range players {
		byID[p.ID] = p.DisplayName
	}
	home, okHome := byID[h.HomeID]
	away, okAway := byID[h.AwayID]
	if !okHome || !okAway {
		return [2]string{}, errors.New("a chosen player is not in the list any more")
	}
	return [2]string{home, away}, nil
}

// point applies one point, with the same four fields the firmware sends.
//
// The page counts for itself: a source drawn per page load and an id that goes
// up by one per click, exactly what a phone does in ADR-0002. So the mock
// exercises the deduplication rather than working around it — a double-posted
// click is discarded here for the same reason a retried button press is.
func (s *Server) point(w http.ResponseWriter, r *http.Request) {
	m, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if err := parseForm(w, r); err != nil {
		s.renderBoard(w, s.errorView(m, err))
		return
	}

	ev, err := scoreEvent(m.ID, r.PostForm)
	if err != nil {
		s.renderBoard(w, s.errorView(m, err))
		return
	}

	if !m.Running() {
		s.renderBoard(w, s.errorView(m, errMatchOver))
		return
	}

	before := m.Scorer.State()
	res, err := m.Scorer.Apply(ev)
	if err != nil {
		s.renderBoard(w, s.errorView(m, err))
		return
	}

	s.log.Info("event ingested", "match_id", m.ID, "source", ev.Source,
		"event_id", ev.EventID, "outcome", res.Outcome.String(),
		"points", res.State.Points, "sets", res.State.Sets, "serving", res.State.Serving)

	s.renderBoard(w, s.liveView(m, kindBetween(before, res.State)))
}

func (s *Server) undo(w http.ResponseWriter, r *http.Request) {
	m, ok := s.lookup(w, r)
	if !ok {
		return
	}

	st, err := m.Scorer.Undo()
	if err != nil {
		s.renderBoard(w, s.errorView(m, err))
		return
	}

	s.log.Info("point taken back", "match_id", m.ID,
		"points", st.Points, "sets", st.Sets, "set", st.SetNumber)
	s.renderBoard(w, s.liveView(m, scorer.TransitionUndo))
}

func (s *Server) endMatch(w http.ResponseWriter, r *http.Request) {
	m, ok := s.lookup(w, r)
	if !ok {
		return
	}

	st := s.lifecycle.EndMatch(m)
	s.renderBoard(w, s.liveView(m, kindOf(st)))
}

// lookup resolves the match in the path, answering with an empty board that
// says why when there is none.
func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (*match.Match, bool) {
	m, err := s.registry.Get(r.PathValue("id"))
	if err != nil {
		s.renderBoard(w, view{Error: err.Error()})
		return nil, false
	}
	return m, true
}

var errMatchOver = errors.New("the match is over")

// parseForm reads a form body, capping how much of it is read.
func parseForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		return fmt.Errorf("reading the form: %w", err)
	}
	return nil
}

// config turns the new-match form into a scorer config. Every field is
// optional, as it is on POST /matches.
func config(form url.Values) (scorer.Config, error) {
	get := func(key string) string { return strings.TrimSpace(form.Get(key)) }

	cfg := scorer.Config{FirstServer: scorer.Player(get("first_server"))}
	if a, b := get("player_a"), get("player_b"); a != "" || b != "" {
		cfg.Players = [2]string{a, b}
		if a == "" || b == "" {
			return cfg, fmt.Errorf("players: name both of them, or neither")
		}
	}

	if raw := get("best_of"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return cfg, fmt.Errorf("best_of %q: not a number", raw)
		}
		cfg.BestOf = n
	}
	return cfg, nil
}

// scoreEvent builds the event one click stands for. Delta is always 1: a button
// on a page is a point, and the piezo's "a hit that was not a point" has no
// meaning here.
func scoreEvent(matchID string, form url.Values) (scorer.ScoreEvent, error) {
	ev := scorer.ScoreEvent{
		MatchID: matchID,
		Player:  scorer.Player(form.Get("player")),
		Delta:   1,
		Source:  form.Get("source"),
	}

	if ev.Source == "" {
		return ev, fmt.Errorf("source is required")
	}
	raw := form.Get("event_id")
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return ev, fmt.Errorf("event_id %q: not a counter", raw)
	}
	ev.EventID = id
	return ev, nil
}
