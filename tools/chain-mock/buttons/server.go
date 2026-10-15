package buttons

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
	"github.com/stuttgart-things/zaehlwerk/tools/chain-mock/piezo"
)

//go:embed page.html
var pageFS embed.FS

var page = template.Must(template.New("").Funcs(template.FuncMap{
	"ms":    func(d time.Duration) int64 { return d.Milliseconds() },
	"clock": func(t time.Time) string { return t.Format("15:04:05.000") },
	"name":  func(st scorer.State, half string) string { return st.Players[index(playerOn(half, st.SetNumber))] },
	"pct":   func(share float64) int { return int(share*100 + 0.5) },
	"ago":   func(t time.Time) string { return time.Since(t).Round(time.Second).String() },
	"share": func(left time.Duration) int { return int(100 * left / FobWindow) },
	"sides": func() []string { return []string{"A", "B"} },
}).ParseFS(pageFS, "page.html"))

// maxHold caps a press posted by the page. Longer than anybody holds a button,
// short enough that a stuck pointer does not read as a gesture.
const maxHold = 10 * time.Second

// Roster is where the page gets the players it offers a fob for: the
// Schmetterpause zaehlwerk reports to.
type Roster interface {
	Players(ctx context.Context) ([]schmetterpause.Player, error)
}

// Server is the page. Use [NewServer].
type Server struct {
	rig *Rig
	// zaehlwerkUI is where the page links to start a match. Not the API URL
	// the rig calls: in a pod that one is a cluster address a browser cannot
	// reach.
	zaehlwerkUI string
	// piezo is nil when PIEZO_CONTROL_URL is unset.
	piezo *piezoClient
	// roster is nil when SCHMETTERPAUSE_URL is unset: the page then offers
	// only the fobs that belong to nobody.
	roster Roster
	log    *slog.Logger
	mux    *http.ServeMux

	// fobs is the list the page was last drawn with, and fobsErr why it has
	// no players in it. A press on a fob is looked up here, so the fob held is
	// the one that was on the page, not one from a list fetched since.
	fobsMu  sync.Mutex
	fobs    []Fob
	fobsErr string
}

// WithRoster gives the page the players to offer a fob for.
func (s *Server) WithRoster(roster Roster) *Server {
	s.roster = roster
	return s
}

// NewServer serves the page. piezoURL is a piezo board's control endpoint;
// empty leaves the piezo section off the page.
func NewServer(rig *Rig, zaehlwerkUI, piezoURL string, log *slog.Logger) *Server {
	s := &Server{rig: rig, zaehlwerkUI: zaehlwerkUI, piezo: newPiezoClient(piezoURL), log: log, mux: http.NewServeMux(),
		fobs: FobsFor(nil)}
	s.mux.HandleFunc("GET /{$}", s.index)
	s.mux.HandleFunc("GET /live", s.live)
	s.mux.HandleFunc("POST /press", s.press)
	s.mux.HandleFunc("POST /press-both", s.pressBoth)
	s.mux.HandleFunc("POST /tap", s.tap)
	s.mux.HandleFunc("GET /screens", s.screens)
	s.mux.HandleFunc("POST /settings", s.settings)
	s.mux.HandleFunc("POST /clear", s.clear)
	s.mux.HandleFunc("POST /fob", s.holdFob)
	s.mux.HandleFunc("GET /reader", s.reader)
	s.mux.HandleFunc("POST /piezo", s.setPiezo)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

type view struct {
	Settings Settings
	// Acks is what each half's display was last told, keyed "A" and "B".
	Acks        map[string]Ack
	Running     bool
	State       scorer.State
	APIError    string
	Entries     []Entry
	ZaehlwerkUI string
	Sources     [2]string
	// Fobs are the ones the page can hold to the reader, and FobsError why
	// none of them is a player's.
	Fobs      []Fob
	FobsError string
	Reader    readerView

	// PiezoOn is whether the page has a piezo section at all. Piezo is nil
	// when the board did not answer, and PiezoError says why.
	PiezoOn    bool
	Piezo      *piezo.Status
	PiezoError string
}

func (s *Server) view(ctx context.Context) view {
	v := view{
		Settings:    s.rig.Settings(),
		Acks:        s.rig.Acks(),
		Entries:     s.rig.Entries(),
		ZaehlwerkUI: s.zaehlwerkUI,
		Sources:     [2]string{s.rig.buttons["A"].source, s.rig.buttons["B"].source},
	}
	s.fobsMu.Lock()
	v.Fobs, v.FobsError = s.fobs, s.fobsErr
	s.fobsMu.Unlock()
	v.Reader = s.readerView()
	st, ok, err := s.rig.Current(ctx)
	if err != nil {
		v.APIError = err.Error()
	}
	v.Running, v.State = ok, st

	if s.piezo != nil {
		v.PiezoOn = true
		if ps, err := s.piezo.status(ctx); err != nil {
			v.PiezoError = err.Error()
		} else {
			v.Piezo = &ps
		}
	}
	return v
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.ExecuteTemplate(w, name, s.view(r.Context())); err != nil {
		s.log.Error("rendering the page", "template", name, "error", err)
	}
}

// index draws the whole page, with the roster as it is now.
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	s.refreshFobs(r.Context())
	s.render(w, r, "page")
}

