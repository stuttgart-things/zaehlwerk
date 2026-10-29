package buttons

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/face"
	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
)

// figures stands in for Schmetterpause's figure routes: each figure says whose
// it is, and an id in fail has none.
type figures struct {
	mu   sync.Mutex
	ids  []string
	fail map[string]bool
}

func (f *figures) Figure(_ context.Context, id string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids = append(f.ids, id)
	if f.fail[id] {
		return nil, errors.New("schmetterpause: 404")
	}
	return []byte(`<svg data-face="happy" data-who="` + id + `"></svg>`), nil
}

func (f *figures) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ids...)
}

// dressedPage is the page against zw, with the fake's roster and figs.
func dressedPage(t *testing.T, zw *httptest.Server, r *Rig, figs *figures) *httptest.Server {
	t.Helper()
	s := NewServer(r, zw.URL+"/ui", "", quiet).
		WithRoster(rosterFunc(func(context.Context) ([]schmetterpause.Player, error) { return roster(), nil }))
	if figs != nil {
		s.WithFigures(figs)
	}
	page := httptest.NewServer(s)
	t.Cleanup(page.Close)
	return page
}

func TestEachPlayersFobCarriesTheirFigure(t *testing.T) {
	zw, _, _ := checkedInTable(t)
	figs := &figures{}
	page := dressedPage(t, zw, rig(t, zw, nil), figs)
	anna := fob(t, "Anna's fob")

	_, body := getPage(t, page.URL+"/")
	require.Contains(t, body, `<img src="/figures/`+anna.Key+`.svg"`)
	require.Equal(t, len(roster()), strings.Count(body, `<img src="/figures/`), "the fobs that belong to nobody have none")

	resp, err := http.Get(page.URL + "/figures/" + anna.Key + ".svg")
	require.NoError(t, err)
	svg, _ := io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "image/svg+xml", resp.Header.Get("Content-Type"))
	require.Equal(t, "default-src 'none'; style-src 'unsafe-inline'", resp.Header.Get("Content-Security-Policy"))
	require.Contains(t, string(svg), `data-face="neutral" data-who="`+anna.Key+`"`, "nobody is playing yet")
}

// The route fetches the figures of the players on the page and nothing else.
func TestOnlyAPlayerOnThePageHasAFigure(t *testing.T) {
	zw, _, _ := checkedInTable(t)
	figs := &figures{}
	page := dressedPage(t, zw, rig(t, zw, nil), figs)
	getPage(t, page.URL+"/")

	for _, path := range []string{"/figures/" + SpareFob.Key + ".svg", "/figures/someone-else.svg", "/figures/" + fob(t, "Anna's fob").Key} {
		status, _ := getPage(t, page.URL+path)
		require.Equal(t, http.StatusNotFound, status, path)
	}
	require.Empty(t, figs.asked())
}

func TestAFigureThatFailedIsTheMascot(t *testing.T) {
	zw, _, _ := checkedInTable(t)
	anna := fob(t, "Anna's fob")
	page := dressedPage(t, zw, rig(t, zw, nil), &figures{fail: map[string]bool{anna.Key: true}})
	getPage(t, page.URL+"/")

	resp, err := http.Get(page.URL + "/figures/" + anna.Key + ".svg")
	require.NoError(t, err)
	svg, _ := io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(svg), `<svg id="mascot"`)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
}

func TestWithoutFiguresTheFobsAreWhatTheyWere(t *testing.T) {
	zw, _, _ := checkedInTable(t)
	page := dressedPage(t, zw, rig(t, zw, nil), nil)

	_, body := getPage(t, page.URL+"/")
	require.NotContains(t, body, `/figures/`)
}

// zaehlwerk names a checked-in player, and the page knows the name from its
// fobs.
func TestAHalfShowsWhoIsCheckedInThere(t *testing.T) {
	zw, _, _ := checkedInTable(t)
	r := rig(t, zw, nil)
	page := dressedPage(t, zw, r, &figures{})
	getPage(t, page.URL+"/")

	r.HoldFob(fob(t, "Anna's fob"))
	press(t, r, "A", 100*time.Millisecond)

	live := get(t, page.URL+"/live")
	require.Contains(t, live, `<img class="figure" src="/figures/`+fob(t, "Anna's fob").Key+`.svg"`)
	require.Contains(t, live, `<b>Anna</b>`)
	require.Contains(t, live, `nobody checked in`, "the right half is free")
}

// The running match's figures are zaehlwerk's: a side without a player has a
// random figure only zaehlwerk knows.
func TestTheRunningMatchShowsZaehlwerksFigures(t *testing.T) {
	zw := zaehlwerk(t)
	r := rig(t, zw, nil)
	page := dressedPage(t, zw, r, &figures{})
	id := current(t, r).MatchID

	live := get(t, page.URL+"/live")

	for _, side := range []string{"a", "b"} {
		require.Contains(t, live, `src="`+zw.URL+`/ui/matches/`+id+`/figures/`+side+`.svg?face=neutral"`)
	}
}

func TestTheFacesAreWhatTheTableStreamSaid(t *testing.T) {
	events := make(chan string)
	stream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		for {
			select {
			case ev := <-events:
				_, _ = fmt.Fprintf(w, "data: %s\n\n", ev)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	}))
	t.Cleanup(stream.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	faces := &tableFaces{}
	go faces.follow(ctx, stream.URL, quiet)
	neutral := face.Faces{face.Neutral, face.Neutral}

	events <- `{"kind":"point","state":{"match_id":"m1"},"faces":["happy","sad"]}`
	require.Eventually(t, func() bool { return faces.of("m1") == face.Faces{face.Happy, face.Sad} }, time.Second, time.Millisecond)
	require.Equal(t, neutral, faces.of("m2"), "another match has not been seen")

	events <- `{"kind":"no_match"}`
	require.Eventually(t, func() bool { return faces.of("m1") == neutral }, time.Second, time.Millisecond)
}
