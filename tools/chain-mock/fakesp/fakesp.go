package fakesp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Package fakesp is a Schmetterpause that answers the three routes zaehlwerk
// calls, and nothing else.
//
// The fake Schmetterpause answers the three routes this service calls, with the
// refusals the real one gives, and nothing else. It is modelled on
// schmetterpause internal/server/api.go: same status codes, same order of
// checks, same {"error": …} body. The contract test drives it with the real
// internal/schmetterpause client, so a change on this side that the client does
// not speak shows up there rather than at a table.
//
// Nothing is written anywhere. A result lives until the process stops, which is
// all a look at the end of the chain needs.

// Modes the fake can be put in, so the lost-result path from ADR-0004 is
// something to watch rather than to take on trust.
const (
	ModeAccept = "accept"
	ModeRefuse = "refuse" // 503, as an unreachable or broken instance would look
	ModeHang   = "hang"   // never answers; the client's own timeout ends it
)

// hangCap bounds mode hang, well past the five seconds zaehlwerk waits.
const hangCap = time.Minute

type Person struct {
	ID          uuid.UUID `json:"id"`
	DisplayName string    `json:"display_name"`
	TTR         int       `json:"ttr"`
	Observer    bool      `json:"-"`
}

// DefaultRoster is fixed so the Taskfile can name ids without asking first.
// Olga is the observer: offered as an operator, never as a side, which is the
// distinction Schmetterpause ADR-0022 and ADR-0023 draw.
var DefaultRoster = []Person{
	{ID: uuid.MustParse("11111111-1111-4111-8111-111111111111"), DisplayName: "Anna", TTR: 1520},
	{ID: uuid.MustParse("22222222-2222-4222-8222-222222222222"), DisplayName: "Bernd", TTR: 1480},
	{ID: uuid.MustParse("33333333-3333-4333-8333-333333333333"), DisplayName: "Clara", TTR: 1610},
	{ID: uuid.MustParse("44444444-4444-4444-8444-444444444444"), DisplayName: "Olga", Observer: true},
}

// resultBody is POST /api/results as Schmetterpause decodes it. The ids are
// uuid.UUID there too, so an id that is not one is a 400 here as it is there.
type resultBody struct {
	HomeID      uuid.UUID  `json:"home_id"`
	AwayID      uuid.UUID  `json:"away_id"`
	OperatorID  uuid.UUID  `json:"operator_id"`
	Sets        [][2]int   `json:"sets"`
	BestOf      int        `json:"best_of"`
	PointsToWin int        `json:"points_to_win"`
	PlayedAt    *time.Time `json:"played_at,omitempty"`
}

// Result is one accepted result, with the names resolved so that
// GET /results reads as a match rather than as three uuids.
type Result struct {
	MatchID    uuid.UUID  `json:"match_id"`
	Status     string     `json:"status"`
	Home       string     `json:"home"`
	Away       string     `json:"away"`
	Operator   string     `json:"operator"`
	Sets       [][2]int   `json:"sets"`
	BestOf     int        `json:"best_of"`
	PlayedAt   *time.Time `json:"played_at,omitempty"`
	ReceivedAt time.Time  `json:"received_at"`
}

// Server is the fake. Roster may be replaced before it serves, for a test that
// needs other people in it.
type Server struct {
	token  string
	Roster []Person
	log    *slog.Logger

	mu      sync.Mutex
	mode    string
	results []Result
}

func New(token, mode string, log *slog.Logger) (*Server, error) {
	if err := checkMode(mode); err != nil {
		return nil, err
	}
	return &Server{token: token, Roster: DefaultRoster, mode: mode, log: log}, nil
}

func checkMode(mode string) error {
	switch mode {
	case ModeAccept, ModeRefuse, ModeHang:
		return nil
	}
	return fmt.Errorf("mode %q: want %s, %s or %s", mode, ModeAccept, ModeRefuse, ModeHang)
}

