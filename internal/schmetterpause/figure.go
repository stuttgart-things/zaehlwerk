package schmetterpause

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
)

// figureCap bounds one figure. Schmetterpause's are around 40 KB: all six faces
// in one drawing. Ten times that is a figure that has grown a great deal, more
// is a wrong answer.
const figureCap = 512 << 10

// Figure fetches the figure a player assembled in Schmetterpause
// (Schmetterpause ADR-0029), or a random one when id is empty — a side with
// nobody behind it, a demo, Pong against itself. Random means a new one per
// call, so a caller that wants one per match fetches once per match.
//
// Public on that side, so no token goes with it: a figure is shown to anybody
// who opens Schmetterpause's start page anyway, and this call must work for an
// instance that has only SCHMETTERPAUSE_URL.
//
// What comes back is an SVG document carrying every face, the one shown chosen
// by data-face on its root and its colours as custom properties there. It is
// checked to be one — the type, and an <svg root — and returned as it came.
func (c *Client) Figure(ctx context.Context, id string) ([]byte, error) {
	path := "/avatars/random.svg"
	if id != "" {
		path = "/avatars/" + url.PathEscape(id) + ".svg"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, fmt.Errorf("schmetterpause: building the figure request: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("schmetterpause: fetching a figure: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("schmetterpause: fetching a figure: %w", statusError(resp))
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "image/svg+xml" {
		return nil, fmt.Errorf("schmetterpause: a figure came as %q, not image/svg+xml", mt)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, figureCap+1))
	if err != nil {
		return nil, fmt.Errorf("schmetterpause: reading a figure: %w", err)
	}
	if len(body) > figureCap {
		return nil, fmt.Errorf("schmetterpause: a figure is larger than %d bytes", figureCap)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(body), []byte("<svg")) {
		return nil, errors.New("schmetterpause: a figure is not an <svg> document")
	}
	return body, nil
}
