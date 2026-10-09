// Package buttons is the table's two wireless buttons and the hub they report
// to, with a page to press them on.
//
// It exists to find out what the buttons have to do before anybody solders
// one. The firmware issues describe a button that wakes, sends and sleeps
// (zaehlwerk-firmware#2), gestures for correcting a point at the table (#56)
// and a hub that receives them (#3, #58). Written as a program, each of those
// decisions can be pressed and looked at: how long a long press is, what it
// costs to tell "both long" from "long", and which faults the chain survives.
//
// The layers are kept apart the way the hardware keeps them apart, and every
// step is logged under the layer that took it:
//
//	button  decides the gesture from how long it was held, numbers the frame
//	radio   ESP-NOW: a frame arrives, or arrives twice when its ACK was lost
//	hub     drops repeated frames, pairs two long presses, maps a half to a
//	        player, and calls zaehlwerk
//	api     what zaehlwerk answered
//
// Nothing here is the firmware. Where the issues leave a decision open, this
// picks one and says so in the log, so the choice is visible rather than
// buried.
package buttons

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// Gesture is what a button decided a press was.
type Gesture string

const (
	// Short is a point for the half the button is on.
	Short Gesture = "short"
	// Long takes the last point back.
	Long Gesture = "long"
	// BothLong is both buttons held. No single button can see it, so the hub
	// makes it out of two long presses close together.
	BothLong Gesture = "both_long"
)

// Frame is the ESP-NOW payload, in the fields zaehlwerk-firmware#58 lists for
// lib/protocol. Shown on the page as JSON so the payload can be argued about
// before it is a packed struct.
type Frame struct {
	Kind      string  `json:"kind"`
	SourceID  string  `json:"source_id"`
	Side      string  `json:"side"`
	Gesture   Gesture `json:"gesture"`
	EventID   uint64  `json:"event_id"`
	BatteryMV int     `json:"battery_mv"`
	FWVersion string  `json:"fw_version"`
}

// Settings are what the page lets a person change while pressing.
type Settings struct {
	// LongPress is the hold after which a press is a long one.
	LongPress time.Duration
	// BothWindow is how long the hub waits after one long press for the
	// other side's. Every undo is late by this much, because until it is over
	// the hub cannot know the long press was not half of a both-long.
	BothWindow time.Duration

	// Debounce is the button's lockout after a press. Off, a bouncing contact
	// wakes it twice and it sends two presses with two ids, which no
	// deduplication further down can tell from two real presses.
	Debounce bool
	// RadioAckLost makes every frame arrive twice, as a button resends one it
	// saw no ACK for. Same event id both times.
	RadioAckLost bool
	// HubDedup is the hub dropping a frame whose id it has already seen from
	// that source (zaehlwerk-firmware#58).
	HubDedup bool
	// APIResponseLost makes the hub send every call to zaehlwerk twice, as it
	// would after a timeout. A point is safe because ingest is idempotent; an
	// undo is not, and this is what shows it.
	APIResponseLost bool
	// BothLongEndsMatch decides what both-long does. Ending is the only door
	// the API has that comes close to "new game", so it is off unless asked
	// for: a match ended by accident at the table cannot be resumed.
	BothLongEndsMatch bool
}

// DefaultSettings is a button that works, on a radio that does.
func DefaultSettings() Settings {
	return Settings{
		LongPress:  time.Second,
		BothWindow: 400 * time.Millisecond,
		Debounce:   true,
		HubDedup:   true,
	}
}

// Entry is one line of the log on the page.
type Entry struct {
	At    time.Time
	Layer string
	Text  string
	// Bad marks the line a person should look at: something the chain got
	// wrong, not something that went wrong on purpose.
	Bad bool
}

const maxEntries = 120

// button is one of the two. Its counter only goes up; the firmware keeps it in
// RTC memory across deep sleep (zaehlwerk-firmware#2). The mock has none and
// starts from the clock, as the piezo mock does, so a restart cannot send it
// backwards into ids zaehlwerk has already seen.
type button struct {
	side    string
	source  string
	eventID uint64
}

// Rig is the two buttons and the hub. Use [New].
type Rig struct {
	api  string
	http *http.Client
	log  *slog.Logger

	mu       sync.Mutex
	settings Settings
	buttons  map[string]*button
	entries  []Entry

	// hubMu serialises the hub. A real hub drains one queue, so two presses
	// never reach zaehlwerk side by side; holding this across the HTTP call is
	// what gives the mock the same order.
	hubMu sync.Mutex
	// seen is the hub's watermark per source, for HubDedup.
	seen map[string]uint64
	// pending is a long press waiting out BothWindow.
	pending *pendingLong
}