func (f *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Unset token, no routes: the real one does not register /api at all
	// without SP_SCOREBOARD_TOKEN, and a 404 from here should mean the same
	// thing it means there.
	if f.token != "" {
		mux.HandleFunc("GET /api/players", f.players)
		mux.HandleFunc("GET /api/operators", f.operators)
		mux.HandleFunc("POST /api/results", f.result)
	}

	// The fake's own routes, not Schmetterpause's. No token, because they
	// exist for a person at a terminal and not for zaehlwerk.
	mux.HandleFunc("GET /results", f.listResults)
	mux.HandleFunc("GET /mode", f.getMode)
	mux.HandleFunc("POST /mode", f.setMode)
	return mux
}

func (f *Server) authorized(r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(token), []byte(f.token)) == 1
}

func (f *Server) refuse(w http.ResponseWriter, status int, msg string) {
	f.log.Info("refused", "status", status, "error", msg)
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{msg})
}

func (f *Server) players(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(r) {
		f.refuse(w, http.StatusUnauthorized, "a bearer token is required")
		return
	}
	out := []Person{}
	for _, p := range f.Roster {
		if !p.Observer {
			out = append(out, p)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *Server) operators(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(r) {
		f.refuse(w, http.StatusUnauthorized, "a bearer token is required")
		return
	}
	type operator struct {
		ID          uuid.UUID `json:"id"`
		DisplayName string    `json:"display_name"`
		Observer    bool      `json:"observer"`
	}
	out := make([]operator, 0, len(f.Roster))
	for _, p := range f.Roster {
		out = append(out, operator{ID: p.ID, DisplayName: p.DisplayName, Observer: p.Observer})
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *Server) result(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(r) {
		f.refuse(w, http.StatusUnauthorized, "a bearer token is required")
		return
	}

	switch f.Mode() {
	case ModeRefuse:
		f.refuse(w, http.StatusServiceUnavailable, "fake schmetterpause is refusing results (mode refuse)")
		return
	case ModeHang:
		f.log.Info("holding a result without answering (mode hang)")
		// Read first: net/http only notices a client that gave up once the
		// body is consumed, and until then the request context never ends.
		// The cap is for a client with no timeout of its own.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(hangCap):
		}
		return
	}

	var body resultBody
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	// The real one refuses a field it does not know, because a misspelt one
	// would enter a result that is not the one that was played.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		f.refuse(w, http.StatusBadRequest, "the body is not a result: "+err.Error())
		return
	}

	if msg := validateResult(body); msg != "" {
		f.refuse(w, http.StatusBadRequest, msg)
		return
	}
	switch {
	case body.HomeID == uuid.Nil || body.AwayID == uuid.Nil:
		f.refuse(w, http.StatusBadRequest, "home_id and away_id are required")
		return
	case body.HomeID == body.AwayID:
		f.refuse(w, http.StatusBadRequest, "home_id and away_id must differ")
		return
	case body.OperatorID == uuid.Nil:
		f.refuse(w, http.StatusBadRequest, "operator_id is required")
		return
	case body.OperatorID == body.HomeID || body.OperatorID == body.AwayID:
		f.refuse(w, http.StatusUnprocessableEntity, "the operator may not be one of the two players")
		return
	}

	names := make(map[uuid.UUID]Person, len(f.Roster))
	for _, p := range f.Roster {
		names[p.ID] = p
	}
	for _, id := range []uuid.UUID{body.HomeID, body.AwayID, body.OperatorID} {
		if _, ok := names[id]; !ok {
			f.refuse(w, http.StatusUnprocessableEntity, fmt.Sprintf("no player with id %s", id))
			return
		}
	}
	if names[body.HomeID].Observer || names[body.AwayID].Observer {
		f.refuse(w, http.StatusUnprocessableEntity, "an observer cannot be one of the two players")
		return
	}

	// Pending, as the real one stores every scoreboard result while its
	// ADR-0015 test phase runs: a player confirms it before it counts.
	stored := Result{
		MatchID:    uuid.New(),
		Status:     "pending",
		Home:       names[body.HomeID].DisplayName,
		Away:       names[body.AwayID].DisplayName,
		Operator:   names[body.OperatorID].DisplayName,
		Sets:       body.Sets,
		BestOf:     body.BestOf,
		PlayedAt:   body.PlayedAt,
		ReceivedAt: time.Now(),
	}
	f.mu.Lock()
	f.results = append(f.results, stored)
	f.mu.Unlock()

	f.log.Info("result recorded", "match_id", stored.MatchID,
		"home", stored.Home, "away", stored.Away, "operator", stored.Operator, "sets", stored.Sets)
	writeJSON(w, http.StatusCreated, struct {
		MatchID uuid.UUID `json:"match_id"`
		Status  string    `json:"status"`
	}{stored.MatchID, stored.Status})
}

// validateResult is the arithmetic of schmetterpause internal/match.Validate,
// restated: a result this service would send and the real one would refuse is
// exactly what the chain is run to find, so the fake must not be laxer.
func validateResult(b resultBody) string {
	if len(b.Sets) == 0 {
		return "sets must not be empty"
	}
	if b.BestOf <= 0 || b.PointsToWin <= 0 {
		return "best_of and points_to_win are required"
	}
	switch b.BestOf {
	case 1, 3, 5, 7:
	default:
		return fmt.Sprintf("best_of %d is not a mode", b.BestOf)
	}
	if b.PointsToWin != 11 && b.PointsToWin != 21 {
		return fmt.Sprintf("points_to_win %d is not a mode", b.PointsToWin)
	}
	if len(b.Sets) > b.BestOf {
		return fmt.Sprintf("%d sets in a best of %d", len(b.Sets), b.BestOf)
	}

	toWin := b.BestOf/2 + 1
	home, away := 0, 0
	for i, s := range b.Sets {
		n := i + 1
		hi, lo := max(s[0], s[1]), min(s[0], s[1])
		switch {
		case lo < 0:
			return fmt.Sprintf("set %d: negative points", n)
		case hi == lo:
			return fmt.Sprintf("set %d: a set cannot be drawn", n)
		case hi < b.PointsToWin:
			return fmt.Sprintf("set %d: not finished", n)
		case hi-lo < 2:
			return fmt.Sprintf("set %d: won by less than two", n)
		case hi > b.PointsToWin && (hi-lo != 2 || lo < b.PointsToWin-1):
			return fmt.Sprintf("set %d: played past its end", n)
		}
		if home == toWin || away == toWin {
			return fmt.Sprintf("set %d: played after the match was decided", n)
		}
		if s[0] > s[1] {
			home++
		} else {
			away++
		}
	}
	if home != toWin && away != toWin {
		return "nobody won the match"
	}
	return ""
}

func (f *Server) Mode() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mode
}

