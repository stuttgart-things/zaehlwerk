// Package portal is one page for the whole chain on a laptop: whether each
// part is up, their own pages side by side in frames, and the controls that
// otherwise live in curl lines in the Taskfile.
//
// It owns nothing. Every button is a call to a part that already exists —
// zaehlwerk's API, the fake Schmetterpause's own routes, the piezo board's
// control endpoint, zaehlwerk-pong's game API — and the page is drawn from
// what they answer, never from anything kept here. Restarting it loses
// nothing; stopping it changes nothing about the chain.
//
// Each part has two addresses: the one the portal calls, and the one the
// browser is given to frame the part's page. On a laptop they are the same; in
// a cluster the first is a Service and the second a route.
package portal

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/live"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
	"github.com/stuttgart-things/zaehlwerk/tools/chain-mock/fakesp"
	"github.com/stuttgart-things/zaehlwerk/tools/chain-mock/piezo"
)

//go:embed page.html
var pageFS embed.FS

var page = template.Must(template.New("").Funcs(template.FuncMap{
	"clock": func(t time.Time) string { return t.Local().Format("15:04:05") },
	"sets": func(sets [][2]int) string {
		parts := make([]string, len(sets))
		for i, s := range sets {
			parts[i] = fmt.Sprintf("%d:%d", s[0], s[1])
		}
		return strings.Join(parts, " ")
	},
	"pct":       func(share float64) int { return int(share*100 + 0.5) },
	"modes":     func() []string { return []string{fakesp.ModeAccept, fakesp.ModeRefuse, fakesp.ModeHang} },
	"kindWidth": func() int { return kindWidth },
}).ParseFS(pageFS, "page.html"))

// logKinds is every kind the table stream sends with a state, which the event
// log puts in a column of its own: the scorer's transitions and the stream's
// own. no_match carries no state and is logged alone.
var logKinds = []string{
	string(scorer.TransitionPoint), string(scorer.TransitionSetWon), string(scorer.TransitionMatchWon),
	string(scorer.TransitionUndo), string(scorer.TransitionConfirmed), string(scorer.TransitionWarmupOver),
	string(scorer.TransitionServeDecided), string(scorer.TransitionEndsChanged),
	live.KindSnapshot, live.KindStarted, live.KindEnded,
}

// kindWidth is how wide the log pads a kind: the longest of logKinds, not a
// guess. padEnd(10) ran warmup_over into the player's name. The page still adds
// a space after it, so a kind added later and missing here is only out of line.
var kindWidth = len(slices.MaxFunc(logKinds, func(a, b string) int { return len(a) - len(b) }))

// probeTimeout bounds one status check. The page polls every few seconds, so
// a part that hangs must not hold the whole row.
const probeTimeout = 1500 * time.Millisecond

// Part is where one part of the chain is. URL is what the portal calls, Public
// what the browser is given for the part's page, and the same as URL when
// empty. A part with no URL is not there at all — no chip, no frame, no
// buttons — which is different from one that is there and down: a cluster
// whose table has real buttons has no table mock, and a red chip for it
// forever would teach everybody to ignore the red ones.
type Part struct {
	URL    string
	Public string
}

// On is whether the part is there.
func (p Part) On() bool { return p.URL != "" }

// Page is the part's address for a browser.
func (p Part) Page() string {
	if p.Public != "" {
		return p.Public
	}
	return p.URL
}

// Targets are the parts. zaehlwerk is the one that has to be there; it is
// what the page is about.
type Targets struct {
	Zaehlwerk      Part
	Simulator      Part
	Schmetterpause Part
	Buttons        Part
	Piezo          Part
	Pong           Part
}

// Server is the page. Use [New].
type Server struct {
	t      Targets
	client *http.Client
	// stream has no timeout: it carries zaehlwerk's table stream, which is
	// meant to stay open for as long as the page does.
	stream *http.Client
	log    *slog.Logger
	mux    *http.ServeMux
}

