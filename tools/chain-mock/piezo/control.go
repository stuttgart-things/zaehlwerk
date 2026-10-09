package piezo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Control is what can be changed on a board while it plays: whether it plays
// at all, and how it misbehaves. Everything else in [Config] is fixed for the
// run, because changing it mid-match would change what the match is.
//
// It exists for the table mock's page (tools/chain-mock/buttons), which pauses
// the board to try the buttons undisturbed. Pausing is the runtime switch;
// EDGE_ESP_MOCK_REPLICAS in a cluster stays the hard one, for the day real
// boards are at the table.
type Control struct {
	Paused    bool    `json:"paused"`
	PaceMs    int64   `json:"pace_ms"`
	Ambiguous float64 `json:"ambiguous"`
	Resend    float64 `json:"resend"`
	Undo      float64 `json:"undo"`
}

func (c Control) pace() time.Duration { return time.Duration(c.PaceMs) * time.Millisecond }

func (c Control) validate() error {
	if c.PaceMs < 100 || c.PaceMs > 60_000 {
		return errors.New("pace_ms: want 100 to 60000")
	}
	for name, v := range map[string]float64{"ambiguous": c.Ambiguous, "resend": c.Resend, "undo": c.Undo} {
		if v < 0 || v > 1 {
			return fmt.Errorf("%s: want a share between 0 and 1", name)
		}
	}
	return nil
}

// Status is Control plus what the board is doing, for GET /control.
type Status struct {
	Control
	Doing   string `json:"doing"`
	MatchID string `json:"match_id,omitempty"`
}

// Control returns the current knobs.
func (b *Board) Control() Control {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ctl
}

// SetControl replaces them; the next rally plays with the new ones.
func (b *Board) SetControl(c Control) error {
	if err := c.validate(); err != nil {
		return err
	}
	b.mu.Lock()
	b.ctl = c
	b.mu.Unlock()
	b.log.Info("control", "paused", c.Paused, "pace_ms", c.PaceMs,
		"ambiguous", c.Ambiguous, "resend", c.Resend, "undo", c.Undo)
	return nil
}

// Status returns the knobs and what the board is doing.
func (b *Board) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	doing := b.doing
	if b.ctl.Paused && doing == "playing" {
		doing = "paused"
	}
	return Status{Control: b.ctl, Doing: doing, MatchID: b.matchID}
}

func (b *Board) setDoing(doing, matchID string) {
	b.mu.Lock()
	b.doing, b.matchID = doing, matchID
	b.mu.Unlock()
}

// pausePoll is how often a paused board looks whether it may go on. Resuming
// is a person clicking; a quarter of a second is not noticed.
const pausePoll = 250 * time.Millisecond

func (b *Board) waitWhilePaused(ctx context.Context) error {
	logged := false
	for b.Control().Paused {
		if !logged {
			b.log.Info("paused")
			logged = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pausePoll):
		}
	}
	if logged {
		b.log.Info("resumed")
	}
	return nil
}

// ControlHandler serves GET and POST /control as JSON, and /healthz.
func (b *Board) ControlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /control", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, b.Status())
	})
	// A whole Control, not a patch: the one caller reads it first and sends
	// it back changed, and a missing field silently meaning zero is worse
	// than a client that has to send all five.
	mux.HandleFunc("POST /control", func(w http.ResponseWriter, r *http.Request) {
		var c Control
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := b.SetControl(c); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, b.Status())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// ServeControl listens on addr until ctx ends or stop is called. It returns
// once the listener is bound, so a port already taken fails the start rather
// than a goroutine's log line later.
func (b *Board) ServeControl(ctx context.Context, addr string) (stop func(), err error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("PIEZO_CONTROL_ADDR: %w", err)
	}
	srv := &http.Server{Handler: b.ControlHandler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	b.log.Info("control listening", "addr", ln.Addr().String())

	return func() {
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