func (f *Server) Results() []Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Result{}, f.results...)
}

func (f *Server) listResults(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, f.Results())
}

func (f *Server) getMode(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"mode": f.Mode()})
}

// setMode switches while running, so a match can be lost on purpose and then
// retried from the page without restarting anything in between.
func (f *Server) setMode(w http.ResponseWriter, r *http.Request) {
	mode := r.FormValue("mode")
	if err := checkMode(mode); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	f.mu.Lock()
	f.mode = mode
	f.mu.Unlock()
	f.log.Info("mode changed", "mode", mode)
	writeJSON(w, http.StatusOK, map[string]string{"mode": mode})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// RunFromEnv serves the fake until ctx ends, configured the way everything here
// is: environment variables with defaults.
func RunFromEnv(ctx context.Context, log *slog.Logger) error {
	token := os.Getenv("SCHMETTERPAUSE_TOKEN")
	fake, err := New(token, env("SCHMETTERPAUSE_MODE", ModeAccept), log)
	if err != nil {
		return fmt.Errorf("SCHMETTERPAUSE_MODE: %w", err)
	}
	if token == "" {
		log.Warn("SCHMETTERPAUSE_TOKEN not set, so /api does not exist — as with the real one")
	}

	srv := &http.Server{
		Addr:              env("SCHMETTERPAUSE_ADDR", ":8082"),
		Handler:           fake.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	log.Info("listening", "addr", srv.Addr, "mode", fake.Mode())
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving: %w", err)
	}
	return nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
