// Package api serves the ingest and match lifecycle endpoints.
//
// It is the adapter half of ADR-0002: each ingest route accepts the wire format
// of its own source and normalises it into a [scorer.ScoreEvent]. Source
// specific quirks stop here — the scorer never learns where a point came from
// beyond the Source it deduplicates on.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/live"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// maxBodyBytes caps a request body. A batched burst from the hub is a few
// hundred bytes; this is room to spare and a bound on what an open endpoint on
// the office network can make the process allocate.
const maxBodyBytes = 64 << 10

// Server routes requests to the registry. Use [New] to build one.
type Server struct {
	registry *match.Registry
	log      *slog.Logger
	now      func() time.Time
	mux      *http.ServeMux

	// hub is nil when live streaming is not configured, and the stream route
	// then answers 503 rather than 404 — the match exists, the feature does not.
	hub            *live.Hub
	allowedOrigins []string
	heartbeat      time.Duration
}

// Option configures a Server.
type Option func(*Server)

// WithLogger replaces the logger.
func WithLogger(l *slog.Logger) Option {
	return func(s *Server) { s.log = l }
}

// WithClock replaces the clock, for tests.
func WithClock(now func() time.Time) Option {
	return func(s *Server) { s.now = now }
}

// WithHub enables the live stream, served from h.
func WithHub(h *live.Hub) Option {
	return func(s *Server) { s.hub = h }
}

// WithAllowedOrigins sets the origins that may read the live stream from a
// browser. Deliberately a list and never "*".
func WithAllowedOrigins(origins []string) Option {
	return func(s *Server) { s.allowedOrigins = slices.Clone(origins) }
}

// WithHeartbeat sets how often an idle stream writes a keepalive comment.
func WithHeartbeat(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.heartbeat = d
		}
	}
}

func New(registry *match.Registry, opts ...Option) *Server {
	s := &Server{
		registry:  registry,
		log:       slog.Default(),
		now:       time.Now,
		mux:       http.NewServeMux(),
		heartbeat: DefaultHeartbeat,
	}
	for _, opt := range opts {
		opt(s)
	}

	s.mux.HandleFunc("GET /healthz", s.healthz)

	s.mux.HandleFunc("POST /matches", s.createMatch)
	s.mux.HandleFunc("GET /matches/{id}", s.getMatch)
	s.mux.HandleFunc("POST /matches/{id}/undo", s.undo)
	s.mux.HandleFunc("POST /matches/{id}/end", s.endMatch)
	s.mux.HandleFunc("GET /matches/{id}/stream", s.stream)
	s.mux.HandleFunc("OPTIONS /matches/{id}/stream", s.streamPreflight)

	s.mux.HandleFunc("POST /ingest/button", s.ingestButton)
	s.mux.HandleFunc("POST /ingest/piezo", s.ingestPiezo)
	s.mux.HandleFunc("POST /ingest/web", s.ingestWeb)

	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// errorBody carries the reason and, where a match was identified, its state —
// so a client that gets a 409 can still render the score it asked about.
type errorBody struct {
	Error string        `json:"error"`
	State *scorer.State `json:"state,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status is already on the wire, so a failed encode can only be logged
	// by the caller's middleware, not turned into an error response.
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) writeState(w http.ResponseWriter, status int, st scorer.State) {
	writeJSON(w, status, st)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, err error, st *scorer.State) {
	s.log.InfoContext(r.Context(), "request rejected",
		"method", r.Method, "path", r.URL.Path, "status", status, "error", err)
	writeJSON(w, status, errorBody{Error: err.Error(), State: st})
}

// decode reads a JSON body into v, capping how much it will read.
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return err
	}
	return nil
}

// lookup resolves the match named in the path.
func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (*match.Match, bool) {
	m, err := s.registry.Get(r.PathValue("id"))
	if err != nil {
		s.fail(w, r, http.StatusNotFound, err, nil)
		return nil, false
	}
	return m, true
}