// refreshFobs asks the roster for the players again. On an error the page
// keeps the fobs that belong to nobody and says why there are no others.
func (s *Server) refreshFobs(ctx context.Context) {
	if s.roster == nil {
		s.setFobs(FobsFor(nil), "no player list: SCHMETTERPAUSE_URL is unset")
		return
	}
	players, err := s.roster.Players(ctx)
	if err != nil {
		s.log.Warn("fetching the players for the fobs", "error", err)
		s.setFobs(FobsFor(nil), "no player list: "+err.Error())
		return
	}
	s.setFobs(FobsFor(players), "")
}

func (s *Server) setFobs(fobs []Fob, why string) {
	s.fobsMu.Lock()
	s.fobs, s.fobsErr = fobs, why
	s.fobsMu.Unlock()
}

// fob is the fob with key from the list the page was drawn with.
func (s *Server) fob(key string) (Fob, bool) {
	s.fobsMu.Lock()
	defer s.fobsMu.Unlock()
	for _, f := range s.fobs {
		if f.Key == key {
			return f, true
		}
	}
	return Fob{}, false
}

// fobLine is one fob in the reader's list: its label and what the last
// check-in with it said.
type fobLine struct {
	Label  string
	Status string
	// Known is a fob Schmetterpause named an owner for, Unknown one it said
	// it does not know. Neither is a fob nobody tried yet.
	Known, Unknown bool
}

// readerView is the reader partial: the LED, the countdown and the list.
type readerView struct {
	Reader
	Fobs []fobLine
}

func (s *Server) readerView() readerView {
	v := readerView{Reader: s.rig.Reader()}
	answers := s.rig.FobAnswers()
	s.fobsMu.Lock()
	fobs := s.fobs
	s.fobsMu.Unlock()
	for _, f := range fobs {
		line := fobLine{Label: f.Label, Status: "not tried yet"}
		if a, ok := answers[f.Key]; ok {
			switch {
			case a.Owner != "":
				line.Status, line.Known = "Schmetterpause: "+a.Owner+"'s", true
			case a.Outcome == "unknown":
				line.Status, line.Unknown = "Schmetterpause: unknown", true
			default:
				line.Status = "last answer: " + a.Outcome
			}
		}
		v.Fobs = append(v.Fobs, line)
	}
	return v
}

// reader is the reader partial alone, polled while the page is open. It asks
// neither zaehlwerk nor the roster: the rig knows all it shows.
func (s *Server) reader(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.ExecuteTemplate(w, "reader", s.readerView()); err != nil {
		s.log.Error("rendering the reader", "error", err)
	}
}
func (s *Server) live(w http.ResponseWriter, r *http.Request) { s.render(w, r, "live") }

