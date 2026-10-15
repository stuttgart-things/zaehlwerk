package ui

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// nav returns the scoring page's navigation, and nothing else of the page.
func nav(t *testing.T, f *fixture) string {
	t.Helper()
	_, body := f.do(http.MethodGet, "/ui", nil)
	_, after, ok := strings.Cut(body, `<nav class="services"`)
	require.True(t, ok, "the page has its navigation")
	n, _, ok := strings.Cut(after, "</nav>")
	require.True(t, ok)
	return n
}

func TestTheNavigationLinksTheServicesThatAreDeployed(t *testing.T) {
	f := newFixture(t,
		WithServiceLinks("https://buttons.example.com/", "https://pong.example.com"),
		WithReportsTo("https://schmetterpause.example.com"))

	n := nav(t, f)

	require.Contains(t, n, `href="/ui/watch"`)
	require.Contains(t, n, `href="https://buttons.example.com"`, "without the trailing slash")
	require.Contains(t, n, `href="https://pong.example.com/"`, "the lobby")
	require.Contains(t, n, `href="https://schmetterpause.example.com"`)
	require.Equal(t, 4, strings.Count(n, `target="_blank"`), "each in a new tab: this one is keeping score")
}

// A service that is not deployed has no link: a link to nothing is worse than
// none.
func TestWithoutTheServicesOnlyTheWatchScreenIsLinked(t *testing.T) {
	f := newFixture(t, WithServiceLinks("", ""))

	n := nav(t, f)

	require.Contains(t, n, `href="/ui/watch"`)
	require.Equal(t, 1, strings.Count(n, "<a "))
	require.NotContains(t, n, "Buttons")
	require.NotContains(t, n, "Pong")
	require.NotContains(t, n, "Schmetterpause")
}

// The page is open on the office network: a password somebody put in an
// address does not go out with it.
func TestALinkCarriesNoPassword(t *testing.T) {
	f := newFixture(t, WithServiceLinks("https://admin:hunter2@buttons.example.com", ""))

	require.NotContains(t, nav(t, f), "hunter2")
}
