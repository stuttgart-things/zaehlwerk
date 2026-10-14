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

// Package fakesp is a Schmetterpause that answers the routes zaehlwerk calls,
// and nothing else.
//
// The fake Schmetterpause answers the four routes this service calls, with the
// refusals the real one gives, and makes the one call the real one makes to
// this service: the claim of a phone check-in. It is modelled on
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
	// Fob is the UID of the player's key fob, written the one way
	// checkin.NormaliseUID writes it. The real one keeps an HMAC of it
	// (Schmetterpause ADR-0025); a fake has nothing to protect.
	Fob string `json:"-"`
}

// DefaultRoster is fixed so the Taskfile can name ids without asking first.
// Olga is the observer: offered as an operator, never as a side, which is the
// distinction Schmetterpause ADR-0022 and ADR-0023 draw. The fobs are fixed
// for the same reason; DE:AD:BE:EF belongs to nobody.
var DefaultRoster = []Person{
	{ID: uuid.MustParse("11111111-1111-4111-8111-111111111111"), DisplayName: "Anna", TTR: 1520, Fob: "04:A3:1B:22"},
	{ID: uuid.MustParse("22222222-2222-4222-8222-222222222222"), DisplayName: "Bernd", TTR: 1480, Fob: "04:5C:7E:91"},
	{ID: uuid.MustParse("33333333-3333-4333-8333-333333333333"), DisplayName: "Clara", TTR: 1610, Fob: "04:11:22:33:44:55:66"},
	{ID: uuid.MustParse("44444444-4444-4444-8444-444444444444"), DisplayName: "Olga", Observer: true},
}

// resultBody is POST /api/results as Schmetterpause decodes it. The ids are
// uuid.UUID there too, so an id that is not one is a 400 here as it is there.
type resultBody struct {
	HomeID        uuid.UUID  `json:"home_id"`
	AwayID        uuid.UUID  `json:"away_id"`
	OperatorID    uuid.UUID  `json:"operator_id"`
	HomeCheckinID uuid.UUID  `json:"home_checkin_id"`
	AwayCheckinID uuid.UUID  `json:"away_checkin_id"`
	Sets          [][2]int   `json:"sets"`
	BestOf        int        `json:"best_of"`
	PointsToWin   int        `json:"points_to_win"`
	PlayedAt      *time.Time `json:"played_at,omitempty"`
}

// Result is one accepted result, with the names resolved so that
// GET /results reads as a match rather than as three uuids.
type Result struct {
	MatchID  uuid.UUID `json:"match_id"`
	Status   string    `json:"status"`
	Home     string    `json:"home"`
	Away     string    `json:"away"`
	Operator string    `json:"operator"`
	// EnteredVia is "scoreboard" for a result with an operator and "checkin"
	// for one proven by two check-ins, as the real one records it.
	EnteredVia    string     `json:"entered_via"`
	HomeCheckinID *uuid.UUID `json:"home_checkin_id,omitempty"`
	AwayCheckinID *uuid.UUID `json:"away_checkin_id,omitempty"`
	Sets          [][2]int   `json:"sets"`
	BestOf        int        `json:"best_of"`
	PlayedAt      *time.Time `json:"played_at,omitempty"`
	ReceivedAt    time.Time  `json:"received_at"`
}

// Server is the fake. Roster may be replaced before it serves, for a test that
// needs other people in it.
type Server struct {
	token  string
	Roster []Person
	log    *slog.Logger

	// NoFobs leaves the resolve route out, as the real one does without
	// SP_RFID_UID_KEY. Set before it serves.
	NoFobs bool
	// Zaehlwerk is where POST /phone claims a half, the real one's
	// SP_ZAEHLWERK_URL. Empty, there is no phone check-in, as there.
	Zaehlwerk string

	mu       sync.Mutex
	mode     string
	results  []Result
	checkins map[uuid.UUID]*Checkin
	// window is the player whose "Anhänger koppeln" window is open, and
	// until when. The next unknown fob becomes theirs.
	window      uuid.UUID
	windowUntil time.Time
	now         func() time.Time
}

func New(token, mode string, log *slog.Logger) (*Server, error) {
	if err := checkMode(mode); err != nil {
		return nil, err
	}
	roster := make([]Person, len(DefaultRoster))
	copy(roster, DefaultRoster)
	return &Server{token: token, Roster: roster, mode: mode, log: log,
		checkins: make(map[uuid.UUID]*Checkin), now: time.Now}, nil
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
		if !f.NoFobs {
			mux.HandleFunc("POST /api/identities/rfid/resolve", f.resolveFob)
		}
	}

	// The fake's own routes, not Schmetterpause's. No token, because they
	// exist for a person at a terminal and not for zaehlwerk.
	mux.HandleFunc("GET /results", f.listResults)
	mux.HandleFunc("GET /mode", f.getMode)
	mux.HandleFunc("POST /mode", f.setMode)
	mux.HandleFunc("GET /checkins", f.listCheckins)
	mux.HandleFunc("POST /link", f.openWindow)
	mux.HandleFunc("POST /phone", f.phoneCheckin)
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
	checkedIn := body.HomeCheckinID != uuid.Nil || body.AwayCheckinID != uuid.Nil
	switch {
	case body.HomeID == uuid.Nil || body.AwayID == uuid.Nil:
		f.refuse(w, http.StatusBadRequest, "home_id and away_id are required")
		return
	case body.HomeID == body.AwayID:
		f.refuse(w, http.StatusBadRequest, "home_id and away_id must differ")
		return
	case checkedIn && body.OperatorID != uuid.Nil:
		f.refuse(w, http.StatusBadRequest, "send operator_id or home_checkin_id and away_checkin_id, not both")
		return
	case checkedIn && (body.HomeCheckinID == uuid.Nil || body.AwayCheckinID == uuid.Nil):
		f.refuse(w, http.StatusBadRequest, "home_checkin_id and away_checkin_id are both required")
		return
	case checkedIn && body.HomeCheckinID == body.AwayCheckinID:
		f.refuse(w, http.StatusBadRequest, "home_checkin_id and away_checkin_id must differ")
		return
	case checkedIn:
		f.checkedInResult(w, body)
		return
	case body.OperatorID == uuid.Nil:
		f.refuse(w, http.StatusBadRequest, "operator_id is required, or home_checkin_id and away_checkin_id")
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
		EnteredVia: "scoreboard",
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
	// As the real one without SP_RFID_UID_KEY: no resolve route at all.
	if os.Getenv("SCHMETTERPAUSE_FOBS") == "off" {
		fake.NoFobs = true
		log.Info("no fob route, as without SP_RFID_UID_KEY")
	}

	fake.Zaehlwerk = os.Getenv("ZAEHLWERK_URL")
	if fake.Zaehlwerk == "" || token == "" {
		log.Info("no phone check-in, it needs ZAEHLWERK_URL and SCHMETTERPAUSE_TOKEN")
	} else {
		log.Info("phone check-in claims at zaehlwerk", "zaehlwerk", fake.Zaehlwerk)
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
