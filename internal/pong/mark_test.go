package pong

import (
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// The game is Zählwerk's too: every page names its mark as the favicon, and
// the mark is the scoring page's own file.
func TestEveryPageCarriesZaehlwerksMark(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)

	for _, path := range []string{"/", "/join", "/screensaver"} {
		page := mustGet(t, p.url+path)
		require.Contains(t, page, `<link rel="icon" type="image/png" sizes="32x32" href="/static/mark-32.png">`, path)
		require.Contains(t, page, `<link rel="apple-touch-icon" href="/static/mark-180.png">`, path)
	}

	for _, name := range []string{"mark-32.png", "mark-180.png"} {
		resp, err := http.Get(p.url + "/static/" + name)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, name)
		require.Equal(t, "image/png", resp.Header.Get("Content-Type"), name)
		require.Equal(t, "\x89PNG", string(body[:4]), name)
	}
}
