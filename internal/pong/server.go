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
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
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
	// AttractAfter is how long the table must be free before a demo starts
	// by itself; zero is no attract mode. AttractPace is the demo's pause
	// between two reports.
	AttractAfter time.Duration
	AttractPace  time.Duration
	// BrowserIdle ends a browser game nobody presses in; zero is ten
	// minutes.
	BrowserIdle time.Duration
	// CheckTimeout is how long a button check's prompt waits for its
	// press; zero is DefaultCheckTimeout.
	CheckTimeout time.Duration
	Build        BuildInfo
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

	// starting is held for the whole of a start, from looking at what runs
	// to the new game running. It is what keeps a demo and a game somebody
	// asked for from starting in the same moment: the demo starts under it,
	// and a requested game ends the demo under it.
	starting sync.Mutex

	mu      sync.Mutex
	games   map[string]*Game
	order   []string
	running *Game
	played  int64
	wg      sync.WaitGroup
	attract attractState

	// resetQuiet is the gap before a counter reset, zero for the real one,
	// and only the tests set it. idle is how long a browser game waits for a
	// press, BrowserIdle unless a test sets it.
	resetQuiet time.Duration
	idle       time.Duration
	// checkGap is the pause between a button check's prompts, zero for
	// the real one; only the tests set it.
	checkGap time.Duration
	// heartbeat is how often an idle stream to a page writes a keepalive.
	heartbeat time.Duration
	// now is the clock a join code expires by; only the tests set it.
	now func() time.Time
}

func New(cfg Config, log *slog.Logger) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		cfg:       cfg,
		client:    NewClient(cfg.ZaehlwerkURL, log),
		log:       log,
		mux:       http.NewServeMux(),
		ctx:       ctx,
		cancel:    cancel,
		games:     make(map[string]*Game),
		heartbeat: DefaultHeartbeat,
		idle:      cfg.BrowserIdle,
		now:       time.Now,
	}
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("POST /games", s.startGame)
	s.mux.HandleFunc("GET /games/{id}", s.getGame)
	s.mux.HandleFunc("GET /games/{id}/events", s.events)
	s.mux.HandleFunc("POST /games/{id}/presses", s.press)
	s.mux.HandleFunc("POST /games/{id}/end", s.endGame)
	s.mux.HandleFunc("POST /games/{id}/code", s.drawCode)
	s.mux.HandleFunc("POST /games/{id}/paddle", s.paddle)
	s.mux.HandleFunc("POST /games/{id}/frames", s.frames)
	s.mux.HandleFunc("GET /games/{id}/relay", s.relay)
	s.routeUI()
	s.routeScreensaver()
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
	// Pace is a Go duration, "0s" for as fast as zaehlwerk answers. Only
	// headless games have one; a browser game goes at the speed it is
	// played.
	Pace string `json:"pace"`
	// Faults replaces PONG_FAULTS for this game, as a whole: a share left
	// out is zero, not the configured one. Absent keeps the configured
	// faults.
	Faults *Faults `json:"faults"`
	// Driver is "bots" (the default), "browser", or "check" for a button
	// check, which takes nothing else.
	Driver string `json:"driver"`
	// Players, Mode and Bots are a browser game's; see browserGame.
	Players [2]string     `json:"players"`
	Mode    string        `json:"mode"`
	Bots    [2]*BotParams `json:"bots"`
}

// started is the answer to a start: the game, and for a browser game the
// token its presses must carry. The token is in this answer and nowhere
// else, so the page that started the game is the one that reports for it. A
// game on two devices adds the code its guest joins with.
type started struct {
	View
	Token    string `json:"token,omitempty"`
	JoinCode string `json:"join_code,omitempty"`
}

// errBusy is a game or a match already holding the table.
var errBusy = errors.New("busy")

