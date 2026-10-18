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

	require.Contains(t, n, `action="https://pong.example.com/ui/demo" method="post"`, "Demo starts a game, so it posts")
	require.Contains(t, n, `href="https://pong.example.com/#new-game"`, "Pong lands on the lobby's form")
	require.Contains(t, n, `href="https://buttons.example.com"`, "without the trailing slash")
	require.Contains(t, n, `href="https://schmetterpause.example.com"`)
	require.Equal(t, 4, strings.Count(n, `target="_blank"`), "each in a new tab: this one is keeping score")
}

// Three ways to look at one game were one too many: the screens are set up
// once on a screen, by their address, and not picked from this page.
func TestTheSpectatorScreensAreNotInTheNavigation(t *testing.T) {
	f := newFixture(t, WithServiceLinks("https://buttons.example.com", "https://pong.example.com"))

	n := nav(t, f)

	require.NotContains(t, n, "/ui/watch")
	require.NotContains(t, n, "/ui/live")
}

// A service that is not deployed has no link: a link to nothing is worse than
// none. With none deployed there is no navigation at all.
func TestWithoutTheServicesThereIsNoNavigation(t *testing.T) {
	f := newFixture(t, WithServiceLinks("", ""))

	_, body := f.do(http.MethodGet, "/ui", nil)

	require.NotContains(t, body, `<nav class="services"`)
	require.NotContains(t, body, "/ui/demo")
}

// The page is open on the office network: a password somebody put in an
// address does not go out with it.
func TestALinkCarriesNoPassword(t *testing.T) {
	f := newFixture(t, WithServiceLinks("https://admin:hunter2@buttons.example.com", ""))

	require.NotContains(t, nav(t, f), "hunter2")
}
