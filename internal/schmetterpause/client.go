// Package schmetterpause is the one-way coupling to the application that keeps
// players, ratings and history.
//
// ADR-0001 planned it from the start — this service owns the running match and
// hands the finished result over at the end — and ADR-0004 decided its shape.
// Two calls, one direction each: the player list out of Schmetterpause, a
// finished result into it. Nothing comes back, and nothing here is stored:
// invariant 2 means this service has nowhere to put a copy and does not want
// one.
//
// Optional, like every outbound coupling here (invariant 4). Without a base URL
// the constructor is never called, and a match is scored, shown and pitched
// exactly as before.
package schmetterpause

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout bounds both calls.
//
// Short on purpose. The player list is fetched while somebody is looking at a
// form waiting for it, and the result is posted from an observer at the end of
// a match — neither is a place to wait out a dead connection. A result that
// does not go through is retried from the page rather than by holding a
// request open.
const DefaultTimeout = 5 * time.Second

// Config configures a Client.
type Config struct {
	// BaseURL is scheme and host, for example https://schmetterpause.example.
	BaseURL string
	// Token is the bearer token. Schmetterpause does not register its /api
	// routes without one configured on its side, so an empty token here means
	// every call gets a 404 or a 401 — which is why the caller logs whether
	// one is set.
	Token   string
	Timeout time.Duration
}

// Client talks to Schmetterpause.
type Client struct {
	base   string
	token  string
	client *http.Client
}

// Player is one entry of the player list.
//
// Exactly the three fields Schmetterpause publishes. Fetched fresh whenever it
// is needed and never cached: ADR-0004 keeps that application the sole owner of
// player identity, so a stale copy here could offer a player who no longer
// exists or miss one who just joined.
type Player struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	TTR         int    `json:"ttr"`
}

// Result is a finished match, in the shape Schmetterpause accepts.
//
// Sets are pairs because scorer.State.CompletedSets is [][2]int and exists for
// exactly this handover: the caller passes its own field through rather than
// rebuilding it, so there is no transformation in which home and away could be
// swapped.
type Result struct {
	HomeID      string     `json:"home_id"`
	AwayID      string     `json:"away_id"`
	OperatorID  string     `json:"operator_id"`
	Sets        [][2]int   `json:"sets"`
	BestOf      int        `json:"best_of"`
	PointsToWin int        `json:"points_to_win"`
	PlayedAt    *time.Time `json:"played_at,omitempty"`
}

// Accepted is what Schmetterpause says about a result it stored.
//
// Status is "pending" while the test phase in Schmetterpause's ADR-0015 runs:
// the result waits for one of the two players to agree before it reaches the
// ranking. The two clocks are different on purpose — points on the matrix are
// immediate, the rating is not — so this is reported rather than waited on.
type Accepted struct {
	MatchID string `json:"match_id"`
	Status  string `json:"status"`
}

// ErrNotConfigured is returned by New when there is no base URL. It is not a
// failure: it is the shape of "this coupling is off".
var ErrNotConfigured = errors.New("schmetterpause: no base URL configured")

// New builds a client, or reports that the coupling is off.
func New(cfg Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, ErrNotConfigured
	}
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("schmetterpause: base url %q: %w", base, err)
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}

	return &Client{
		base:   base,
		token:  cfg.Token,
		client: &http.Client{Timeout: timeout},
	}, nil
}

// Players fetches the list, every time it is asked.
func (c *Client) Players(ctx context.Context) ([]Player, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/players", nil)
	if err != nil {
		return nil, fmt.Errorf("schmetterpause: building the player request: %w", err)
	}
	c.authorize(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("schmetterpause: fetching players: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("schmetterpause: fetching players: %w", statusError(resp))
	}

	var players []Player
	if err := json.NewDecoder(resp.Body).Decode(&players); err != nil {
		return nil, fmt.Errorf("schmetterpause: decoding players: %w", err)
	}
	return players, nil
}

// Report hands a finished result over.
//
// Loud on failure and with nothing behind it: there is no queue, because
// queueing would mean giving this service persistence, and ADR-0004 accepted a
// lost result rather than take that decision here. The caller keeps the match
// in the registry so the page can offer a retry until retention drops it.
func (c *Client) Report(ctx context.Context, result Result) (Accepted, error) {
	body, err := json.Marshal(result)
	if err != nil {
		return Accepted{}, fmt.Errorf("schmetterpause: encoding the result: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/results", bytes.NewReader(body))
	if err != nil {
		return Accepted{}, fmt.Errorf("schmetterpause: building the result request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return Accepted{}, fmt.Errorf("schmetterpause: reporting the result: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return Accepted{}, fmt.Errorf("schmetterpause: reporting the result: %w", statusError(resp))
	}

	var accepted Accepted
	if err := json.NewDecoder(resp.Body).Decode(&accepted); err != nil {
		return Accepted{}, fmt.Errorf("schmetterpause: decoding the response: %w", err)
	}
	return accepted, nil
}

func (c *Client) authorize(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
}

// statusError turns a refusal into something a person can read in a log.
//
// Schmetterpause answers every refusal on this surface with {"error": "..."},
// and that sentence says what to change — which player id is unknown, that the
// operator is playing. Passing it through is the difference between a log line
// somebody can act on and "422".
func statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))

	var refusal struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &refusal); err == nil && refusal.Error != "" {
		return fmt.Errorf("%s: %s", resp.Status, refusal.Error)
	}
	if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
		return fmt.Errorf("%s: %s", resp.Status, trimmed)
	}
	return errors.New(resp.Status)
}
