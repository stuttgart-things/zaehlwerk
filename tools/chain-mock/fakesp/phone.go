package fakesp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// claimTimeout is how long the fake waits for zaehlwerk, as the real one
// waits briefly and does not retry (Schmetterpause ADR-0026).
const claimTimeout = 5 * time.Second

// phoneCheckin is the fake's "Am Tisch anmelden": POST /phone with player_id
// and half, standing in for a player who scanned the QR code at that half and
// pressed the button. Like the real one it records the check-in first, with
// method phone, and then claims the half at zaehlwerk's POST /checkin/claim
// with its own token as a bearer (zaehlwerk ADR-0011). The answer is
// zaehlwerk's, status and body, so a terminal sees what the page would put
// into words.
//
// checkin_id, when given, is used instead of a new one, so that a repeated
// claim of one check-in can be played, as a double tap or a proxy retry
// would send it.
func (f *Server) phoneCheckin(w http.ResponseWriter, r *http.Request) {
	if f.Zaehlwerk == "" || f.token == "" {
		// As the real one: /table/* exists only with SP_ZAEHLWERK_URL and
		// SP_SCOREBOARD_TOKEN together.
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "no phone check-in without ZAEHLWERK_URL and SCHMETTERPAUSE_TOKEN"})
		return
	}
	half := r.FormValue("half")
	if half != "left" && half != "right" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "half must be left or right"})
		return
	}
	playerID, err := uuid.Parse(r.FormValue("player_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "player_id is not a uuid"})
		return
	}

	c, status, msg := f.recordPhoneCheckin(playerID, half, r.FormValue("checkin_id"))
	if c == nil {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	status, answer, err := f.claim(r.Context(), c)
	if err != nil {
		f.log.Warn("zaehlwerk did not answer the claim", "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "zaehlwerk not reachable: " + err.Error()})
		return
	}
	f.log.Info("phone check-in claimed at zaehlwerk", "player", c.Player, "half", half, "status", status)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(answer)
}

// recordPhoneCheckin records the check-in, or finds the one named by
// checkinID for a repeat. It answers nil with a status and a reason when
// there is nobody to check in.
func (f *Server) recordPhoneCheckin(playerID uuid.UUID, half, checkinID string) (*Checkin, int, string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if checkinID != "" {
		id, err := uuid.Parse(checkinID)
		if err != nil {
			return nil, http.StatusBadRequest, "checkin_id is not a uuid"
		}
		c := f.checkins[id]
		if c == nil || c.PlayerID != playerID || c.Half != half {
			return nil, http.StatusNotFound, "no such check-in for that player at that half"
		}
		return c, 0, ""
	}

	for _, p := range f.Roster {
		if p.ID != playerID {
			continue
		}
		if p.Observer {
			// ADR-0022 over there: an observer does not check in.
			return nil, http.StatusUnprocessableEntity, "an observer does not check in"
		}
		c := &Checkin{ID: uuid.New(), PlayerID: p.ID, Player: p.DisplayName, Half: half,
			Method: MethodPhone, CreatedAt: f.now()}
		f.checkins[c.ID] = c
		f.log.Info("phone check-in recorded", "player", p.DisplayName, "checkin_id", c.ID, "half", half)
		return c, 0, ""
	}
	return nil, http.StatusNotFound, "no such player"
}

// claim is the one call to zaehlwerk.
func (f *Server) claim(ctx context.Context, c *Checkin) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, claimTimeout)
	defer cancel()

	body, err := json.Marshal(struct {
		Half      string    `json:"half"`
		PlayerID  uuid.UUID `json:"player_id"`
		CheckinID uuid.UUID `json:"checkin_id"`
	}{c.Half, c.PlayerID, c.ID})
	if err != nil {
		return 0, nil, fmt.Errorf("encoding the claim: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(f.Zaehlwerk, "/")+"/checkin/claim", bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("building the claim: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("POST /checkin/claim: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return 0, nil, fmt.Errorf("reading the answer: %w", err)
	}
	return resp.StatusCode, answer, nil
}
