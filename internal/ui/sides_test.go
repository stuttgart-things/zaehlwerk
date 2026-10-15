package ui

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/checkin"
)

// desk stands in for *checkin.Desk: who holds which half, set by the test.
type desk struct {
	mu     sync.Mutex
	claims [2]*checkin.View
}

func (d *desk) Claims() [2]*checkin.View {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.claims
}

func (d *desk) set(left, right string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.claims = [2]*checkin.View{}
	for i, name := range []string{left, right} {
		if name != "" {
			d.claims[i] = &checkin.View{Name: name}
		}
	}
}

const spBase = "https://schmetterpause.example.com"

// clock is a time the test moves.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestAFreeTableShowsACodePerHalf(t *testing.T) {
	f := newFixture(t, WithSides(&desk{}, true, spBase+"/"))

	rec, body := f.do(http.MethodGet, "/ui/watch", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, body, "Pick your side")
	require.Equal(t, 2, strings.Count(body, `<svg class="qr"`), "a code per half")
	// The text under each code is the link without its scheme, and the
	// trailing slash of the base does not double up.
	require.Contains(t, body, "schmetterpause.example.com/table/left")
	require.Contains(t, body, "schmetterpause.example.com/table/right")
	require.NotContains(t, body, "com//table")
	require.Contains(t, body, `hx-get="/ui/watch/sides"`)
	require.Contains(t, body, "hold your fob", "the hub takes fobs too")
	require.NotContains(t, body, "No match is running")
}

// A screen on the wall, with the side select on it: still nothing to press.
func TestTheSideSelectHasNothingToPress(t *testing.T) {
	f := newFixture(t, WithSides(&desk{}, true, spBase))

	for _, path := range []string{"/ui/watch", "/ui/watch/sides"} {
		_, body := f.do(http.MethodGet, path, nil)
		require.NotContains(t, body, "hx-post", path)
		require.NotContains(t, body, "<button", path)
		require.NotContains(t, body, "<form", path)
	}
}

// Without the phone path the codes would lead to a page that cannot claim a
// half, so there are none; the columns and the fob hint stay.
func TestWithOnlyFobsThereAreNoCodes(t *testing.T) {
	f := newFixture(t, WithSides(&desk{}, true, ""))

	_, body := f.do(http.MethodGet, "/ui/watch", nil)

	require.Contains(t, body, "Pick your side")
	require.NotContains(t, body, `class="qr"`)
	require.NotContains(t, body, "/table/")
	require.Contains(t, body, "Hold your fob to the reader")
}

func TestWithoutCheckInTheScreenIsAsItWas(t *testing.T) {
	f := newFixture(t, WithSides(nil, true, spBase))

	_, body := f.do(http.MethodGet, "/ui/watch", nil)
	require.Contains(t, body, "No match is running")
	require.NotContains(t, body, "Pick your side")

	rec, _ := f.do(http.MethodGet, "/ui/watch/sides", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAClaimShowsInItsColumn(t *testing.T) {
	d := &desk{}
	f := newFixture(t, WithSides(d, true, spBase))

	d.set("Anna", "")
	_, body := f.do(http.MethodGet, "/ui/watch/sides", nil)

	require.Contains(t, body, "Waiting for an opponent")
	require.Contains(t, body, `<section class="half taken">`)
	left, right, _ := strings.Cut(body, `<section class="half">`)
	require.Contains(t, left, "Anna")
	require.NotContains(t, left, "/table/left", "a taken half needs no code")
	require.Contains(t, right, "/table/right", "the free half keeps its code")

	// Moving is the name jumping columns.
	d.set("", "Anna")
	_, body = f.do(http.MethodGet, "/ui/watch/sides", nil)
	left, right, _ = strings.Cut(body, `<section class="half taken">`)
	require.NotContains(t, left, "Anna")
	require.Contains(t, right, "Anna")
}

// Two claims start a match; the next poll sends the screen to the score.
func TestTheSideSelectMakesWayForTheMatch(t *testing.T) {
	f := newFixture(t, WithSides(&desk{}, true, spBase))
	f.match("Anna", "Bernd")

	rec, _ := f.do(http.MethodGet, "/ui/watch/sides", nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, "/ui/watch", rec.Header().Get("HX-Redirect"))

	_, body := f.do(http.MethodGet, "/ui/watch", nil)
	require.NotContains(t, body, "Pick your side")
	require.Contains(t, body, "Anna")
}

// The final score stays up for the hold, then the side select comes back.
func TestAFinishedMatchStaysUpForTheHold(t *testing.T) {
	c := &clock{now: time.Date(2026, 10, 15, 6, 0, 0, 0, time.UTC)}
	f := newFixture(t, WithSides(&desk{}, true, spBase), WithClock(c.Now), WithResultHold(time.Minute))
	m := f.match("Anna", "Bernd")
	f.do(http.MethodPost, "/ui/matches/"+m.ID+"/end", nil)

	next := "/ui/watch/next?match=" + m.ID
	rec, _ := f.do(http.MethodGet, next, nil)
	require.Empty(t, rec.Header().Get("HX-Redirect"), "the score just ended")

	c.add(59 * time.Second)
	rec, _ = f.do(http.MethodGet, next, nil)
	require.Empty(t, rec.Header().Get("HX-Redirect"), "still within the hold")

	c.add(time.Second)
	rec, _ = f.do(http.MethodGet, next, nil)
	require.Equal(t, "/ui/watch", rec.Header().Get("HX-Redirect"))

	_, body := f.do(http.MethodGet, "/ui/watch", nil)
	require.Contains(t, body, "Pick your side")
}

// Somebody checking in wants to see their name, not the last match's score.
func TestACheckInCutsTheHoldShort(t *testing.T) {
	d := &desk{}
	f := newFixture(t, WithSides(d, true, spBase))
	m := f.match("Anna", "Bernd")
	f.do(http.MethodPost, "/ui/matches/"+m.ID+"/end", nil)

	next := "/ui/watch/next?match=" + m.ID
	rec, _ := f.do(http.MethodGet, next, nil)
	require.Empty(t, rec.Header().Get("HX-Redirect"))

	d.set("", "Carla")
	rec, _ = f.do(http.MethodGet, next, nil)
	require.Equal(t, "/ui/watch", rec.Header().Get("HX-Redirect"))
}

// The hold records are bounded: a caller making up ids cannot grow them.
func TestTheHoldRecordsAreBounded(t *testing.T) {
	sd := &sides{desk: &desk{}, over: map[string]time.Time{}}
	now := time.Now()
	for i := range 10 * maxOver {
		sd.held(strings.Repeat("x", i+1), now, time.Minute)
	}
	require.LessOrEqual(t, len(sd.over), maxOver)
}
