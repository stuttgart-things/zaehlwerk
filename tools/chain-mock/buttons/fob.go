package buttons

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
	"github.com/stuttgart-things/zaehlwerk/tools/chain-mock/fobuid"
)

// FobWindow is how long the hub keeps a fob it read, waiting for the press
// that names the half. zaehlwerk-firmware#63 makes it a build flag with this
// default.
const FobWindow = 5 * time.Second

// Fob is one fob the page offers to hold to the reader.
type Fob struct {
	// Key names the fob in a form: a player id, or one of the spare ones.
	// Never the UID, which stays out of the page as it stays out of the log.
	Key   string
	Label string
	UID   string
}

// The fobs that belong to nobody: a new one, the kind a player links in
// "Anhänger koppeln", and a card nobody owns, which is what a company badge
// held up by mistake looks like. Neither is known to any Schmetterpause until
// somebody links it.
var (
	SpareFob    = Fob{Key: "spare", Label: "a new fob", UID: "04:5E:7A:4E:00:00:01"}
	StrangerFob = Fob{Key: "stranger", Label: "a stranger's badge", UID: "DE:AD:BE:EF"}
)

// FobsFor is the fobs the page offers: one for every player on the roster,
// then the two that belong to nobody.
//
// The fob for a player is fobuid.For(id). The fake Schmetterpause has the same
// one on its roster, so there it is the player's at once. A real one keeps
// only an HMAC of a UID and cannot say whose a fob is: there the fob for a
// player is unknown until that player links it, and what the page shows is
// whatever the check-in answered, never the label.
func FobsFor(players []schmetterpause.Player) []Fob {
	fobs := make([]Fob, 0, len(players)+2)
	for _, p := range players {
		fobs = append(fobs, Fob{Key: p.ID, Label: p.DisplayName + "'s fob", UID: fobuid.For(p.ID)})
	}
	return append(fobs, SpareFob, StrangerFob)
}

// heldFob is a fob the reader saw, waiting for a press. Guarded by Rig.mu.
type heldFob struct {
	key   string
	uid   string
	label string
	until time.Time
	timer *time.Timer
}

// FobAnswer is what the last check-in with a fob said about it: whose it is,
// or that Schmetterpause does not know it. It is the only place the page
// learns who a fob belongs to.
type FobAnswer struct {
	// Owner is the name zaehlwerk answered with, empty when it had none.
	Owner string
	// Outcome is zaehlwerk's word for the check-in.
	Outcome string
}

// readerShows is how long the reader's LED keeps the last answer up.
const readerShows = 4 * time.Second

// readerResult is the last thing the reader's LED said. Guarded by Rig.mu.
type readerResult struct {
	label string
	text  string
	ok    bool
	at    time.Time
}

// Reader is the reader at the hub as the page draws it: its LED, and how long
// a fob it read still waits for a press.
type Reader struct {
	// Waiting is a fob read and waiting for a press, Label which, and Left
	// how long it still waits.
	Waiting bool
	Label   string
	Left    time.Duration
	// Sending is a check-in on its way to zaehlwerk: both LEDs on.
	Sending bool
	// Result is the last answer while it is still shown, OK whether the LED
	// was green.
	Result string
	OK     bool
}

// HoldFob is a person holding fob to the reader at the hub. The next short
// press within FobWindow names the half and is a check-in, not a point
// (zaehlwerk-firmware#63).
//
// Like the firmware, the mock never writes the UID anywhere: the log says
// which fob by the page's label, and the hub's line says only the outcome.
func (r *Rig) HoldFob(fob Fob) {
	r.mu.Lock()
	if r.fob != nil {
		r.fob.timer.Stop()
	}
	held := &heldFob{key: fob.Key, uid: fob.UID, label: fob.Label, until: time.Now().Add(FobWindow)}
	held.timer = time.AfterFunc(FobWindow, func() {
		r.mu.Lock()
		mine := r.fob == held
		if mine {
			r.fob = nil
			r.result = readerResult{label: held.label, text: "no press in time, checked in nowhere", at: time.Now()}
		}
		r.mu.Unlock()
		if mine {
			r.note("reader", fmt.Sprintf("no press within %s, %s dropped — checked in nowhere", FobWindow, held.label), false)
		}
	})
	r.fob = held
	r.result = readerResult{}
	r.mu.Unlock()

	r.note("reader", fmt.Sprintf("%s read; LED blinks: press your button within %s", fob.Label, FobWindow), false)
}

// FobHeld is the label of the fob waiting for a press, or empty.
func (r *Rig) FobHeld() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fob == nil {
		return ""
	}
	return r.fob.label
}

// Reader is the reader's state now.
func (r *Rig) Reader() Reader {
	r.mu.Lock()
	defer r.mu.Unlock()
	var v Reader
	if r.fob != nil {
		v.Waiting, v.Label, v.Left = true, r.fob.label, max(time.Until(r.fob.until), 0)
	}
	v.Sending = r.sending
	if !r.result.at.IsZero() && time.Since(r.result.at) < readerShows {
		v.Result, v.OK = r.result.label+": "+r.result.text, r.result.ok
		if v.Label == "" {
			v.Label = r.result.label
		}
	}
	return v
}