type pendingLong struct {
	frame Frame
	timer *time.Timer
}

// New builds the rig against the zaehlwerk at api.
func New(api, sourcePrefix string, settings Settings, log *slog.Logger) *Rig {
	start := uint64(time.Now().UnixMilli())
	return &Rig{
		api:      api,
		http:     &http.Client{Timeout: 5 * time.Second},
		log:      log,
		settings: settings,
		buttons: map[string]*button{
			"A": {side: "A", source: sourcePrefix + "-a", eventID: start},
			"B": {side: "B", source: sourcePrefix + "-b", eventID: start},
		},
		seen: map[string]uint64{},
	}
}

// Settings returns the current settings.
func (r *Rig) Settings() Settings {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settings
}

// SetSettings replaces them.
func (r *Rig) SetSettings(s Settings) {
	r.mu.Lock()
	r.settings = s
	r.mu.Unlock()
	r.note("page", fmt.Sprintf("settings: long press %s, both window %s, debounce %t, "+
		"radio ack lost %t, hub dedup %t, api response lost %t, both long ends match %t",
		s.LongPress, s.BothWindow, s.Debounce, s.RadioAckLost, s.HubDedup,
		s.APIResponseLost, s.BothLongEndsMatch), false)
}

// Entries returns the log, newest first.
func (r *Rig) Entries() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Entry, len(r.entries))
	for i, e := range r.entries {
		out[len(r.entries)-1-i] = e
	}
	return out
}

// ClearLog empties it.
func (r *Rig) ClearLog() {
	r.mu.Lock()
	r.entries = nil
	r.mu.Unlock()
}

func (r *Rig) note(layer, text string, bad bool) {
	r.mu.Lock()
	r.entries = append(r.entries, Entry{At: time.Now(), Layer: layer, Text: text, Bad: bad})
	if len(r.entries) > maxEntries {
		r.entries = r.entries[len(r.entries)-maxEntries:]
	}
	r.mu.Unlock()

	if bad {
		r.log.Warn(text, "layer", layer)
	} else {
		r.log.Info(text, "layer", layer)
	}
}

// Press is a person pressing the button on side for held.
func (r *Rig) Press(ctx context.Context, side string, held time.Duration) error {
	frames, err := r.press(side, held)
	if err != nil {
		return err
	}
	for _, f := range frames {
		r.receive(ctx, f)
	}
	return nil
}

// PressBoth is both buttons held for held and let go together: two long
// presses, which the hub has to pair.
func (r *Rig) PressBoth(ctx context.Context, held time.Duration) error {
	a, err := r.press("A", held)
	if err != nil {
		return err
	}
	b, err := r.press("B", held)
	if err != nil {
		return err
	}
	for _, f := range append(a, b...) {
		r.receive(ctx, f)
	}
	return nil
}

// press is the button's half: decide, number, and put on the air. It returns
// the frames as the hub will receive them, duplicates included.
func (r *Rig) press(side string, held time.Duration) ([]Frame, error) {
	r.mu.Lock()
	b, ok := r.buttons[side]
	s := r.settings
	if !ok {
		r.mu.Unlock()
		return nil, fmt.Errorf("no button on side %q", side)
	}

	// Decided on the button, not on the hub: the button is the only one that
	// saw the press start and end, and a gesture made of two timestamps sent
	// over a radio that loses frames would be guessed at the far end.
	g := Short
	if held >= s.LongPress {
		g = Long
	}

	wakes := 1
	if !s.Debounce {
		wakes = 2
	}
	var frames []Frame
	for range wakes {
		b.eventID++
		frames = append(frames, Frame{
			Kind: "button", SourceID: b.source, Side: side, Gesture: g,
			EventID: b.eventID, BatteryMV: 3950, FWVersion: "mock",
		})
	}
	r.mu.Unlock()

	r.note("button "+side, fmt.Sprintf("held %d ms → %s (long from %d ms)",
		held.Milliseconds(), g, s.LongPress.Milliseconds()), false)
	if !s.Debounce {
		r.note("button "+side, fmt.Sprintf("contact bounced and nothing locked it out: woke twice, "+
			"sent event %d and %d — two presses as far as anybody downstream can tell",
			frames[0].EventID, frames[1].EventID), true)
	}

	var out []Frame
	for _, f := range frames {
		out = append(out, f)
		if s.RadioAckLost {
			// The button resends what it saw no ACK for, with the same id.
			out = append(out, f)
		}
	}
	return out, nil
}

