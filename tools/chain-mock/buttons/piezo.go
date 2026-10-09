package buttons

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/stuttgart-things/zaehlwerk/tools/chain-mock/piezo"
)

// piezoClient talks to a piezo board's control endpoint (PIEZO_CONTROL_ADDR
// there, PIEZO_CONTROL_URL here), so the table can be run from one page: the
// board paused while somebody tries the buttons, or its faults turned up.
//
// Optional in the way every coupling of zaehlwerk is: unset, the page has no
// piezo section and nothing else changes.
type piezoClient struct {
	url  string
	http *http.Client
}

func newPiezoClient(url string) *piezoClient {
	if url == "" {
		return nil
	}
	return &piezoClient{url: url, http: &http.Client{Timeout: 3 * time.Second}}
}

func (c *piezoClient) status(ctx context.Context) (piezo.Status, error) {
	return c.do(ctx, http.MethodGet, nil)
}

func (c *piezoClient) set(ctx context.Context, ctl piezo.Control) (piezo.Status, error) {
	return c.do(ctx, http.MethodPost, ctl)
}

func (c *piezoClient) do(ctx context.Context, method string, body any) (piezo.Status, error) {
	var st piezo.Status

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return st, fmt.Errorf("encoding the piezo control: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+"/control", reader)
	if err != nil {
		return st, fmt.Errorf("building %s /control: %w", method, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return st, fmt.Errorf("the piezo board: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return st, fmt.Errorf("reading the piezo board's answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var refusal struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &refusal)
		return st, fmt.Errorf("the piezo board refused: %d %s", resp.StatusCode, refusal.Error)
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("decoding the piezo board's answer: %w", err)
	}
	return st, nil
}
