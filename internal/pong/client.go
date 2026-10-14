// Package pong is a table tennis game that plays whole matches against a
// running zaehlwerk and scores them through the real ingest path (ADR-0006).
//
// It is a pure HTTP client of zaehlwerk. It imports wire types — the state, and
// the names of players, halves and phases — and nothing that holds state: no
// scorer, no registry, no hub. What it knows about a match it was told, either
// in the answer to what it sent or on /live/stream.
package pong

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// The kinds on /live/stream the game acts on. Spelled out rather than imported:
// they live in internal/api and internal/live, next to the hub and the
// registry, and the game takes nothing from there. The tests run the game
// against the real handlers, so a renamed kind fails there and not at a fair.
const (
	kindSnapshot = "snapshot"
	kindNoMatch  = "no_match"
)

// ingestAttempts is how often a report is sent before the game gives up. A
// retry is safe because zaehlwerk discards an event id it has already seen
// (ADR-0002): a report whose answer was lost counts once however often it is
// sent.
const ingestAttempts = 3

// ErrMatchOver is zaehlwerk refusing a report because the match is no longer
// running: won, ended on the page, or evicted.
var ErrMatchOver = errors.New("the match is over")

// Client talks to one zaehlwerk.
type Client struct {
	base string
	// http has a timeout; stream has none, because /live/stream stays open
	// for as long as the game wants it and is ended by its context.
	http   *http.Client
	stream *http.Client
	log    *slog.Logger
	// retryAfter is the pause before a report is sent again. A variable so
	// the tests do not sit through it.
	retryAfter time.Duration
}

func NewClient(base string, log *slog.Logger) *Client {
	return &Client{
		base:       strings.TrimRight(base, "/"),
		http:       &http.Client{Timeout: 5 * time.Second},
		stream:     &http.Client{},
		log:        log,
		retryAfter: 200 * time.Millisecond,
	}
}

// createBody is POST /matches as the game sends it. There is deliberately no
// field for a player id or an operator: a match started through the JSON API
// has no handover and is never reported to Schmetterpause (ADR-0004, ADR-0006).
//
// Demo is always true. Every match the game creates is no result and gives way
// to one, demo, browser game and button check alike (ADR-0008), and zaehlwerk
// decides that under its own lock, where a real match created at the same
// moment cannot slip in between.
type createBody struct {
	Players    [2]string `json:"players"`
	BestOf     int       `json:"best_of"`
	Start      string    `json:"start"`
	ChangeEnds bool      `json:"change_ends"`
	Demo       bool      `json:"demo"`
}

// CreateMatch starts a match the way it starts at the table: a warm-up, the
// rally for the serve, and a change of ends after every set and at five in the
// deciding one. Not retried: a second POST would be a second match.
func (c *Client) CreateMatch(ctx context.Context, players [2]string, bestOf int) (scorer.State, error) {
	st, status, err := c.call(ctx, c.http, http.MethodPost, "/matches", createBody{
		Players:    players,
		BestOf:     bestOf,
		Start:      string(scorer.StartWarmup),
		ChangeEnds: true,
		Demo:       true,
	})
	if err != nil {
		return st, err
	}
	if status == http.StatusConflict {
		// A real match is running, and zaehlwerk refuses a demo next to it:
		// the table is taken, as it is when the stream shows a current match.
		// The answer's state is that match.
		return st, fmt.Errorf("%w: POST /matches: match %s is running at the table", errBusy, st.MatchID)
	}
	if status != http.StatusCreated {
		return st, fmt.Errorf("POST /matches: %d", status)
	}
	return st, nil
}

// Ingest reports one press: a rally won, or a side ready. It goes to
// /ingest/web, the browser's door (invariant 6), with the match id so a game
// left running cannot score into somebody else's match.
//
// A lost answer is sent again with the same event id. That is the whole of
// what makes it safe: zaehlwerk discards the repeat and answers with the state
// the first one left.
func (c *Client) Ingest(ctx context.Context, ev scorer.ScoreEvent) (scorer.State, error) {
	var lastErr error
	for attempt := 1; attempt <= ingestAttempts; attempt++ {
		st, status, err := c.call(ctx, c.http, http.MethodPost, "/ingest/web", ev)
		switch {
		case err == nil && status == http.StatusOK:
			return st, nil
		case err == nil && (status == http.StatusConflict || status == http.StatusNotFound):
			return st, fmt.Errorf("event %d: %w", ev.EventID, ErrMatchOver)
		case err == nil && status < http.StatusInternalServerError:
			// A 400 is the game's mistake, and sending it again will not
			// make it less of one.
			return st, fmt.Errorf("POST /ingest/web: event %d: %d", ev.EventID, status)
		case err == nil:
			lastErr = fmt.Errorf("POST /ingest/web: event %d: %d", ev.EventID, status)
		case ctx.Err() != nil:
			return st, err
		default:
			lastErr = err
		}

		c.log.Warn("report not answered, sending it again",
			"event_id", ev.EventID, "attempt", attempt, "error", lastErr)
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(c.retryAfter):
		}
	}
	return scorer.State{}, lastErr
}