// New serves the portal for the parts at t.
func New(t Targets, log *slog.Logger) *Server {
	s := &Server{
		t:      t,
		client: &http.Client{Timeout: 6 * time.Second},
		stream: &http.Client{},
		log:    log,
		mux:    http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /{$}", s.index)
	s.mux.HandleFunc("GET /status", s.status)
	s.mux.HandleFunc("GET /side", s.side)
	s.mux.HandleFunc("GET /events", s.events)
	s.mux.HandleFunc("POST /match/start", s.startMatch)
	s.mux.HandleFunc("POST /match/end", s.endMatch)
	s.mux.HandleFunc("POST /sp/mode", s.setMode)
	s.mux.HandleFunc("POST /sp/phone", s.phone)
	s.mux.HandleFunc("POST /piezo", s.setPiezo)
	s.mux.HandleFunc("POST /pong/game", s.pongGame)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// ─── Status ──────────────────────────────────────────────────────────────

// SP kinds: which Schmetterpause is on the other end. The fake answers
// GET /mode, which the real one has no route for, and that is all it takes to
// tell them apart — so the same SCHMETTERPAUSE_URL works for either.
const (
	spFake = "fake"
	spReal = "real"
	spDown = "down"
	spOff  = "off"
)

// Probe is one part in the status row.
type Probe struct {
	Key    string
	Title  string
	URL    string
	Up     bool
	Detail string
}

func (s *Server) probes(ctx context.Context) []Probe {
	parts := []struct {
		key, title string
		part       Part
	}{
		{"zaehlwerk", "zaehlwerk", s.t.Zaehlwerk},
		{"panel", "Panel simulator", s.t.Simulator},
		{"schmetterpause", "Schmetterpause", s.t.Schmetterpause},
		{"buttons", "Buttons", s.t.Buttons},
		{"piezo", "Piezo", s.t.Piezo},
		{"pong", "Pong", s.t.Pong},
	}
	var ps []Probe
	var calls []string
	for _, p := range parts {
		if p.part.On() {
			ps = append(ps, Probe{Key: p.key, Title: p.title, URL: p.part.Page()})
			calls = append(calls, p.part.URL)
		}
	}
	var wg sync.WaitGroup
	for i := range ps {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			p, call := &ps[i], calls[i]
			switch p.Key {
			case "schmetterpause":
				kind, mode, err := s.spKind(ctx)
				p.Up = kind != spDown
				switch {
				case err != nil:
					p.Detail = err.Error()
				case kind == spFake:
					p.Detail = "fake, " + mode
				default:
					p.Detail = kind
				}
			case "panel":
				// The simulator is somebody else's image with no health route
				// this repository can count on; its page answering is enough.
				p.Up, p.Detail = s.ping(ctx, call+"/")
			default:
				p.Up, p.Detail = s.ping(ctx, call+"/healthz")
			}
		})
	}
	wg.Wait()
	return ps
}

func (s *Server) ping(ctx context.Context, u string) (bool, string) {
	start := time.Now()
	status, _, err := s.do(ctx, http.MethodGet, u, "", nil)
	switch {
	case err != nil:
		return false, short(err)
	case status >= 400:
		return false, fmt.Sprintf("answered %d", status)
	}
	return true, time.Since(start).Round(time.Millisecond).String()
}

// spKind finds out which Schmetterpause is at the URL, and the fake's mode.
func (s *Server) spKind(ctx context.Context) (kind, mode string, err error) {
	if !s.t.Schmetterpause.On() {
		return spOff, "", nil
	}
	status, body, err := s.do(ctx, http.MethodGet, s.t.Schmetterpause.URL+"/mode", "", nil)
	if err != nil {
		return spDown, "", errors.New(short(err))
	}
	var m struct {
		Mode string `json:"mode"`
	}
	if status == http.StatusOK && json.Unmarshal(body, &m) == nil && m.Mode != "" {
		return spFake, m.Mode, nil
	}
	if up, detail := s.ping(ctx, s.t.Schmetterpause.URL+"/readyz"); !up {
		return spDown, "", errors.New(detail)
	}
	return spReal, "", nil
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	s.render(w, "status", s.probes(r.Context()))
}