func (s *Server) press(w http.ResponseWriter, r *http.Request) {
	held, err := heldFrom(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	side := r.PostFormValue("side")
	if err := s.rig.Press(r.Context(), side, held); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.render(w, r, "live")
}

func (s *Server) pressBoth(w http.ResponseWriter, r *http.Request) {
	held, err := heldFrom(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.rig.PressBoth(r.Context(), held); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.render(w, r, "live")
}

func (s *Server) tap(w http.ResponseWriter, r *http.Request) {
	if err := s.rig.Tap(r.Context(), r.PostFormValue("side"), Gesture(r.PostFormValue("action"))); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.render(w, r, "screens")
}

func (s *Server) holdFob(w http.ResponseWriter, r *http.Request) {
	fob, ok := s.fob(r.PostFormValue("fob"))
	if !ok {
		http.Error(w, "no such fob on the page: reload it", http.StatusBadRequest)
		return
	}
	s.rig.HoldFob(fob)
	s.reader(w, r)
}

func (s *Server) screens(w http.ResponseWriter, r *http.Request) { s.render(w, r, "screens") }

func heldFrom(r *http.Request) (time.Duration, error) {
	ms, err := strconv.Atoi(r.PostFormValue("held_ms"))
	if err != nil || ms < 0 {
		return 0, errors.New("held_ms: want milliseconds")
	}
	return min(time.Duration(ms)*time.Millisecond, maxHold), nil
}

// settings takes the whole form on every change. A checkbox that is not
// ticked is not posted at all, so absent means off, never "unchanged".
func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	next := s.rig.Settings()

	long, err := strconv.Atoi(r.PostFormValue("long_ms"))
	if err != nil || long < 100 || long > 5000 {
		http.Error(w, "long_ms: want 100 to 5000", http.StatusBadRequest)
		return
	}
	window, err := strconv.Atoi(r.PostFormValue("window_ms"))
	if err != nil || window < 0 || window > 3000 {
		http.Error(w, "window_ms: want 0 to 3000", http.StatusBadRequest)
		return
	}
	next.LongPress = time.Duration(long) * time.Millisecond
	next.BothWindow = time.Duration(window) * time.Millisecond

	device := r.PostFormValue("device")
	if device != DevicePushButton && device != DeviceDisplay {
		http.Error(w, "device: want button or display", http.StatusBadRequest)
		return
	}
	if device != next.Device {
		// The table itself changes shape, which is more than #live: the
		// whole page is drawn again.
		w.Header().Set("HX-Refresh", "true")
	}
	next.Device = device

	on := func(k string) bool { return r.PostFormValue(k) == "on" }
	next.Debounce = on("debounce")
	next.RadioAckLost = on("radio_ack_lost")
	next.HubDedup = on("hub_dedup")
	next.APIResponseLost = on("api_response_lost")
	next.BothLongEndsMatch = on("both_long_ends")

	s.rig.SetSettings(next)
	s.render(w, r, "live")
}

// setPiezo changes the board's knobs, or pauses and resumes it. The board
// takes a whole Control, so the current one is read first and only what the
// form says is changed.
func (s *Server) setPiezo(w http.ResponseWriter, r *http.Request) {
	if s.piezo == nil {
		http.Error(w, "no piezo board: PIEZO_CONTROL_URL is unset", http.StatusNotFound)
		return
	}
	cur, err := s.piezo.status(r.Context())
	if err != nil {
		s.render(w, r, "piezo")
		return
	}
	next := cur.Control

	switch r.PostFormValue("action") {
	case "pause":
		next.Paused = true
	case "resume":
		next.Paused = false
	default:
		pace, err1 := strconv.ParseInt(r.PostFormValue("pace_ms"), 10, 64)
		amb, err2 := percent(r.PostFormValue("ambiguous"))
		res, err3 := percent(r.PostFormValue("resend"))
		und, err4 := percent(r.PostFormValue("undo"))
		if err := errors.Join(err1, err2, err3, err4); err != nil {
			http.Error(w, "the piezo form: "+err.Error(), http.StatusBadRequest)
			return
		}
		next.PaceMs, next.Ambiguous, next.Resend, next.Undo = pace, amb, res, und
	}

	if _, err := s.piezo.set(r.Context(), next); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.rig.note("page", fmt.Sprintf("piezo: paused %t, pace %d ms, ambiguous %.0f%%, resend %.0f%%, undo %.0f%%",
		next.Paused, next.PaceMs, next.Ambiguous*100, next.Resend*100, next.Undo*100), false)
	s.render(w, r, "piezo")
}

// percent reads a share the page shows as a percentage.
func percent(v string) (float64, error) {
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n < 0 || n > 100 {
		return 0, fmt.Errorf("%q: want 0 to 100", v)
	}
	return n / 100, nil
}

func (s *Server) clear(w http.ResponseWriter, r *http.Request) {
	s.rig.ClearLog()
	s.render(w, r, "live")
}

// RunFromEnv serves the page until ctx ends. The variables follow the piezo
// mock's, so one ZAEHLWERK_URL points both at the same instance.
func RunFromEnv(ctx context.Context, log *slog.Logger) error {
	api := strings.TrimRight(env("ZAEHLWERK_URL", "http://localhost:8080"), "/")
	public := strings.TrimRight(env("ZAEHLWERK_PUBLIC_URL", api), "/")
	addr := env("BUTTONS_ADDR", ":8084")

	settings := DefaultSettings()
	var err error
	if settings.LongPress, err = time.ParseDuration(env("BUTTONS_LONG_PRESS", "1s")); err != nil {
		return fmt.Errorf("BUTTONS_LONG_PRESS: %w", err)
	}
	if settings.BothWindow, err = time.ParseDuration(env("BUTTONS_BOTH_WINDOW", "400ms")); err != nil {
		return fmt.Errorf("BUTTONS_BOTH_WINDOW: %w", err)
	}
	if settings.Device = env("BUTTONS_DEVICE", DevicePushButton); settings.Device != DevicePushButton && settings.Device != DeviceDisplay {
		return fmt.Errorf("BUTTONS_DEVICE: want %s or %s", DevicePushButton, DeviceDisplay)
	}

	rig := New(api, env("BUTTONS_SOURCE", "button-mock"), settings, log)
	rig.SetCheckinToken(os.Getenv("CHECKIN_TOKEN"))
	pages := NewServer(rig, public+"/ui", strings.TrimRight(os.Getenv("PIEZO_CONTROL_URL"), "/"), log)

	// The players to offer a fob for, from the Schmetterpause zaehlwerk
	// reports to: the same two variables, so one pair configures both.
	if spURL := strings.TrimRight(os.Getenv("SCHMETTERPAUSE_URL"), "/"); spURL != "" {
		sp, err := schmetterpause.New(schmetterpause.Config{BaseURL: spURL, Token: os.Getenv("SCHMETTERPAUSE_TOKEN")})
		if err != nil {
			return fmt.Errorf("SCHMETTERPAUSE_URL: %w", err)
		}
		pages.WithRoster(sp)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           pages,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errs := make(chan error, 1)
	go func() { errs <- srv.ListenAndServe() }()
	log.Info("buttons up", "addr", addr, "api", api, "device", settings.Device, "long_press", settings.LongPress,
		"both_window", settings.BothWindow, "piezo_control", os.Getenv("PIEZO_CONTROL_URL"),
		// Whether a token is set, never the token.
		"checkin_token", os.Getenv("CHECKIN_TOKEN") != "",
		"schmetterpause", os.Getenv("SCHMETTERPAUSE_URL"), "schmetterpause_token", os.Getenv("SCHMETTERPAUSE_TOKEN") != "")

	select {
	case err := <-errs:
		return fmt.Errorf("serving the buttons page: %w", err)
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		return ctx.Err()
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