// EndMatch ends a match the game will not finish. Idempotent on zaehlwerk's
// side, so ending one that is already over is harmless.
func (c *Client) EndMatch(ctx context.Context, matchID string) (scorer.State, error) {
	st, status, err := c.call(ctx, c.http, http.MethodPost, "/matches/"+matchID+"/end", nil)
	if err != nil {
		return st, err
	}
	if status != http.StatusOK {
		return st, fmt.Errorf("POST /matches/%s/end: %d", matchID, status)
	}
	return st, nil
}

// ErrNothingToUndo is zaehlwerk refusing an undo because there is no press to
// take back.
var ErrNothingToUndo = errors.New("nothing to undo")

// Undo takes back the last press that changed the score, the way the page's
// undo button does. Not retried: an undo sent twice takes back two presses.
func (c *Client) Undo(ctx context.Context, matchID string) (scorer.State, error) {
	st, status, err := c.call(ctx, c.http, http.MethodPost, "/matches/"+matchID+"/undo", nil)
	switch {
	case err != nil:
		return st, err
	case status == http.StatusOK:
		return st, nil
	case status == http.StatusConflict:
		return st, fmt.Errorf("POST /matches/%s/undo: %w", matchID, ErrNothingToUndo)
	case status == http.StatusNotFound:
		return st, fmt.Errorf("POST /matches/%s/undo: %w", matchID, ErrMatchOver)
	}
	return st, fmt.Errorf("POST /matches/%s/undo: %d", matchID, status)
}

// TableEvent is one event on GET /live/stream.
type TableEvent struct {
	Kind  string        `json:"kind"`
	State *scorer.State `json:"state"`
	// HomeID and AwayID are the players' Schmetterpause ids, present exactly
	// when the match is being reported.
	HomeID string `json:"home_id"`
	AwayID string `json:"away_id"`
	// Demo is a match that is no result and gives way to one (ADR-0008).
	Demo bool `json:"demo"`
}

// Reported says the match is a real one, played by people and reported to
// Schmetterpause. The game never touches it.
func (e TableEvent) Reported() bool { return e.HomeID != "" || e.AwayID != "" }

// Follow connects to /live/stream and delivers its events until ctx ends or the
// stream breaks; the channel is closed either way.
//
// It returns once the connection is up, so the first event on the channel is
// what the table looks like now: the current match's snapshot, or no_match.
func (c *Client) Follow(ctx context.Context) (<-chan TableEvent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/live/stream", nil)
	if err != nil {
		return nil, fmt.Errorf("building GET /live/stream: %w", err)
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /live/stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("GET /live/stream: %s", resp.Status)
	}

	events := make(chan TableEvent)
	go func() {
		defer close(events)
		defer func() { _ = resp.Body.Close() }()

		lines := bufio.NewScanner(resp.Body)
		// A state with a long match's completed sets is well under this, but
		// the default 64 KiB would end the stream silently if it ever were not.
		lines.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for lines.Scan() {
			// Server-Sent Events: one "data:" line per event here, and
			// comments (": ping") and blank separators in between.
			data, ok := strings.CutPrefix(lines.Text(), "data: ")
			if !ok {
				continue
			}
			var ev TableEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				c.log.Warn("skipping an event on /live/stream that does not decode", "error", err)
				continue
			}
			select {
			case events <- ev:
			case <-ctx.Done():
				return
			}
		}
		if err := lines.Err(); err != nil && ctx.Err() == nil {
			c.log.Warn("/live/stream broke off", "error", err)
		}
	}()
	return events, nil
}

// call sends a request and decodes the state from the body, or from the state
// an error body carries. The status is returned rather than judged here: a 409
// means "over" to a report and "busy" to nobody else.
func (c *Client) call(ctx context.Context, hc *http.Client, method, path string, body any) (scorer.State, int, error) {
	var st scorer.State

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return st, 0, fmt.Errorf("encoding %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return st, 0, fmt.Errorf("building %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := hc.Do(req)
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
		Error string        `json:"error"`
		State *scorer.State `json:"state"`
	}
	if json.Unmarshal(raw, &refusal) == nil {
		if refusal.State != nil {
			st = *refusal.State
		}
		c.log.Debug("refused", "method", method, "path", path,
			"status", resp.StatusCode, "error", refusal.Error)
	}
	return st, resp.StatusCode, nil
}