// ─── The page and its frames ─────────────────────────────────────────────

// Frame is one part's own page, embedded.
type Frame struct {
	Key   string
	Title string
	URL   string
	On    bool
}

// defaultFrames is what the page shows before anybody picks: the table as it
// is played — the panel, the scoring page, the buttons — and the real
// Schmetterpause when that is the one at the other end.
var defaultFrames = []string{"panel", "scoring", "buttons", "schmetterpause"}

func (s *Server) frames(show []string, realSP bool) []Frame {
	all := []struct {
		Frame
		on bool
	}{
		{Frame{Key: "panel", Title: "Panel simulator", URL: s.t.Simulator.Page() + "/"}, s.t.Simulator.On()},
		{Frame{Key: "scoring", Title: "Scoring page", URL: s.t.Zaehlwerk.Page() + "/ui"}, true},
		{Frame{Key: "watch", Title: "Watch screen", URL: s.t.Zaehlwerk.Page() + "/ui/watch"}, true},
		{Frame{Key: "buttons", Title: "Buttons", URL: s.t.Buttons.Page() + "/"}, s.t.Buttons.On()},
		{Frame{Key: "pong", Title: "Pong", URL: s.t.Pong.Page() + "/"}, s.t.Pong.On()},
		// The fake has no page of its own; its card on the side is the page.
		{Frame{Key: "schmetterpause", Title: "Schmetterpause", URL: s.t.Schmetterpause.Page() + "/"}, realSP},
	}
	var fs []Frame
	for _, f := range all {
		if f.on {
			f.On = slices.Contains(show, f.Key)
			fs = append(fs, f.Frame)
		}
	}
	return fs
}

type indexView struct {
	Frames []Frame
	// Shown is the frames that are on, in the order of Frames.
	Shown []Frame
	Probe []Probe
	Side  sideView
}

// index draws the whole page. Which frames are on is in the query
// (?show=panel&show=pong), so a layout is a bookmark and needs no storage.
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	show := r.URL.Query()["show"]
	if len(show) == 0 {
		show = defaultFrames
	}
	side := s.sideView(r.Context())
	v := indexView{Frames: s.frames(show, side.SP.Kind == spReal), Probe: s.probes(r.Context()), Side: side}
	for _, f := range v.Frames {
		if f.On {
			v.Shown = append(v.Shown, f)
		}
	}
	s.render(w, "page", v)
}

// ─── The side: match, Schmetterpause, piezo, pong ────────────────────────

type matchView struct {
	State scorer.State
	Demo  bool
}

type spView struct {
	Kind     string
	Mode     string
	Err      string
	Results  []fakesp.Result
	Checkins []fakesp.Checkin
	// Players are the fake's, for phone check-in. The real one's players
	// check in on its own page, in the frame.
	Players []fakesp.Person
}

type sideView struct {
	// Flash is what the last button did, and FlashBad whether it failed.
	Flash    string
	FlashBad bool

	Match    *matchView
	MatchErr string
	SP       spView
	Piezo    *piezo.Status
	PiezoErr string
	// PiezoOn and PongOn are whether those parts are there at all; without
	// them their cards are left off.
	PiezoOn bool
	PongOn  bool
}

