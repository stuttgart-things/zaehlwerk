package schmetterpause

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const svg = `<svg viewBox="0 0 420 420" data-face="neutral" style="--paddle:#159c8c"></svg>`

func figureServer(t *testing.T, contentType, body string) (*Client, *string, *string) {
	t.Helper()
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, Token: "tok"})
	require.NoError(t, err)
	return c, &path, &auth
}

// The figure routes are public there, and the token is a key to the results:
// it stays at home when nothing needs it.
func TestAPlayersFigureIsFetchedWithoutTheToken(t *testing.T) {
	c, path, auth := figureServer(t, "image/svg+xml", svg)

	got, err := c.Figure(context.Background(), "7f1c2a90-3b4d")
	require.NoError(t, err)
	require.Equal(t, svg, string(got))
	require.Equal(t, "/avatars/7f1c2a90-3b4d.svg", *path)
	require.Empty(t, *auth)
}

func TestASideWithNobodyBehindItGetsARandomFigure(t *testing.T) {
	c, path, _ := figureServer(t, "image/svg+xml; charset=utf-8", svg)

	_, err := c.Figure(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, "/avatars/random.svg", *path)
}

// Anything that is not an SVG is refused here, so the screen falls back to its
// own mascot rather than showing an error page as a picture.
func TestAnAnswerThatIsNoFigureIsRefused(t *testing.T) {
	for name, tc := range map[string]struct{ contentType, body string }{
		"an HTML page":      {"text/html", "<!doctype html><p>not found</p>"},
		"a mislabelled one": {"image/svg+xml", "<!doctype html><p>not found</p>"},
		"a huge one":        {"image/svg+xml", "<svg>" + strings.Repeat(" ", figureCap) + "</svg>"},
	} {
		t.Run(name, func(t *testing.T) {
			c, _, _ := figureServer(t, tc.contentType, tc.body)
			_, err := c.Figure(context.Background(), "p1")
			require.Error(t, err)
		})
	}
}