// receive is the hub taking one frame off the radio.
func (r *Rig) receive(ctx context.Context, f Frame) {
	raw, _ := json.Marshal(f)
	r.note("radio", string(raw), false)

	r.hubMu.Lock()
	defer r.hubMu.Unlock()

	s := r.Settings()
	if s.HubDedup {
		if f.EventID <= r.seen[f.SourceID] {
			r.note("hub", fmt.Sprintf("%s event %d already seen, dropped", f.SourceID, f.EventID), false)
			return
		}
		r.seen[f.SourceID] = f.EventID
	}

	switch f.Gesture {
	case Short:
		r.point(ctx, f)
	case Long:
		r.long(ctx, f, s.BothWindow)
	default:
		r.note("hub", fmt.Sprintf("gesture %q is not one a button sends, dropped", f.Gesture), true)
	}
}

// long holds a long press back for the window, because it may be half of a
// both-long. Must be called with hubMu held.
func (r *Rig) long(ctx context.Context, f Frame, window time.Duration) {
	if p := r.pending; p != nil {
		if p.frame.Side != f.Side && p.timer.Stop() {
			r.pending = nil
			r.note("hub", fmt.Sprintf("long on %s and %s inside %d ms → both_long",
				p.frame.Side, f.Side, window.Milliseconds()), false)
			r.bothLong(ctx)
			return
		}
		if p.frame.Side == f.Side && p.frame.SourceID == f.SourceID && p.frame.EventID == f.EventID {
			// The same press again: a resend the hub did not deduplicate.
			// It must not start a second wait, or it becomes a second undo.
			r.note("hub", fmt.Sprintf("%s event %d again while it waits — without hub dedup "+
				"this is a second undo", f.SourceID, f.EventID), true)
		}
	}

	r.note("hub", fmt.Sprintf("long on %s: waiting %d ms for the other side before it is an undo",
		f.Side, window.Milliseconds()), false)

	p := &pendingLong{frame: f}
	// The undo runs after the request that pressed the button has returned,
	// so it cannot use that request's context.
	p.timer = time.AfterFunc(window, func() {
		r.hubMu.Lock()
		defer r.hubMu.Unlock()
		if r.pending == p {
			r.pending = nil
		}
		r.undo(context.WithoutCancel(ctx), p.frame)
	})
	r.pending = p
}

// ingestBody is POST /ingest/button: a batch, with no match id, because a
// button has nowhere to get one from and the API resolves the running match.
type ingestBody struct {
	Events []ingestEvent `json:"events"`
}

type ingestEvent struct {
	Source  string `json:"source"`
	Player  string `json:"player"`
	Delta   int    `json:"delta"`
	EventID uint64 `json:"event_id"`
}

func (r *Rig) point(ctx context.Context, f Frame) {
	cur, status, err := r.call(ctx, http.MethodGet, "/matches/current", nil)
	if err != nil {
		r.note("api", err.Error(), true)
		return
	}
	if status != http.StatusOK {
		r.note("api", "no match running — start one on the zaehlwerk page; the press goes nowhere", true)
		return
	}

	// The ingest contract takes a player, a button only knows its half. Who
	// stands on which half is the board's to know (zaehlwerk-firmware
	// ADR-0006); a standalone hub has nobody to ask, which #3 leaves open.
	// The mock asks the API for the set and swaps at every set boundary.
	player := playerOn(f.Side, cur.SetNumber)
	r.note("hub", fmt.Sprintf("half %s is %s in set %d (%s)", f.Side, player, cur.SetNumber,
		cur.Players[index(player)]), false)

	body := ingestBody{Events: []ingestEvent{{
		Source: f.SourceID, Player: string(player), Delta: 1, EventID: f.EventID,
	}}}
	before := cur
	for attempt := range r.attempts() {
		st, status, err := r.call(ctx, http.MethodPost, "/ingest/button", body)
		if err != nil {
			r.note("api", err.Error(), true)
			return
		}
		label := "POST /ingest/button"
		if attempt > 0 {
			label += " (retry, the first answer was lost)"
		}
		changed := st.Points != before.Points || st.Sets != before.Sets
		bad := attempt > 0 && changed
		r.note("api", fmt.Sprintf("%s event %d → %d, %s", label, f.EventID, status, score(st)), bad)
		if attempt > 0 && !changed {
			r.note("api", "the retry changed nothing: ingest discarded it as a duplicate", false)
		}
		before = st
	}
}

