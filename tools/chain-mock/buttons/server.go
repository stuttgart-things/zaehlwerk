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
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

//go:embed page.html
var pageFS embed.FS

var page = template.Must(template.New("").Funcs(template.FuncMap{
	"ms":    func(d time.Duration) int64 { return d.Milliseconds() },
	"clock": func(t time.Time) string { return t.Format("15:04:05.000") },
	"name":  func(st scorer.State, half string) string { return st.Players[index(playerOn(half, st.SetNumber))] },
}).ParseFS(pageFS, "page.html"))

// maxHold caps a press posted by the page. Longer than anybody holds a button,
// short enough that a stuck pointer does not read as a gesture.
const maxHold = 10 * time.Second

// Server is the page. Use [NewServer].
type Server struct {
	rig *Rig
	// zaehlwerkUI is where the page links to start a match. Not the API URL
	// the rig calls: in a pod that one is a cluster address a browser cannot
	// reach.
	zaehlwerkUI string
	log         *slog.Logger
	mux         *http.ServeMux
}

func NewServer(rig *Rig, zaehlwerkUI string, log *slog.Logger) *Server {
	s := &Server{rig: rig, zaehlwerkUI: zaehlwerkUI, log: log, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /{$}", s.index)
	s.mux.HandleFunc("GET /live", s.live)
	s.mux.HandleFunc("POST /press", s.press)
	s.mux.HandleFunc("POST /press-both", s.pressBoth)
	s.mux.HandleFunc("POST /settings", s.settings)
	s.mux.HandleFunc("POST /clear", s.clear)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

type view struct {
	Settings    Settings
	Running     bool
	State       scorer.State
	APIError    string
	Entries     []Entry
	ZaehlwerkUI string
	Sources     [2]string
}

func (s *Server) view(ctx context.Context) view {
	v := view{
		Settings:    s.rig.Settings(),
		Entries:     s.rig.Entries(),
		ZaehlwerkUI: s.zaehlwerkUI,
		Sources:     [2]string{s.rig.buttons["A"].source, s.rig.buttons["B"].source},
	}
	st, ok, err := s.rig.Current(ctx)
	if err != nil {
		v.APIError = err.Error()
	}
	v.Running, v.State = ok, st
	return v
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.ExecuteTemplate(w, name, s.view(r.Context())); err != nil {
		s.log.Error("rendering the page", "template", name, "error", err)
	}
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) { s.render(w, r, "page") }
func (s *Server) live(w http.ResponseWriter, r *http.Request)  { s.render(w, r, "live") }

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

	on := func(k string) bool { return r.PostFormValue(k) == "on" }
	next.Debounce = on("debounce")
	next.RadioAckLost = on("radio_ack_lost")
	next.HubDedup = on("hub_dedup")
	next.APIResponseLost = on("api_response_lost")
	next.BothLongEndsMatch = on("both_long_ends")

	s.rig.SetSettings(next)
	s.render(w, r, "live")
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

	rig := New(api, env("BUTTONS_SOURCE", "button-mock"), settings, log)
	srv := &http.Server{
		Addr:              addr,
		Handler:           NewServer(rig, public+"/ui", log),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errs := make(chan error, 1)
	go func() { errs <- srv.ListenAndServe() }()
	log.Info("buttons up", "addr", addr, "api", api, "long_press", settings.LongPress,
		"both_window", settings.BothWindow)

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