func (s *Server) sideView(ctx context.Context) sideView {
	v := sideView{PiezoOn: s.t.Piezo.On(), PongOn: s.t.Pong.On()}
	var wg sync.WaitGroup

	wg.Go(func() {
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		var cur struct {
			scorer.State
			Demo bool `json:"demo"`
		}
		status, body, err := s.do(ctx, http.MethodGet, s.t.Zaehlwerk.URL+"/matches/current", "", nil)
		switch {
		case err != nil:
			v.MatchErr = short(err)
		case status == http.StatusNotFound:
		case status != http.StatusOK:
			v.MatchErr = fmt.Sprintf("answered %d", status)
		case json.Unmarshal(body, &cur) != nil:
			v.MatchErr = "not a match state"
		default:
			v.Match = &matchView{State: cur.State, Demo: cur.Demo}
		}
	})

	wg.Go(func() {
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		v.SP = s.spView(ctx)
	})

	wg.Go(func() {
		if !v.PiezoOn {
			return
		}
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		var st piezo.Status
		if err := s.getJSON(ctx, s.t.Piezo.URL+"/control", &st); err != nil {
			v.PiezoErr = err.Error()
			return
		}
		v.Piezo = &st
	})

	wg.Wait()
	return v
}

func (s *Server) spView(ctx context.Context) spView {
	kind, mode, err := s.spKind(ctx)
	v := spView{Kind: kind, Mode: mode}
	if err != nil {
		v.Err = err.Error()
	}
	if kind != spFake {
		return v
	}
	if err := s.getJSON(ctx, s.t.Schmetterpause.URL+"/results", &v.Results); err != nil {
		v.Err = err.Error()
	}
	if err := s.getJSON(ctx, s.t.Schmetterpause.URL+"/checkins", &v.Checkins); err != nil {
		v.Err = err.Error()
	}
	// Newest first: the one just played is the one being looked for.
	slices.Reverse(v.Results)
	slices.Reverse(v.Checkins)
	for _, p := range fakesp.DefaultRoster {
		if !p.Observer {
			v.Players = append(v.Players, p)
		}
	}
	return v
}

func (s *Server) side(w http.ResponseWriter, r *http.Request) {
	s.render(w, "side", s.sideView(r.Context()))
}

// answer draws the side again after a button, with what the button did on
// top. Failures are drawn too rather than answered with an error status:
// htmx swaps nothing on a 4xx, and then the press looks like it did nothing.
func (s *Server) answer(w http.ResponseWriter, r *http.Request, flash string, bad bool) {
	v := s.sideView(r.Context())
	v.Flash, v.FlashBad = flash, bad
	s.render(w, "side", v)
}

// call makes one call for a button and turns its outcome into the flash.
func (s *Server) call(w http.ResponseWriter, r *http.Request, what, method, u, contentType string, body []byte) {
	status, answer, err := s.do(r.Context(), method, u, contentType, body)
	switch {
	case err != nil:
		s.answer(w, r, what+": "+short(err), true)
	case status >= 400:
		s.answer(w, r, fmt.Sprintf("%s: %d %s", what, status, oneLine(answer)), true)
	default:
		s.answer(w, r, fmt.Sprintf("%s: %d %s", what, status, oneLine(answer)), false)
	}
}

// startMatch puts a match on the table in one click: Anna against Bernd, by
// name. Names are all POST /matches takes, so nobody is handed over and the
// result goes nowhere — the page says so. A reported match is the scoring
// page's, with players picked, or two check-ins'.
func (s *Server) startMatch(w http.ResponseWriter, r *http.Request) {
	bestOf := 3
	if r.PostFormValue("best_of") == "1" {
		bestOf = 1
	}
	body, _ := json.Marshal(map[string]any{"players": []string{"Anna", "Bernd"}, "best_of": bestOf})
	s.call(w, r, "start match", http.MethodPost, s.t.Zaehlwerk.URL+"/matches", "application/json", body)
}

func (s *Server) endMatch(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("match_id")
	if id == "" {
		s.answer(w, r, "end match: no match on the page", true)
		return
	}
	s.call(w, r, "end match", http.MethodPost, s.t.Zaehlwerk.URL+"/matches/"+url.PathEscape(id)+"/end", "", nil)
}

func (s *Server) setMode(w http.ResponseWriter, r *http.Request) {
	form := url.Values{"mode": {r.PostFormValue("mode")}}
	s.call(w, r, "schmetterpause mode", http.MethodPost, s.t.Schmetterpause.URL+"/mode",
		"application/x-www-form-urlencoded", []byte(form.Encode()))
}