func (r *Rig) undo(ctx context.Context, f Frame) {
	cur, status, err := r.call(ctx, http.MethodGet, "/matches/current", nil)
	if err != nil {
		r.note("api", err.Error(), true)
		return
	}
	if status != http.StatusOK {
		r.note("api", "no match running, nothing to take back", true)
		return
	}

	before := cur
	for attempt := range r.attempts() {
		// There is no undo in the ingest contract. The only door is this
		// one, and it carries no event id, so the API cannot tell a retry
		// from a second correction.
		st, status, err := r.call(ctx, http.MethodPost, "/matches/"+cur.MatchID+"/undo", nil)
		if err != nil {
			r.note("api", err.Error(), true)
			return
		}
		label := "POST /matches/" + cur.MatchID + "/undo"
		if attempt > 0 {
			label += " (retry, the first answer was lost)"
		}
		changed := status == http.StatusOK && (st.Points != before.Points || st.Sets != before.Sets)
		bad := attempt > 0 && changed
		r.note("api", fmt.Sprintf("%s for %s event %d → %d, %s", label, f.SourceID, f.EventID,
			status, score(st)), bad || status != http.StatusOK)
		if bad {
			r.note("api", "the retry took back a second point: undo has no event id to deduplicate on", true)
		}
		before = st
	}
}

func (r *Rig) bothLong(ctx context.Context) {
	if !r.Settings().BothLongEndsMatch {
		r.note("hub", "both_long has no API call behind it yet — a new match is started on the page, "+
			"and ending this one is switched off on the page", false)
		return
	}
	cur, status, err := r.call(ctx, http.MethodGet, "/matches/current", nil)
	if err != nil {
		r.note("api", err.Error(), true)
		return
	}
	if status != http.StatusOK {
		r.note("api", "no match running, nothing to end", false)
		return
	}
	st, status, err := r.call(ctx, http.MethodPost, "/matches/"+cur.MatchID+"/end", nil)
	if err != nil {
		r.note("api", err.Error(), true)
		return
	}
	r.note("api", fmt.Sprintf("POST /matches/%s/end → %d, ended at %s", cur.MatchID, status, score(st)),
		status != http.StatusOK)
}

// attempts is how often the hub sends one call: twice when its first answer
// is lost.
func (r *Rig) attempts() int {
	if r.Settings().APIResponseLost {
		return 2
	}
	return 1
}

// Current is the running match, for the page; ok is false when there is none.
func (r *Rig) Current(ctx context.Context) (st scorer.State, ok bool, err error) {
	st, status, err := r.call(ctx, http.MethodGet, "/matches/current", nil)
	return st, status == http.StatusOK, err
}

// call sends a request and decodes the state, from the body or from the one an
// error body carries. The status is the caller's to judge.
func (r *Rig) call(ctx context.Context, method, path string, body any) (scorer.State, int, error) {
	var st scorer.State

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return st, 0, fmt.Errorf("encoding %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.api+path, reader)
	if err != nil {
		return st, 0, fmt.Errorf("building %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := r.http.Do(req)
	if err != nil {
		return st, 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return st, resp.StatusCode, fmt.Errorf("reading %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, &st); err != nil {
			return st, resp.StatusCode, fmt.Errorf("decoding %s %s: %w", method, path, err)
		}
		return st, resp.StatusCode, nil
	}

	var refusal struct {
		State *scorer.State `json:"state"`
	}
	if json.Unmarshal(raw, &refusal) == nil && refusal.State != nil {
		st = *refusal.State
	}
	return st, resp.StatusCode, nil
}

// playerOn resolves a table half to the player standing there, as the piezo
// mock does: A starts on half A and the two change ends every set. The change
// at five points in a deciding set is left out, as it is there.
func playerOn(half string, setNumber int) scorer.Player {
	aOnHalfA := setNumber%2 == 1
	if (half == "A") == aOnHalfA {
		return scorer.PlayerA
	}
	return scorer.PlayerB
}

func index(p scorer.Player) int {
	if p == scorer.PlayerB {
		return 1
	}
	return 0
}

func score(st scorer.State) string {
	return fmt.Sprintf("%d:%d in set %d, sets %d:%d", st.Points[0], st.Points[1],
		st.SetNumber, st.Sets[0], st.Sets[1])
}
