package schmetterpause

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// FobCheckin is what Schmetterpause answers for a fob it knows: who it is, and
// the check-in it recorded for that player at that half. Every answer is a new
// check-in over there, which is why the caller must not ask twice for one
// press (ADR-0010).
type FobCheckin struct {
	PlayerID  string `json:"player_id"`
	CheckinID string `json:"checkin_id"`
}

var (
	// ErrUnknownFob is a fob nobody owns while no link window is open: a 404
	// with an empty body. Not a failure of the coupling, and nothing about the
	// card is in it.
	ErrUnknownFob = errors.New("schmetterpause: unknown fob")
	// ErrNoFobRoute is a Schmetterpause that does not serve the resolve route
	// at all: a 404 with the mux's own body, because SP_RFID_UID_KEY or the
	// token is not configured over there. Check-in cannot work until it is.
	ErrNoFobRoute = errors.New("schmetterpause: no fob route, SP_RFID_UID_KEY or the scoreboard token is not configured there")
)

// fobResolve is the body of POST /api/identities/rfid/resolve. The UID goes
// in the body and never in the path, so that no access log on the way has it.
type fobResolve struct {
	UID  string `json:"uid"`
	Half string `json:"half"`
}

// ResolveFob asks Schmetterpause who a fob is and has it record a check-in at
// half, "left" or "right".
//
// The UID is in the request body and nowhere else. No error returned from
// here quotes it, not even when Schmetterpause's answer does: the answer is
// dropped from the error in that case. A company badge held against the
// reader by mistake has to pass through without a trace (ADR-0010).
func (c *Client) ResolveFob(ctx context.Context, uid, half string) (FobCheckin, error) {
	body, err := json.Marshal(fobResolve{UID: uid, Half: half})
	if err != nil {
		return FobCheckin{}, errors.New("schmetterpause: encoding the fob request failed")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/api/identities/rfid/resolve", bytes.NewReader(body))
	if err != nil {
		return FobCheckin{}, fmt.Errorf("schmetterpause: building the fob request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)

	resp, err := c.client.Do(req)
	if err != nil {
		// A transport error names the URL and the cause, and the UID is in
		// neither.
		return FobCheckin{}, fmt.Errorf("schmetterpause: resolving a fob: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// Both are 404s. The route's own answer is empty; Go's mux, when the
		// route is not registered, says "404 page not found".
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if len(bytes.TrimSpace(raw)) == 0 {
			return FobCheckin{}, ErrUnknownFob
		}
		return FobCheckin{}, ErrNoFobRoute
	default:
		return FobCheckin{}, fmt.Errorf("schmetterpause: resolving a fob: %w", scrubbedStatusError(resp, uid))
	}

	var got FobCheckin
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&got); err != nil {
		return FobCheckin{}, fmt.Errorf("schmetterpause: decoding the fob answer: %w", err)
	}
	if got.PlayerID == "" || got.CheckinID == "" {
		return FobCheckin{}, errors.New("schmetterpause: the fob answer has no player_id or checkin_id")
	}
	return got, nil
}

// scrubbedStatusError is statusError for an answer that might quote the UID.
// Schmetterpause's refusals on this route are fixed sentences and do not, but
// a proxy's error page or a later version might, and the rule is that the UID
// appears in no error message here whatever the other side does.
func scrubbedStatusError(resp *http.Response, uid string) error {
	err := statusError(resp)
	if digits := hexDigits(uid); digits != "" && strings.Contains(hexDigits(err.Error()), digits) {
		return errors.New(resp.Status)
	}
	return err
}

// hexDigits keeps the hex digits of s, lowercased, so that a UID written with
// colons is found in an answer that writes it with dashes or none.
func hexDigits(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			b.WriteRune(r)
		}
	}
	return b.String()
}