// startGame starts a headless bot match and answers once its match exists, so
// a refusal — the table taken, zaehlwerk unreachable — is this request's
// answer rather than a game that failed a moment later.
func (s *Server) startGame(w http.ResponseWriter, r *http.Request) {
	var body startBody
	if r.ContentLength != 0 {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			fail(w, http.StatusBadRequest, fmt.Errorf("decoding the body: %w", err))
			return
		}
	}
	cfg := GameConfig{Pace: DefaultPace}
	switch body.Driver {
	case "", DriverBots:
		if body.Players != ([2]string{}) || body.Mode != "" || body.Bots != ([2]*BotParams{}) {
			fail(w, http.StatusBadRequest, errors.New("players, mode and bots are for a browser game"))
			return
		}
	case DriverBrowser:
		if body.Pace != "" {
			fail(w, http.StatusBadRequest, errors.New("pace is for a headless game"))
			return
		}
		var err error
		if cfg, err = browserGame(body.Mode, body.Players, body.Bots); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
	case DriverCheck:
		if body.Pace != "" || body.Faults != nil || body.Players != ([2]string{}) || body.Mode != "" ||
			body.Bots != ([2]*BotParams{}) {
			fail(w, http.StatusBadRequest, errors.New("a button check takes no pace, faults, players, mode or bots"))
			return
		}
		cfg = checkGame()
	default:
		fail(w, http.StatusBadRequest, fmt.Errorf("driver %q: want bots, browser or check", body.Driver))
		return
	}
	if body.Pace != "" {
		p, err := time.ParseDuration(body.Pace)
		if err != nil || p < 0 {
			fail(w, http.StatusBadRequest, fmt.Errorf("pace %q: want a duration such as 500ms or 0s", body.Pace))
			return
		}
		cfg.Pace = p
	}
	if cfg.Driver != DriverCheck {
		cfg.Faults = s.cfg.Faults
	}
	if body.Faults != nil {
		// Unknown names are already refused by the decoder.
		if err := body.Faults.Validate(); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		cfg.Faults = *body.Faults
	}

	g, err := s.start(cfg)
	switch {
	case errors.Is(err, errBusy):
		fail(w, http.StatusConflict, err)
		return
	case err != nil:
		fail(w, http.StatusBadGateway, err)
		return
	}
	answer := started{View: g.View(), Token: g.token}
	if g.remote != nil {
		c, err := g.remote.draw()
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		answer.JoinCode = c.Code
	}
	w.Header().Set("Location", "/games/"+g.ID)
	writeJSON(w, http.StatusCreated, answer)
}

// The modes of a browser game, and the lobby's mode for a button check.
const (
	ModePvP   = "pvp"   // two people at one device
	ModePvC   = "pvc"   // a person as player a, the computer as b
	ModeCvC   = "cvc"   // two bots, in the browser, to watch
	ModeCheck = "check" // the buttons at the table
	// ModeRemote is two people on two devices (ADR-0009): the host plays
	// a on the page that started the game, the guest b on a second one.
	ModeRemote = "remote"
)

// checkGame is a button check: no faults, because the hardware brings its own,
// and the check's names.
func checkGame() GameConfig {
	return GameConfig{Driver: DriverCheck, Mode: ModeCheck, Players: CheckNames}
}

// maxNameRunes keeps a name to what fits on the page. The panel shows less: it
// cuts a name at six characters in its banner.
const maxNameRunes = 20

// browserGame checks what a browser game is started with and fills in what is
// left out: the names by mode, and a bot from the default preset.
func browserGame(mode string, players [2]string, bots [2]*BotParams) (GameConfig, error) {
	cfg := GameConfig{Driver: DriverBrowser, Mode: mode}
	defaults := map[string][2]string{
		ModePvP: {"Links", "Rechts"},
		ModePvC: {"Spieler", "Computer"},
		ModeCvC: BotNames,
		// The host names both: zaehlwerk takes the names when the match
		// is created, and the guest joins only afterwards.
		ModeRemote: {"Gastgeber", "Gast"},
	}
	if cfg.Mode == "" {
		cfg.Mode = ModePvP
	}
	names, ok := defaults[cfg.Mode]
	if !ok {
		return cfg, fmt.Errorf("mode %q: want pvp, pvc, cvc or remote", mode)
	}

	// Who is a bot follows from the mode, so a request cannot make a person
	// of the computer or the other way round: in pvc it is player b.
	wantBot := map[string][2]bool{ModePvP: {false, false}, ModePvC: {false, true}, ModeCvC: {true, true},
		ModeRemote: {false, false}}[cfg.Mode]
	preset, _ := Preset(DefaultSkill)
	for i := range players {
		name := strings.TrimSpace(players[i])
		if name == "" {
			name = names[i]
		}
		if utf8.RuneCountInString(name) > maxNameRunes || strings.ContainsFunc(name, unicode.IsControl) {
			return cfg, fmt.Errorf("player %q: want a name of at most %d characters", name, maxNameRunes)
		}
		cfg.Players[i] = name

		switch {
		case !wantBot[i] && bots[i] != nil:
			return cfg, fmt.Errorf("player %s is a person in mode %s, not a bot", playerAt(i), cfg.Mode)
		case !wantBot[i]:
		case bots[i] == nil:
			b := preset.Bot()
			cfg.Bots[i] = &b
		default:
			if err := bots[i].Validate(); err != nil {
				return cfg, fmt.Errorf("player %s: %w", playerAt(i), err)
			}
			b := *bots[i]
			cfg.Bots[i] = &b
		}
	}
	if cfg.Players[0] == cfg.Players[1] {
		return cfg, fmt.Errorf("players: two different names, not %q twice", cfg.Players[0])
	}
	return cfg, nil
}

