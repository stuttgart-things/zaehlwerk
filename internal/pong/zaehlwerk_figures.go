package pong

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

// figureCap bounds one figure, as zaehlwerk bounds what it takes from
// Schmetterpause: around 40 KB each, and ten times that is a wrong answer.
const figureCap = 512 << 10

// Figure fetches one side's figure of a match from zaehlwerk's spectator view,
// the drawing zaehlwerk shows and resolves the panel's colours from: so a
// guest's random figure is the same one here, there and on the matrix. key is
// "<match id>/<side>", side a or b, which is how the board's figure cache
// names a side (Server.figuresOf).
//
// zaehlwerk answers with its mascot, not to be kept, when its own fetch from
// Schmetterpause failed or is still running; that is no figure, and an error
// here, so the board shows its own mascot for that side.
func (c *Client) Figure(ctx context.Context, key string) ([]byte, error) {
	matchID, side, ok := strings.Cut(key, "/")
	if !ok || matchID == "" || (side != "a" && side != "b") {
		return nil, fmt.Errorf("figure %q: want <match id>/a or /b", key)
	}
	u := c.base + "/ui/matches/" + url.PathEscape(matchID) + "/figures/" + side + ".svg"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("building the figure request: %w", err)
	}
	// The stream's client, without a timeout of its own: zaehlwerk waits for
	// its fetch before it answers, and ctx is the figure cache's bound.
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching a figure from zaehlwerk: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching a figure from zaehlwerk: %s", resp.Status)
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "image/svg+xml" {
		return nil, fmt.Errorf("zaehlwerk sent a figure as %q, not image/svg+xml", mt)
	}
	if resp.Header.Get("Cache-Control") == "no-store" {
		return nil, fmt.Errorf("zaehlwerk has no figure for side %s of match %s", side, matchID)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, figureCap+1))
	if err != nil {
		return nil, fmt.Errorf("reading a figure: %w", err)
	}
	if len(body) > figureCap {
		return nil, fmt.Errorf("a figure from zaehlwerk is over %d bytes", figureCap)
	}
	return body, nil
}
