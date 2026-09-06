package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	homerun "github.com/stuttgart-things/homerun-library/v4"
)

// Defaults for HTTPPitcherConfig.
const (
	// DefaultPitchPath is where homerun2-omni-pitcher accepts a message.
	DefaultPitchPath = "/pitch"
	// DefaultPitchTimeoutHTTP bounds one publish over HTTP. Longer than the
	// Redis path allows for, because this one crosses an ingress.
	DefaultPitchTimeoutHTTP = 5 * time.Second
)

// HTTPPitcherConfig configures an [HTTPPitcher].
type HTTPPitcherConfig struct {
	// BaseURL is the omni-pitcher, e.g. https://omni-pitcher.example. The
	// path is appended.
	BaseURL string
	// Path is the endpoint on it. Defaults to /pitch.
	Path string
	// Token is the bearer token. /pitch rejects an unauthenticated request
	// with 401.
	Token string
	// ExpectStream is the stream the messages are meant to land on. Only used
	// to notice when they do not — see Enqueue.
	ExpectStream string

	Timeout time.Duration
	Logger  *slog.Logger
}

func (c HTTPPitcherConfig) withDefaults() HTTPPitcherConfig {
	if c.Path == "" {
		c.Path = DefaultPitchPath
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultPitchTimeoutHTTP
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// HTTPPitcher publishes through homerun2-omni-pitcher's HTTP API instead of
// writing to Redis directly. It satisfies [Pitcher], so [Sink] does not know
// or care which one it has.
//
// This is the path for a zaehlwerk that runs by the table rather than in the
// cluster: Redis there is a ClusterIP service and not reachable, while
// omni-pitcher has an HTTPRoute and a bearer token.
//
// **The destination stream is the server's decision, not ours.** omni-pitcher
// routes by rules an operator declares (`ROUTES_CONFIG`), matching on system,
// author, tags or title; without a matching rule everything lands on its
// `default_stream`. So the stream passed to Enqueue is not an instruction
// here, only an expectation — and one worth checking, because a missing route
// is otherwise invisible: the pitch succeeds, the panel stays empty.
type HTTPPitcher struct {
	cfg    HTTPPitcherConfig
	client *http.Client

	warnOnce sync.Once
}

// NewHTTPPitcher builds a pitcher against an omni-pitcher, or nil when no URL
// is configured.
func NewHTTPPitcher(cfg HTTPPitcherConfig) *HTTPPitcher {
	if cfg.BaseURL == "" {
		return nil
	}
	cfg = cfg.withDefaults()

	return &HTTPPitcher{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout},
	}
}

// pitchResponse is what omni-pitcher answers with.
type pitchResponse struct {
	ObjectID string `json:"objectId"`
	StreamID string `json:"streamId"`
	Status   string `json:"status"`
	Message  string `json:"message"`
}

// Enqueue posts one message. The streamOverride is accepted for [Pitcher] but
// cannot be honoured — see the type documentation — so it is compared against
// what the server reports and a mismatch is logged once.
func (p *HTTPPitcher) Enqueue(ctx context.Context, msg homerun.Message, streamOverride ...string) (string, string, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return "", "", fmt.Errorf("encoding the message: %w", err)
	}

	url := strings.TrimRight(p.cfg.BaseURL, "/") + "/" + strings.TrimLeft(p.cfg.Path, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if p.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("pitching: %w", err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", "", p.statusError(resp)
	}

	var decoded pitchResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&decoded); err != nil {
		// The message is on the bus — the server said 200 — so this is not a
		// publish failure, only an unreadable receipt.
		p.cfg.Logger.Warn("could not read the pitch response", "error", err)
		return "", "", nil
	}

	p.checkStream(decoded.StreamID, streamOverride...)
	return decoded.ObjectID, decoded.StreamID, nil
}

// statusError turns a non-2xx into an error that says something useful. A 401
// in particular is a configuration mistake, not a transient fault, and saying
// so saves reading it as "the bus is down".
func (p *HTTPPitcher) statusError(resp *http.Response) error {
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	trimmed := strings.TrimSpace(string(detail))

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("omni-pitcher rejected the token (%s): check the pitch token", resp.Status)
	case http.StatusBadRequest:
		return fmt.Errorf("omni-pitcher rejected the message (%s): %s", resp.Status, trimmed)
	default:
		if trimmed != "" {
			return fmt.Errorf("omni-pitcher answered %s: %s", resp.Status, trimmed)
		}
		return fmt.Errorf("omni-pitcher answered %s", resp.Status)
	}
}

// checkStream warns once if the score is not landing where the panel is
// looking.
//
// Without a route for our system, omni-pitcher puts every message on its
// default stream. The pitch then succeeds and the panel — switched to the
// match stream for the duration of the match — shows nothing at all. That is a
// hard failure to find from the outside, so it is worth one line in the log.
func (p *HTTPPitcher) checkStream(got string, streamOverride ...string) {
	want := p.cfg.ExpectStream
	if want == "" && len(streamOverride) > 0 {
		want = streamOverride[0]
	}
	if want == "" || got == "" || got == want {
		return
	}

	p.warnOnce.Do(func() {
		p.cfg.Logger.Warn("the score is landing on a different stream than the panel is watching",
			"expected", want, "actual", got,
			"hint", "omni-pitcher needs a route for this system, otherwise everything goes to its default stream")
	})
}

// Close releases the client's idle connections. There is no session to end.
func (p *HTTPPitcher) Close() error {
	p.client.CloseIdleConnections()
	return nil
}
