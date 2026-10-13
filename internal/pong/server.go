package pong

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// maxRetainedGames bounds what the service remembers. Games are in memory and
// gone on restart, like zaehlwerk's matches, and a finished one is forgotten
// once this many newer ones exist.
const maxRetainedGames = 20

// DefaultPace is the pause between two reports when POST /games does not name
// one: slow enough to follow on the panel.
const DefaultPace = time.Second

// Config is the service. cmd/zaehlwerk-pong fills it from the environment and
// is the only place that reads it.
type Config struct {
	ZaehlwerkURL string
	BestOf       int
	Skills       [2]Skill
	// Seed makes a run repeatable: the n-th game of a service started with
	// the same seed draws the same rallies.
	Seed int64
	// Faults is what a game injects unless POST /games names its own.
	Faults Faults
	Build  BuildInfo
}

// BuildInfo is what the binary says about itself on /healthz.
type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

// Server runs headless games against one zaehlwerk.
type Server struct {
	cfg    Config
	client *Client
	log    *slog.Logger
	mux    *http.ServeMux
	// ctx is the service's lifetime. A game outlives the request that started
	// it, and ends when the service does.
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	games   map[string]*Game
	order   []string
	running *Game
	played  int64
	wg      sync.WaitGroup

	// resetQuiet is the gap before a counter reset, zero for the real one.
	// Only the tests set it.
	resetQuiet time.Duration
}

func New(cfg Config, log *slog.Logger) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		cfg:    cfg,
		client: NewClient(cfg.ZaehlwerkURL, log),
		log:    log,
		mux:    http.NewServeMux(),
		ctx:    ctx,
		cancel: cancel,
		games:  make(map[string]*Game),
	}
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("POST /games", s.startGame)
	s.mux.HandleFunc("GET /games/{id}", s.getGame)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Close stops a running game and waits until its match is ended on zaehlwerk,
// or ctx gives up first. A game left running would stay the table's current
// match with nobody scoring it.
func (s *Server) Close(ctx context.Context) error {
	s.cancel()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for the running game to end its match: %w", ctx.Err())
	}
}

// healthz says the process is up, and nothing about zaehlwerk: a zaehlwerk that
// is down is a game that cannot start, not a pod to restart.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
		BuildInfo
	}{Status: "ok", BuildInfo: s.cfg.Build})
}

// startBody is POST /games. Everything else about a game is the service's
// configuration.
type startBody struct {
	// Pace is a Go duration, "0s" for as fast as zaehlwerk answers.
	Pace string `json:"pace"`
	// Faults replaces PONG_FAULTS for this game, as a whole: a share left
	// out is zero, not the configured one. Absent keeps the configured
	// faults.
	Faults *Faults `json:"faults"`
}

// errBusy is a game or a match already holding the table.
var errBusy = errors.New("busy")

// startGame starts a headless bot match and answers once its match exists, so
// a refusal — the table taken, zaehlwerk unreachable — is this request's
// answer rather than a game that failed a moment later.
func (s *Server) startGame(w http.ResponseWriter, r *http.Request) {
	var body startBody
	if r.ContentLength != 0 {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			fail(w, http.StatusBadRequest, fmt.Errorf("decoding the body: %w", err))
			return
		}
	}
	pace := DefaultPace
	if body.Pace != "" {
		p, err := time.ParseDuration(body.Pace)
		if err != nil || p < 0 {
			fail(w, http.StatusBadRequest, fmt.Errorf("pace %q: want a duration such as 500ms or 0s", body.Pace))
			return
		}
		pace = p
	}
	faults := s.cfg.Faults
	if body.Faults != nil {
		// Unknown names are already refused by the decoder.
		if err := body.Faults.Validate(); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		faults = *body.Faults
	}

	g, err := s.start(pace, faults)
	switch {
	case errors.Is(err, errBusy):
		fail(w, http.StatusConflict, err)
		return
	case err != nil:
		fail(w, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Location", "/games/"+g.ID)
	writeJSON(w, http.StatusCreated, g.View())
}

func (s *Server) start(pace time.Duration, faults Faults) (*Game, error) {
	// One game at a time. Exactly one instance reports for a game (ADR-0006),
	// and a second game would be a second match taking the table from the
	// first.
	s.mu.Lock()
	if s.running != nil {
		id := s.running.ID
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: game %s is still playing", errBusy, id)
	}
	// Counted only once a game has started, so a refused start does not
	// shift the seed of every game after it.
	n := s.played
	id, err := newID()
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	g := newGame(id, GameConfig{
		BestOf:     s.cfg.BestOf,
		Skills:     s.cfg.Skills,
		Pace:       pace,
		Seed:       s.cfg.Seed + n,
		Faults:     faults,
		ResetQuiet: s.resetQuiet,
	}, s.client, s.log)
	s.running = g
	s.mu.Unlock()

	g, err = s.launch(g)
	if err != nil {
		s.mu.Lock()
		s.running = nil
		s.mu.Unlock()
		return nil, err
	}
	return g, nil
}

// launch looks at the table, starts the match if it is free, and sets the game
// playing.
func (s *Server) launch(g *Game) (*Game, error) {
	ctx, cancel := context.WithCancel(s.ctx)

	// Connected before the match is created, so the game's own snapshot and
	// everything after it is on the stream rather than missed in between.
	table, err := s.client.Follow(ctx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("reading the table from zaehlwerk: %w", err)
	}

	// The first event is the table as it is now. A current match is
	// somebody's: a reported one is a real match between real players, and
	// an unreported one is still a person scoring on the page, whose buttons
	// and page would land in the game's match the moment it became current.
	var first TableEvent
	select {
	case ev, ok := <-table:
		if !ok {
			cancel()
			return nil, errors.New("reading the table from zaehlwerk: the stream closed at once")
		}
		first = ev
	case <-time.After(5 * time.Second):
		cancel()
		return nil, errors.New("reading the table from zaehlwerk: no answer on /live/stream")
	}
	if first.Kind != kindNoMatch {
		cancel()
		if first.State == nil {
			return nil, fmt.Errorf("%w: the table says %q", errBusy, first.Kind)
		}
		if first.Reported() {
			return nil, fmt.Errorf("%w: match %s is being reported to Schmetterpause, and a game never joins one",
				errBusy, first.State.MatchID)
		}
		return nil, fmt.Errorf("%w: match %s is running at the table", errBusy, first.State.MatchID)
	}

	if err := g.start(ctx); err != nil {
		cancel()
		return nil, err
	}

	s.mu.Lock()
	s.played++
	s.games[g.ID] = g
	s.order = append(s.order, g.ID)
	s.forget()
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		g.run(ctx, table)
		s.mu.Lock()
		if s.running == g {
			s.running = nil
		}
		s.mu.Unlock()
	}()
	return g, nil
}

// forget drops the oldest finished games beyond the bound. Called with s.mu
// held.
func (s *Server) forget() {
	for len(s.order) > maxRetainedGames {
		oldest := s.order[0]
		if s.games[oldest] == s.running {
			return
		}
		delete(s.games, oldest)
		s.order = s.order[1:]
	}
}

func (s *Server) getGame(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	g, ok := s.games[r.PathValue("id")]
	s.mu.Unlock()
	if !ok {
		fail(w, http.StatusNotFound, fmt.Errorf("no game %q", r.PathValue("id")))
		return
	}
	writeJSON(w, http.StatusOK, g.View())
}

func newID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("drawing a game id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func fail(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{err.Error()})
}
