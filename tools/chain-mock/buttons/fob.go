package buttons

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// FobWindow is how long the hub keeps a fob it read, waiting for the press
// that names the half. zaehlwerk-firmware#63 makes it a build flag with this
// default.
const FobWindow = 5 * time.Second

// Fob is one fob the page offers to hold to the reader. The UIDs are the fake
// Schmetterpause's (tools/chain-mock/fakesp), so the mock checks in against
// it without anybody typing a number.
type Fob struct {
	Label string
	UID   string
}

// Fobs are the ones on the page: the fake roster's three, and a card nobody
// owns, which is what a company badge held up by mistake looks like.
var Fobs = []Fob{
	{Label: "Anna's fob", UID: "04:A3:1B:22"},
	{Label: "Bernd's fob", UID: "04:5C:7E:91"},
	{Label: "Clara's fob", UID: "04:11:22:33:44:55:66"},
	{Label: "a stranger's badge", UID: "DE:AD:BE:EF"},
}

// heldFob is a fob the reader saw, waiting for a press. Guarded by Rig.mu.
type heldFob struct {
	uid   string
	label string
	timer *time.Timer
}

// HoldFob is a person holding a fob to the reader at the hub. The next short
// press within FobWindow names the half and is a check-in, not a point
// (zaehlwerk-firmware#63).
//
// Like the firmware, the mock never writes the UID anywhere: the log says
// which fob by the page's label, and the hub's line says only the outcome.
func (r *Rig) HoldFob(label string) error {
	var fob *Fob
	for i := range Fobs {
		if Fobs[i].Label == label {
			fob = &Fobs[i]
		}
	}
	if fob == nil {
		return fmt.Errorf("no fob called %q on the page", label)
	}

	r.mu.Lock()
	if r.fob != nil {
		r.fob.timer.Stop()
	}
	held := &heldFob{uid: fob.UID, label: fob.Label}
	held.timer = time.AfterFunc(FobWindow, func() {
		r.mu.Lock()
		mine := r.fob == held
		if mine {
			r.fob = nil
		}
		r.mu.Unlock()
		if mine {
			r.note("reader", fmt.Sprintf("no press within %s, %s dropped — checked in nowhere", FobWindow, held.label), false)
		}
	})
	r.fob = held
	r.mu.Unlock()

	r.note("reader", fmt.Sprintf("%s read; LED blinks: press your button within %s", fob.Label, FobWindow), false)
	return nil
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

	for attempt := range r.attempts() {
		status, a, err := r.postCheckin(ctx, body)
		if err != nil {
			r.note("api", err.Error(), true)
			return
		}
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
