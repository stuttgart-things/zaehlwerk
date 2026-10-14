package pong

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// TokenHeader carries a browser game's token on its presses.
const TokenHeader = "X-Pong-Token"

// DefaultHeartbeat is how often an idle stream to a page writes a keepalive
// comment, the same as zaehlwerk's.
const DefaultHeartbeat = 25 * time.Second

// writeTimeout bounds one write to one page. The server has no WriteTimeout,
// because it would cut every stream off, so a tab whose window has filled up is
// bounded here, one write at a time.
const writeTimeout = 10 * time.Second

// pressed is the answer to a press: the game as it stands, and why a press was
// not taken.
type pressed struct {
	View
	Error string `json:"error,omitempty"`
}

// press is POST /games/{id}/presses: one rally won or one ready key, from the
// page that holds the game's token.
//
// The answer comes once the game is ready for the next press, so it carries
// whatever happened in between — a ghost point, a change of ends — and the
// page starts its next rally from what zaehlwerk says. A press number seen
// before is answered without being pressed again.
func (s *Server) press(w http.ResponseWriter, r *http.Request) {
	g, ok := s.tokenGame(w, r)
	if !ok {
		return
	}
	b, isBrowser := g.driver.(*browser)
	if !isBrowser {
		// A check presses nothing: the buttons do, through zaehlwerk.
		fail(w, http.StatusConflict, errors.New("a button check takes no presses, the buttons at the table make them"))
		return
	}

	var p Press
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		fail(w, http.StatusBadRequest, fmt.Errorf("decoding the press: %w", err))
		return
	}
	if err := p.validate(); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}

	duplicate, err := b.submit(r.Context(), g.Done(), p)
	switch {
	case err == nil && duplicate:
		g.log.Debug("press already taken", "press", p.Rally)
		writeJSON(w, http.StatusOK, pressed{View: g.View()})
	case err == nil:
		writeJSON(w, http.StatusOK, pressed{View: g.View()})
	case errors.Is(err, errOutOfPhase), errors.Is(err, errOutOfOrder), errors.Is(err, ErrMatchOver):
		writeJSON(w, http.StatusConflict, pressed{View: g.View(), Error: err.Error()})
	case r.Context().Err() != nil:
		// The page went away while its press was waiting. Nothing to answer.
	default:
		fail(w, http.StatusInternalServerError, err)
	}
}

// endGame is POST /games/{id}/end: the page that plays a browser game or runs a
// button check ends it, and its match with it.
func (s *Server) endGame(w http.ResponseWriter, r *http.Request) {
	g, ok := s.tokenGame(w, r)
	if !ok {
		return
	}
	g.stop(errStopped)
	select {
	case <-g.Done():
	case <-r.Context().Done():
		return
	case <-time.After(10 * time.Second):
		fail(w, http.StatusGatewayTimeout, errors.New("the game did not stop in time"))
		return
	}
	writeJSON(w, http.StatusOK, g.View())
}

// tokenGame finds the game in the path and checks that the request may play
// it: a game with a page, and the token it was started with. Another tab may
// watch the same game; it does not get to press or end it.
func (s *Server) tokenGame(w http.ResponseWriter, r *http.Request) (*Game, bool) {
	g, ok := s.game(r.PathValue("id"))
	if !ok {
		fail(w, http.StatusNotFound, fmt.Errorf("no game %q", r.PathValue("id")))
		return nil, false
	}
	if g.token == "" {
		fail(w, http.StatusConflict, errors.New("a headless game plays itself"))
		return nil, false
	}
	token := r.Header.Get(TokenHeader)
	if subtle.ConstantTimeCompare([]byte(token), []byte(g.token)) != 1 {
		fail(w, http.StatusForbidden, errors.New("only the page that started the game reports for it"))
		return nil, false
	}
	return g, true
}

func (s *Server) game(id string) (*Game, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.games[id]
	return g, ok
}

// events is GET /games/{id}/events: the game's view as JSON, once on connect
// and again on every change, until the game is over. The canvas draws from it
// — phase, ends, waiting and serving as zaehlwerk answered them — and derives
// none of them.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	g, ok := s.game(r.PathValue("id"))
	if !ok {
		fail(w, http.StatusNotFound, fmt.Errorf("no game %q", r.PathValue("id")))
		return
	}
	s.follow(w, r, g, func(v View) ([]sseEvent, error) {
		raw, err := json.Marshal(v)
		return []sseEvent{{Data: string(raw)}}, err
	})
}

// sseEvent is one event on a stream: a name, empty for the default, and data.
type sseEvent struct {
	Name string
	Data string
}

// follow writes the game's view to a stream on connect and again on every
// change, rendered into events by render.
func (s *Server) follow(w http.ResponseWriter, r *http.Request, g *Game, render func(View) ([]sseEvent, error)) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	heartbeat := time.NewTicker(s.heartbeat)
	defer heartbeat.Stop()
	for {
		// The wake is taken before the view, so a change between the two
		// wakes this loop again rather than being missed.
		wake := g.Changes()
		events, err := render(g.View())
		if err != nil {
			s.log.Warn("rendering a game event", "game", g.ID, "error", err)
			return
		}
		for _, ev := range events {
			if err := writeEvent(r.Context(), rc, w, ev.Name, ev.Data); err != nil {
				return
			}
		}

		// Open past the end of the game too: a stream that closes is one
		// the browser opens again, every few seconds, for a view that no
		// longer changes.
	wait:
		for {
			select {
			case <-r.Context().Done():
				return
			case <-s.ctx.Done():
				return
			case <-wake:
				break wait
			case <-heartbeat.C:
				if err := writeRaw(rc, w, ": ping\n\n"); err != nil {
					return
				}
			}
		}
	}
}

// writeEvent writes one SSE event. The data may span lines, and a newline ends
// an SSE field, so every line goes out as its own "data:" and the browser
// joins them back.
func writeEvent(ctx context.Context, rc *http.ResponseController, w http.ResponseWriter, name, data string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var b strings.Builder
	if name != "" {
		b.WriteString("event: " + name + "\n")
	}
	for _, line := range strings.Split(data, "\n") {
		b.WriteString("data: " + line + "\n")
	}
	b.WriteString("\n")
	return writeRaw(rc, w, b.String())
}

func writeRaw(rc *http.ResponseController, w http.ResponseWriter, s string) error {
	if err := rc.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return fmt.Errorf("setting the write deadline: %w", err)
	}
	if _, err := w.Write([]byte(s)); err != nil {
		return err
	}
	if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return fmt.Errorf("flushing: %w", err)
	}
	return nil
}