func (s *Server) start(cfg GameConfig) (*Game, error) {
	s.starting.Lock()
	defer s.starting.Unlock()
	if !cfg.Attract {
		s.yield(cfg)
	}

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
	cfg.BestOf, cfg.Skills, cfg.Seed = s.cfg.BestOf, s.cfg.Skills, s.cfg.Seed+n
	cfg.ResetQuiet, cfg.Idle = s.resetQuiet, s.idle
	cfg.CheckTimeout, cfg.CheckGap = s.cfg.CheckTimeout, s.checkGap
	// A demo leaves the driver empty, which is bots, and keeps its pace.
	if cfg.Driver == DriverBrowser || cfg.Driver == DriverCheck {
		cfg.Pace = 0
	}
	g := newGame(id, cfg, s.client, s.log)
	// A check's token only ends it: it presses nothing.
	if cfg.Driver == DriverBrowser || cfg.Driver == DriverCheck {
		if g.token, err = newToken(); err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
	if cfg.Mode == ModeRemote {
		if g.remote, err = newRemote(s.now); err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
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
	ctx, cancel := context.WithCancelCause(s.ctx)
	g.stop = cancel

	// Connected before the match is created, so the game's own snapshot and
	// everything after it is on the stream rather than missed in between.
	table, err := s.client.Follow(ctx)
	if err != nil {
		cancel(nil)
		return nil, fmt.Errorf("reading the table from zaehlwerk: %w", err)
	}

	// The first event is the table as it is now. A current match is
	// somebody's: a reported one is a real match between real players, and
	// an unreported one is still a person scoring on the page, whose buttons
	// and page would land in the game's match the moment it became current.
	// The one exception is a demo that is not ours (ADR-0008): left behind by
	// a pod that is gone, or another instance's. A game somebody asks for
	// ends it, as yield ends our own; a demo does not, it waits for a quiet
	// one in attract mode.
	first, err := nextTableEvent(table)
	if err != nil {
		cancel(nil)
		return nil, err
	}
	if first.Demo && first.State != nil && !g.cfg.demo() && !s.ownsMatch(first.State.MatchID) {
		if first, err = s.endForeignDemo(ctx, first.State.MatchID, table); err != nil {
			cancel(nil)
			return nil, err
		}
	}
	if first.Kind != kindNoMatch {
		cancel(nil)
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
		cancel(nil)
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
		defer cancel(nil)
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

// newToken draws a browser game's token: enough that nobody guesses it, and
// nothing that needs to be stored beyond the game.
func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("drawing a token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
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

// nextTableEvent waits for the next event on /live/stream, bounded, because a
// start must answer rather than hang on a zaehlwerk that does not.
func nextTableEvent(table <-chan TableEvent) (TableEvent, error) {
	select {
	case ev, ok := <-table:
		if !ok {
			return TableEvent{}, errors.New("reading the table from zaehlwerk: the stream closed")
		}
		return ev, nil
	case <-time.After(5 * time.Second):
		return TableEvent{}, errors.New("reading the table from zaehlwerk: no answer on /live/stream")
	}
}

// endForeignDemo ends a demo that is not the service's own and returns what
// the table shows once it has gone: no_match, or whatever match is current
// after it. The demo's own ended event on the way is skipped.
func (s *Server) endForeignDemo(ctx context.Context, matchID string, table <-chan TableEvent) (TableEvent, error) {
	if _, err := s.client.EndMatch(ctx, matchID); err != nil {
		return TableEvent{}, fmt.Errorf("ending demo %s, which is not ours: %w", matchID, err)
	}
	s.log.Info("a demo that is not ours held the table, ended it for a new game", "match_id", matchID)
	for {
		ev, err := nextTableEvent(table)
		if err != nil {
			return TableEvent{}, err
		}
		if ev.State != nil && ev.State.MatchID == matchID {
			continue
		}
		return ev, nil
	}
}