// phone is task chain:phone: the fake records a phone check-in and claims the
// half at zaehlwerk. The answer shown is zaehlwerk's, passed through by the
// fake.
func (s *Server) phone(w http.ResponseWriter, r *http.Request) {
	form := url.Values{"half": {r.PostFormValue("half")}, "player_id": {r.PostFormValue("player_id")}}
	s.call(w, r, "phone check-in", http.MethodPost, s.t.Schmetterpause.URL+"/phone",
		"application/x-www-form-urlencoded", []byte(form.Encode()))
}

// setPiezo pauses or resumes the board. It takes a whole Control, so the
// current one is read first and only Paused is changed — the other knobs are
// the buttons page's.
func (s *Server) setPiezo(w http.ResponseWriter, r *http.Request) {
	var st piezo.Status
	if err := s.getJSON(r.Context(), s.t.Piezo.URL+"/control", &st); err != nil {
		s.answer(w, r, "piezo: "+err.Error(), true)
		return
	}
	next := st.Control
	next.Paused = r.PostFormValue("action") == "pause"
	body, _ := json.Marshal(next)
	s.call(w, r, "piezo", http.MethodPost, s.t.Piezo.URL+"/control", "application/json", body)
}

// pongGame is task chain:pong:play: a headless bot match on zaehlwerk-pong,
// which plays it into zaehlwerk as a demo.
func (s *Server) pongGame(w http.ResponseWriter, r *http.Request) {
	pace := r.PostFormValue("pace")
	if _, err := time.ParseDuration(pace); err != nil {
		s.answer(w, r, "pong: pace: "+err.Error(), true)
		return
	}
	body, _ := json.Marshal(map[string]string{"pace": pace})
	s.call(w, r, "pong game", http.MethodPost, s.t.Pong.URL+"/games", "application/json", body)
}

// ─── The event log ───────────────────────────────────────────────────────

// events relays zaehlwerk's table stream to the page, byte for byte. Through
// here it is same-origin, so zaehlwerk needs no ALLOWED_ORIGINS entry for the
// portal and the stream is the one every other consumer reads.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, s.t.Zaehlwerk.URL+"/live/stream", nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp, err := s.stream.Do(req)
	if err != nil {
		// EventSource retries on its own; a 502 makes it wait and try again
		// rather than give up, which is right while zaehlwerk is restarting.
		http.Error(w, short(err), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("zaehlwerk answered %d", resp.StatusCode), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	buf := make([]byte, 4<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

// ─── Plumbing ────────────────────────────────────────────────────────────

func (s *Server) render(w http.ResponseWriter, name string, v any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.ExecuteTemplate(w, name, v); err != nil {
		s.log.Error("rendering the page", "template", name, "error", err)
	}
}

func (s *Server) do(ctx context.Context, method, u, contentType string, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("building %s %s: %w", method, u, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("reading %s %s: %w", method, u, err)
	}
	return resp.StatusCode, raw, nil
}

func (s *Server) getJSON(ctx context.Context, u string, into any) error {
	status, body, err := s.do(ctx, http.MethodGet, u, "", nil)
	if err != nil {
		return errors.New(short(err))
	}
	if status != http.StatusOK {
		return fmt.Errorf("GET %s answered %d", u, status)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("GET %s: %w", u, err)
	}
	return nil
}

// short is an error as a status line has room for. A refused connection is
// the usual case — the part is not started — and its full text is mostly
// the address the row already shows.
func short(err error) string {
	switch msg := err.Error(); {
	case strings.Contains(msg, "connection refused"):
		return "not running"
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "Client.Timeout"):
		return "no answer"
	default:
		return msg
	}
}

// oneLine is a response body as one line of a flash.
func oneLine(b []byte) string {
	const limit = 200
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	return s
}