// FobAnswers is what the last check-in with each fob said, by Fob.Key.
func (r *Rig) FobAnswers() map[string]FobAnswer {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]FobAnswer, len(r.answers))
	for k, v := range r.answers {
		out[k] = v
	}
	return out
}

// takeFob hands the waiting fob to the press that names its half, once.
func (r *Rig) takeFob() *heldFob {
	r.mu.Lock()
	defer r.mu.Unlock()
	held := r.fob
	if held != nil {
		held.timer.Stop()
		r.fob = nil
	}
	return held
}

// checkinBody is POST /ingest/checkin (ADR-0010).
type checkinBody struct {
	Source  string `json:"source"`
	EventID uint64 `json:"event_id"`
	Half    string `json:"half"`
	UID     string `json:"uid"`
}

type checkinAnswer struct {
	Outcome  string `json:"outcome"`
	Name     string `json:"name"`
	MatchID  string `json:"match_id"`
	Error    string `json:"error"`
	Repeated bool   `json:"repeated"`
}

// checkIn is the hub sending a fob and the half its press came from, once,
// under the hub's own source and counter. Must be called with hubMu held.
func (r *Rig) checkIn(ctx context.Context, f Frame, held *heldFob) {
	half := "left"
	if f.Side == "B" {
		half = "right"
	}
	r.mu.Lock()
	r.hubEvent++
	body := checkinBody{Source: r.hubSource, EventID: r.hubEvent, Half: half, UID: held.uid}
	r.mu.Unlock()

	r.note("hub", fmt.Sprintf("press on %s within the window: a check-in at the %s half with %s, not a point",
		f.Side, half, held.label), false)

	r.setSending(true)
	defer r.setSending(false)

	for attempt := range r.attempts() {
		status, a, err := r.postCheckin(ctx, body)
		if err != nil {
			r.note("api", err.Error(), true)
			r.answered(held, checkinAnswer{}, false, "zaehlwerk did not answer")
			return
		}
		r.answered(held, a, status == http.StatusOK && checkedIn(a.Outcome), describe(a))
		label := fmt.Sprintf("POST /ingest/checkin event %d", body.EventID)
		if attempt > 0 {
			label += " (retry, the first answer was lost)"
		}
		r.note("api", fmt.Sprintf("%s → %d, %s", label, status, describe(a)), status >= 500)
		if attempt > 0 && a.Repeated {
			r.note("api", "the retry got the first answer again and resolved nothing: ingest saw the event id", false)
		}
	}
}

func (r *Rig) setSending(on bool) {
	r.mu.Lock()
	r.sending = on
	r.mu.Unlock()
}

// answered puts an answer on the reader's LED and remembers what it said
// about the fob.
func (r *Rig) answered(held *heldFob, a checkinAnswer, ok bool, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.result = readerResult{label: held.label, text: text, ok: ok, at: time.Now()}
	switch {
	case a.Name != "":
		r.answers[held.key] = FobAnswer{Owner: a.Name, Outcome: a.Outcome}
	case a.Outcome != "":
		r.answers[held.key] = FobAnswer{Outcome: a.Outcome}
	}
}

// checkedIn is whether an outcome put somebody on a half: the LED is green.
func checkedIn(outcome string) bool {
	switch outcome {
	case "claimed", "unchanged", "replaced", "moved", "started":
		return true
	}
	return false
}

// describe is the hub's LED, in words.
func describe(a checkinAnswer) string {
	switch a.Outcome {
	case "claimed", "unchanged", "replaced", "moved":
		return fmt.Sprintf("LED green: %s checked in (%s)", a.Name, a.Outcome)
	case "started":
		return fmt.Sprintf("LED green: %s checked in, both halves taken, match %s started", a.Name, a.MatchID)
	case "unknown":
		return "LED red: unknown fob, nothing stored"
	case "refused":
		return "LED red: a match is running, " + a.Error
	case "":
		return "no answer the hub understands: " + a.Error
	default:
		return fmt.Sprintf("LED red: %s, %s", a.Outcome, a.Error)
	}
}

func (r *Rig) postCheckin(ctx context.Context, body checkinBody) (int, checkinAnswer, error) {
	var a checkinAnswer
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, a, fmt.Errorf("encoding the check-in: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.api+"/ingest/checkin", bytes.NewReader(raw))
	if err != nil {
		return 0, a, fmt.Errorf("building the check-in: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if r.checkinToken != "" {
		req.Header.Set("Authorization", "Bearer "+r.checkinToken)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return 0, a, fmt.Errorf("POST /ingest/checkin: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&a); err != nil {
		return resp.StatusCode, a, fmt.Errorf("decoding the check-in answer: %w", err)
	}
	return resp.StatusCode, a, nil
}
