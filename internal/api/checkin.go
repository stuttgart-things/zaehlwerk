package api

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/stuttgart-things/zaehlwerk/internal/checkin"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// errCheckinOff is the answer while no Schmetterpause or no token for the
// path is configured: CHECKIN_TOKEN for the hub, SCHMETTERPAUSE_TOKEN for
// Schmetterpause's claim. A 503 rather than a 404, like the stream without a
// hub: the route exists, the feature does not, and a caller that gets this
// knows to stop rather than to look for another URL. It does not say which is
// missing; the start log does.
var errCheckinOff = errors.New("check-in is off on this zaehlwerk")

// errCheckinToken is a check-in without the hub's token, or with another.
var errCheckinToken = errors.New("check-in needs the hub's token")

// errClaimToken is a claim without Schmetterpause's token, or with another.
var errClaimToken = errors.New("a claim needs Schmetterpause's token")

// checkinRequest is POST /ingest/checkin: the ingest contract of ADR-0002, a
// half and a UID (ADR-0010).
type checkinRequest struct {
	Source  string `json:"source"`
	EventID uint64 `json:"event_id"`
	Half    string `json:"half"`
	UID     string `json:"uid"`
}

// claimRequest is POST /checkin/claim: Schmetterpause has recorded a phone
// check-in and claims the half for its player (ADR-0011).
type claimRequest struct {
	Half      string `json:"half"`
	PlayerID  string `json:"player_id"`
	CheckinID string `json:"checkin_id"`
}

// checkinAnswer is what the hub gets back, and what drives its LED: checked
// in, unknown, refused. The same body comes back for a retry of the same
// event_id, with repeated set. It never carries the UID.
type checkinAnswer struct {
	Outcome checkin.Outcome `json:"outcome"`
	Half    scorer.Half     `json:"half"`
	Name    string          `json:"name,omitempty"`
	MatchID string          `json:"match_id,omitempty"`
	Error   string          `json:"error,omitempty"`
	// Repeated marks the answer to a retry: it is the first attempt's.
	Repeated bool `json:"repeated,omitempty"`
	// Left and Right are who holds each half now, by name.
	Left  string `json:"left,omitempty"`
	Right string `json:"right,omitempty"`
}

// checkinStatus is GET /checkin: whether check-in is on, by which path, and
// who holds which half. Names only, no ids, as in a match's state.
type checkinStatus struct {
	// Enabled is either path.
	Enabled bool `json:"enabled"`
	// Fob is the hub's /ingest/checkin, Phone Schmetterpause's
	// /checkin/claim.
	Fob     bool          `json:"fob,omitempty"`
	Phone   bool          `json:"phone,omitempty"`
	Timeout string        `json:"timeout,omitempty"`
	Left    *checkin.View `json:"left"`
	Right   *checkin.View `json:"right"`
}

func (s *Server) ingestCheckin(w http.ResponseWriter, r *http.Request) {
	if s.desk == nil || s.checkinToken == "" {
		s.fail(w, r, http.StatusServiceUnavailable, errCheckinOff, nil)
		return
	}
	// Before the body is read: a check-in names a player to Schmetterpause,
	// and during "Fob koppeln" pairs a UID with one, so it is taken only from
	// the hub (#113). A fob UID is no secret, a phone reads one in a second;
	// the token is what says the request came from the reader at the table.
	if !bearerIs(r, s.checkinToken) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="checkin"`)
		s.fail(w, r, http.StatusUnauthorized, errCheckinToken, nil)
		return
	}

	var body checkinRequest
	if err := decode(w, r, &body); err != nil {
		// Not the decoder's message: it can quote the input, and the input
		// has a card number in it (ADR-0010).
		s.fail(w, r, http.StatusBadRequest, errors.New("decoding the body: not a check-in request"), nil)
		return
	}

	req := checkin.Request{
		Source: body.Source, EventID: body.EventID,
		Half: scorer.Half(body.Half), UID: body.UID,
	}
	if err := req.Validate(); err != nil {
		s.fail(w, r, http.StatusBadRequest, err, nil)
		return
	}

	s.writeCheckinAnswer(w, s.desk.CheckIn(r.Context(), req, table{s}))
}

// claimCheckin is POST /checkin/claim: Schmetterpause claims a half for a
// player who checked in there with their phone (ADR-0011). The answer is
// /ingest/checkin's, body and status, so the two paths read alike.
func (s *Server) claimCheckin(w http.ResponseWriter, r *http.Request) {
	if s.desk == nil || s.claimToken == "" {
		s.fail(w, r, http.StatusServiceUnavailable, errCheckinOff, nil)
		return
	}
	// Before the body is read, as for the hub: a claim names a player, and
	// only Schmetterpause, which recorded the check-in, may make one. The
	// token is the one this service already sends to it, in the other
	// direction.
	if !bearerIs(r, s.claimToken) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="checkin"`)
		s.fail(w, r, http.StatusUnauthorized, errClaimToken, nil)
		return
	}

	var body claimRequest
	if err := decode(w, r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest, errors.New("decoding the body: not a claim"), nil)
		return
	}
	claim := checkin.PhoneClaim{
		Half: scorer.Half(body.Half), PlayerID: body.PlayerID, CheckinID: body.CheckinID,
	}
	if err := claim.Validate(); err != nil {
		s.fail(w, r, http.StatusBadRequest, err, nil)
		return
	}

	s.writeCheckinAnswer(w, s.desk.ClaimByPhone(r.Context(), claim, table{s}))
}

// writeCheckinAnswer writes what a check-in did, by either path, with the
// status its outcome maps to.
func (s *Server) writeCheckinAnswer(w http.ResponseWriter, a checkin.Answer) {
	out := checkinAnswer{
		Outcome: a.Outcome, Half: a.Half, Name: a.Name, MatchID: a.MatchID,
		Repeated: a.Repeated, Left: a.Halves[0], Right: a.Halves[1],
	}
	if a.Err != nil {
		out.Error = a.Err.Error()
	}

	status := http.StatusOK
	switch a.Outcome {
	case checkin.Unknown:
		status = http.StatusNotFound
	case checkin.Refused:
		status = http.StatusConflict
	case checkin.Unavailable:
		status = http.StatusServiceUnavailable
	case checkin.Failed:
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, out)
}

// bearerIs says whether r carries token as a bearer, compared in constant
// time so the answer's timing says nothing about how much of a guess was
// right. An empty token matches nothing.
func bearerIs(r *http.Request, token string) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func (s *Server) checkinStatus(w http.ResponseWriter, _ *http.Request) {
	if s.desk == nil {
		writeJSON(w, http.StatusOK, checkinStatus{})
		return
	}
	halves := s.desk.Claims()
	writeJSON(w, http.StatusOK, checkinStatus{
		Enabled: true, Fob: s.checkinToken != "", Phone: s.claimToken != "",
		Timeout: s.desk.Timeout().String(),
		Left:    halves[0], Right: halves[1],
	})
}

// table is the server as the desk sees it: whether a real match holds the
// table, and the one way a match starts.
type table struct{ s *Server }

func (t table) Occupied() bool {
	_, ok := t.s.registry.Occupied()
	return ok
}

func (t table) StartMatch(cfg scorer.Config, h match.Handover) (*match.Match, error) {
	// Not StartMatch: that tells the desk, which is the caller and holds its
	// lock.
	m, err := t.s.create(cfg, match.Setup{Handover: h})
	if err != nil {
		return nil, fmt.Errorf("starting a match: %w", err)
	}
	return m, nil
}

// compile-time check that the desk's view of the table holds.
var _ checkin.Table = table{}
