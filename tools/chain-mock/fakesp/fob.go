package fakesp

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CheckinValidity is how long a check-in can carry a result, as in
// schmetterpause internal/checkin.
const CheckinValidity = 3 * time.Hour

// linkWindow is how long "Anhänger koppeln" stays open.
const linkWindow = time.Minute

// How a check-in was made, as the real one's checkins.method says.
const (
	MethodRFID  = "rfid"
	MethodPhone = "phone"
)

// Checkin is one check-in the resolve route or the phone control recorded.
// Every resolve records a new one, as the real one does.
type Checkin struct {
	ID        uuid.UUID `json:"id"`
	PlayerID  uuid.UUID `json:"player_id"`
	Player    string    `json:"player"`
	Half      string    `json:"half"`
	Method    string    `json:"method"`
	CreatedAt time.Time `json:"created_at"`
	Used      bool      `json:"used"`
}

var errBadUID = errors.New("a uid is 4, 7 or 10 bytes of hex")

// normaliseUID is schmetterpause internal/auth normaliseUID, written out the
// way checkin.NormaliseUID writes it so that the roster can name fobs.
func normaliseUID(uid string) (string, error) {
	clean := strings.Map(func(r rune) rune {
		switch r {
		case ':', '-', ' ':
			return -1
		}
		return r
	}, uid)
	raw, err := hex.DecodeString(clean)
	if err != nil {
		return "", errBadUID
	}
	switch len(raw) {
	case 4, 7, 10:
	default:
		return "", errBadUID
	}
	parts := make([]string, len(raw))
	for i, b := range raw {
		parts[i] = strings.ToUpper(hex.EncodeToString([]byte{b}))
	}
	return strings.Join(parts, ":"), nil
}

// resolveFob is POST /api/identities/rfid/resolve: who a fob is, and a new
// check-in for them at that half. Same checks, same answers as the real one,
// and like the real one it never logs or echoes the UID.
func (f *Server) resolveFob(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(r) {
		f.refuse(w, http.StatusUnauthorized, "a bearer token is required")
		return
	}
	switch f.Mode() {
	case ModeRefuse:
		f.refuse(w, http.StatusServiceUnavailable, "fake schmetterpause is refusing (mode refuse)")
		return
	case ModeHang:
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(hangCap):
		}
		return
	}

	var body struct {
		UID  string `json:"uid"`
		Half string `json:"half"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		f.refuse(w, http.StatusBadRequest, "the body is not a fob resolve request")
		return
	}
	if body.Half != "left" && body.Half != "right" {
		f.refuse(w, http.StatusBadRequest, "half must be left or right")
		return
	}
	uid, err := normaliseUID(body.UID)
	if err != nil {
		f.refuse(w, http.StatusBadRequest, errBadUID.Error())
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	owner := -1
	for i, p := range f.Roster {
		if p.Fob == uid {
			owner = i
		}
	}
	if owner < 0 && f.window != uuid.Nil && f.now().Before(f.windowUntil) {
		for i, p := range f.Roster {
			if p.ID == f.window {
				f.Roster[i].Fob = uid
				owner = i
				f.log.Info("fob linked", "player", p.DisplayName)
			}
		}
		f.window = uuid.Nil
	}
	if owner < 0 {
		f.log.Info("unknown fob")
		w.WriteHeader(http.StatusNotFound)
		return
	}

	p := f.Roster[owner]
	c := &Checkin{ID: uuid.New(), PlayerID: p.ID, Player: p.DisplayName, Half: body.Half, Method: MethodRFID, CreatedAt: f.now()}
	f.checkins[c.ID] = c
	f.log.Info("fob check-in recorded", "player", p.DisplayName, "checkin_id", c.ID, "half", c.Half)
	writeJSON(w, http.StatusOK, struct {
		PlayerID  uuid.UUID `json:"player_id"`
		CheckinID uuid.UUID `json:"checkin_id"`
	}{p.ID, c.ID})
}

// checkedInResult stores a result proven by two check-ins: schmetterpause
// recordCheckedInResult and internal/checkin.Verify, in the same order. The
// shape of the body has been checked already.
func (f *Server) checkedInResult(w http.ResponseWriter, body resultBody) {
	f.mu.Lock()
	defer f.mu.Unlock()

	home, away := f.checkins[body.HomeCheckinID], f.checkins[body.AwayCheckinID]
	for _, id := range []uuid.UUID{body.HomeCheckinID, body.AwayCheckinID} {
		if f.checkins[id] == nil {
			f.refuse(w, http.StatusUnprocessableEntity, fmt.Sprintf("no check-in with id %s", id))
			return
		}
	}
	switch {
	case home.PlayerID != body.HomeID:
		f.refuse(w, http.StatusUnprocessableEntity, fmt.Sprintf("check-in %s belongs to the wrong player", home.ID))
		return
	case away.PlayerID != body.AwayID:
		f.refuse(w, http.StatusUnprocessableEntity, fmt.Sprintf("check-in %s belongs to the wrong player", away.ID))
		return
	case home.Half == away.Half:
		f.refuse(w, http.StatusUnprocessableEntity, "both check-ins are at the same half")
		return
	case home.Used || away.Used:
		// Conflict, as the real one answers: the likeliest sender is the
		// Zählwerk retrying a result that did arrive.
		f.refuse(w, http.StatusConflict, "a check-in already carries a result")
		return
	}
	for _, c := range []*Checkin{home, away} {
		if f.now().Sub(c.CreatedAt) > CheckinValidity {
			f.refuse(w, http.StatusUnprocessableEntity, fmt.Sprintf("check-in %s has expired", c.ID))
			return
		}
	}
	names := make(map[uuid.UUID]Person, len(f.Roster))
	for _, p := range f.Roster {
		names[p.ID] = p
	}
	if names[body.HomeID].Observer || names[body.AwayID].Observer {
		f.refuse(w, http.StatusUnprocessableEntity, "an observer cannot be one of the two players")
		return
	}

	home.Used, away.Used = true, true
	stored := Result{
		MatchID:       uuid.New(),
		Status:        "pending",
		Home:          names[body.HomeID].DisplayName,
		Away:          names[body.AwayID].DisplayName,
		EnteredVia:    "checkin",
		HomeCheckinID: &home.ID,
		AwayCheckinID: &away.ID,
		Sets:          body.Sets,
		BestOf:        body.BestOf,
		PlayedAt:      body.PlayedAt,
		ReceivedAt:    time.Now(),
	}
	f.results = append(f.results, stored)
	f.log.Info("checked-in result recorded", "match_id", stored.MatchID,
		"home", stored.Home, "away", stored.Away, "sets", stored.Sets)
	writeJSON(w, http.StatusCreated, struct {
		MatchID uuid.UUID `json:"match_id"`
		Status  string    `json:"status"`
	}{stored.MatchID, stored.Status})
}

// Checkins is every check-in recorded so far.
func (f *Server) Checkins() []Checkin {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Checkin, 0, len(f.checkins))
	for _, c := range f.checkins {
		out = append(out, *c)
	}
	return out
}

func (f *Server) listCheckins(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, f.Checkins())
}

// openWindow is the fake's "Anhänger koppeln": POST /link with player_id. The
// next unknown fob within a minute becomes that player's.
func (f *Server) openWindow(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.FormValue("player_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "player_id is not a uuid"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.Roster {
		if p.ID == id && !p.Observer {
			f.window, f.windowUntil = id, f.now().Add(linkWindow)
			writeJSON(w, http.StatusOK, map[string]string{"player": p.DisplayName, "until": f.windowUntil.Format(time.RFC3339)})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such player"})
}
