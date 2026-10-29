package buttons

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/face"
	"github.com/stuttgart-things/zaehlwerk/internal/figure"
	"github.com/stuttgart-things/zaehlwerk/internal/ui/static"
)

// The players' figures on the table mock, as on zaehlwerk's spectator view and
// the game's board (zaehlwerk ADR-0018, Schmetterpause ADR-0029): each
// player's fob carries that player's figure, a half somebody checked in at
// shows theirs, and the running match shows the two zaehlwerk shows, with the
// faces zaehlwerk gives them.
//
// The running match's figures are zaehlwerk's, from its own route: a side
// without a player has a random figure, drawn once per match, and only
// zaehlwerk knows which one it drew. Everything else is a player's figure, by
// the id the roster gave the page, fetched here: SCHMETTERPAUSE_URL may be an
// address only the pod can reach.

// WithFigures gives the fobs and the halves their players' figures.
func (s *Server) WithFigures(f figure.Source) *Server {
	s.figures = f
	return s
}

// figureURL is where the page gets the figure of the player with id.
func figureURL(id string) string { return "/figures/" + id + ".svg" }

// figure serves the figure of a player the page offers a fob for, with a
// neutral face: nobody is playing yet.
//
//	GET /figures/{id}.svg
//
// Only a player on the list the page was drawn with: the route is not a way
// to fetch anything else from Schmetterpause. A figure that could not be
// fetched is zaehlwerk's mascot.
func (s *Server) figure(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutSuffix(r.PathValue("file"), ".svg")
	if f, known := s.fob(id); !ok || !known || f.Player == "" {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), figure.Wait)
	defer cancel()
	svg, err := s.figures.Figure(ctx, id)

	h := w.Header()
	for k, v := range figure.Headers {
		h.Set(k, v)
	}
	if err != nil {
		s.log.Warn("figure unavailable", "player", id, "error", err)
		h.Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(static.Mascot))
		return
	}
	// A few minutes, not an hour: a player who saves a new figure sees it on
	// the next reload but one.
	h.Set("Cache-Control", "private, max-age=300")
	_, _ = w.Write(figure.WithFace(svg, face.Neutral))
}

// playerFigure is the figure of the player called name, if the page offers a
// fob for one. zaehlwerk names a checked-in player and no id (ADR-0004), so
// the name is looked up as the game's board looks it up.
func (s *Server) playerFigure(name string) string {
	if s.figures == nil || name == "" {
		return ""
	}
	s.fobsMu.Lock()
	defer s.fobsMu.Unlock()
	for _, f := range s.fobs {
		if f.Player == name {
			return figureURL(f.Key)
		}
	}
	return ""
}

// tableFaces is the faces zaehlwerk's table stream last gave the running
// match's figures. The stream is where they are decided (ADR-0018); the page
// polls the score and cannot see the moment between two polls.
type tableFaces struct {
	mu    sync.Mutex
	match string
	faces face.Faces
}

// of is the faces of the match with id, neutral for any other: the stream
// has not caught up with it, or is not followed.
func (t *tableFaces) of(id string) face.Faces {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.match != id || id == "" {
		return face.Faces{face.Neutral, face.Neutral}
	}
	return t.faces
}

// tableEvent is the part of an event on GET /live/stream the page reads.
type tableEvent struct {
	State *struct {
		MatchID string `json:"match_id"`
	} `json:"state"`
	Faces *face.Faces `json:"faces"`
}

func (t *tableFaces) saw(ev tableEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ev.State == nil || ev.Faces == nil {
		t.match, t.faces = "", face.Faces{}
		return
	}
	t.match, t.faces = ev.State.MatchID, *ev.Faces
}

// followAgain is how long the page waits before it follows the stream again
// after zaehlwerk dropped it or was not there.
const followAgain = 2 * time.Second

// follow reads zaehlwerk's table stream at url until ctx ends, following it
// again whenever it breaks.
func (t *tableFaces) follow(ctx context.Context, url string, log *slog.Logger) {
	// No timeout: the stream stays open all afternoon, with a heartbeat.
	client := &http.Client{}
	for {
		if err := t.read(ctx, client, url); err != nil && ctx.Err() == nil {
			log.Debug("following the table stream", "error", err)
		}
		// Neutral while not following: faces from before the break may be
		// about a point long gone.
		t.saw(tableEvent{})
		select {
		case <-ctx.Done():
			return
		case <-time.After(followAgain):
		}
	}
}

func (t *tableFaces) read(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("building GET /live/stream: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GET /live/stream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("GET /live/stream: %s", resp.Status)
	}
	lines := bufio.NewScanner(resp.Body)
	lines.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for lines.Scan() {
		data, ok := strings.CutPrefix(lines.Text(), "data: ")
		if !ok {
			continue
		}
		var ev tableEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return fmt.Errorf("decoding a table event: %w", err)
		}
		t.saw(ev)
	}
	if err := lines.Err(); err != nil {
		return fmt.Errorf("reading the table stream: %w", err)
	}
	return io.ErrUnexpectedEOF
}
