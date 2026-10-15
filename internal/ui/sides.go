package ui

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/checkin"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/qr"
)

// DefaultResultHold is how long the watch screen leaves a finished match up
// before it goes back to the side select. Long enough to read the score from
// across the room and for the players to shake hands; a check-in in the
// meantime cuts it short.
const DefaultResultHold = time.Minute

// Halves is who holds which half of the table. [checkin.Desk] implements it.
type Halves interface {
	Claims() [2]*checkin.View
}

// sides is the side select on the watch screen (#128): a column per half,
// with who holds it and, when phones can check in, a QR code for the page
// that claims it.
type sides struct {
	desk Halves
	// fob says the hub can check a player in, so the page names the reader.
	fob bool
	// links are Schmetterpause's /table/left and /table/right, nil when the
	// phone path is off. Rendered once: the addresses do not change while
	// the process runs.
	links [2]*sideLink

	// over is when each match the screen was showing was first seen
	// finished, for the hold. Kept here rather than on the match because it
	// is only the screen's business, and a match won on court has no end
	// time of its own (only one ended by hand does). Seen by the screen's
	// own poll, so it is late by at most one poll.
	mu   sync.Mutex
	over map[string]time.Time
	// freeSince is when the table was last seen free with nobody holding a
	// half, for the screensaver; zero while it is not.
	freeSince time.Time
}

// sideLink is one half's way in by phone.
type sideLink struct {
	URL string
	// Text is the URL without its scheme: what somebody would type.
	Text   string
	QRPath string
	QRSize int
}

// maxOver bounds the hold records. One screen sees one match at a time; this
// is a bound on a caller sending made-up ids, not a number to reach.
const maxOver = 32

// WithSides turns on the side select: when no match is running, the watch
// screen shows a column per half with who holds it. fob says the hub takes
// fobs. phoneBase is Schmetterpause's base URL when the phone path is on, and
// then each column gets a QR code for <phoneBase>/table/<half>; empty
// leaves the codes out (ADR-0011: without Schmetterpause's token the page
// behind the code could not claim anything).
//
// A nil desk leaves the watch screen as it was.
func WithSides(desk Halves, fob bool, phoneBase string) Option {
	return func(s *Server) {
		if desk == nil {
			return
		}
		sd := &sides{desk: desk, fob: fob, over: map[string]time.Time{}}
		if base := strings.TrimRight(phoneBase, "/"); base != "" {
			for i, half := range []string{"left", "right"} {
				link, err := newSideLink(base + "/table/" + half)
				if err != nil {
					// Only a host name of hundreds of characters gets here.
					// The column then says how to check in without the code.
					s.log.Warn("the side select shows no QR code", "half", half, "error", err)
					continue
				}
				sd.links[i] = link
			}
		}
		s.sides = sd
	}
}

// WithResultHold sets how long a finished match stays on the watch screen
// before the side select comes back. Without the side select it changes
// nothing: the match then stays up until the next one starts.
func WithResultHold(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.resultHold = d
		}
	}
}

func newSideLink(target string) (*sideLink, error) {
	code, err := qr.Render(target)
	if err != nil {
		return nil, fmt.Errorf("render the code for %s: %w", target, err)
	}
	text := strings.TrimPrefix(strings.TrimPrefix(target, "https://"), "http://")
	return &sideLink{URL: target, Text: text, QRPath: code.Path, QRSize: code.Size}, nil
}

// sidesView is the side select as the template draws it.
type sidesView struct {
	Halves [2]halfView
	Fob    bool
	Phone  bool
	// Waiting is set when one half is taken: the screen asks for an
	// opponent.
	Waiting bool
	// Screensaver is the pong screensaver's address once the table has been
	// idle long enough, and the screen shows it instead of the columns.
	Screensaver string
}

type halfView struct {
	Label string
	// Name is who holds the half, empty when it is free.
	Name string
	Link *sideLink
}

func (sd *sides) view() *sidesView {
	claims := sd.desk.Claims()
	v := &sidesView{Fob: sd.fob, Phone: sd.links[0] != nil || sd.links[1] != nil}
	taken := 0
	for i, label := range []string{"Left", "Right"} {
		h := halfView{Label: label, Link: sd.links[i]}
		if c := claims[i]; c != nil {
			h.Name = c.Name
			taken++
		}
		v.Halves[i] = h
	}
	v.Waiting = taken == 1
	return v
}

// claimed reports whether anybody holds a half.
func (sd *sides) claimed() bool {
	c := sd.desk.Claims()
	return c[0] != nil || c[1] != nil
}

// held reports whether the finished match id is still within hold, and starts
// the hold the first time it is asked about it.
func (sd *sides) held(id string, now time.Time, hold time.Duration) bool {
	sd.mu.Lock()
	defer sd.mu.Unlock()

	since, ok := sd.over[id]
	if !ok {
		if len(sd.over) >= maxOver {
			clear(sd.over)
		}
		sd.over[id] = now
		since = now
	}
	return now.Sub(since) < hold
}

// DefaultScreensaverAfter is how long the side select waits on a free table
// before it hands the screen to the pong screensaver (#120). Long enough that
// a pair finding their fobs or unlocking their phones does not lose the codes
// to a bot game; short enough that a screen nobody uses does not sit still.
const DefaultScreensaverAfter = 3 * time.Minute

// WithScreensaver hands an idle side select to the pong screensaver at
// pongURL after the table has been free for after (ADR-0013). Empty pongURL
// leaves the side select up for as long as the table is free. Without the side
// select it changes nothing.
func WithScreensaver(pongURL string, after time.Duration) Option {
	return func(s *Server) {
		base := strings.TrimRight(pongURL, "/")
		if base == "" {
			return
		}
		if after <= 0 {
			after = DefaultScreensaverAfter
		}
		s.saver = &screensaver{url: base + "/screensaver", after: after}
	}
}

// screensaver is where an idle watch screen goes, and when.
type screensaver struct {
	url   string
	after time.Duration
}

// current is the running match the watch screen follows. With the side select
// on, a demo is not one: a free table shows the halves and their codes, not
// bots, and the screensaver is a demo the screen itself asked for. Without it
// the screen follows a demo as it always has.
func (s *Server) current() (*match.Match, bool) {
	m, err := s.registry.Current()
	if err != nil || (s.sides != nil && m.Demo) {
		return nil, false
	}
	return m, true
}

// idle reports whether the table has been free, with nobody holding a half,
// for the screensaver's wait, and starts or stops the clock for it. Shared by
// every screen: the table is free or it is not, whoever asks.
func (sd *sides) idle(now time.Time, after time.Duration) bool {
	claimed := sd.claimed()
	sd.mu.Lock()
	defer sd.mu.Unlock()
	if claimed {
		sd.freeSince = time.Time{}
		return false
	}
	if sd.freeSince.IsZero() {
		sd.freeSince = now
	}
	return now.Sub(sd.freeSince) >= after
}

// busy stops the screensaver's clock: a real match holds the table.
func (sd *sides) busy() {
	sd.mu.Lock()
	sd.freeSince = time.Time{}
	sd.mu.Unlock()
}

// sidesNow is the side select as it should be now: the columns, or the
// screensaver once the table has been idle long enough.
func (s *Server) sidesNow() *sidesView {
	v := s.sides.view()
	if s.saver != nil && s.sides.idle(s.now(), s.saver.after) {
		v.Screensaver = s.saver.url
	}
	return v
}

// watchSides answers the side select's poll: the columns as they are now, the
// screensaver once the table has been idle long enough, or a redirect to the
// score once a real match is running.
//
// Polled rather than pushed, like [Server.watchNext]: the hub carries a
// match's transitions, and a claim is not one. The screensaver's poll says
// so with ?saver, and while the screensaver should stay it gets a 204, which
// htmx does not swap: swapping it for itself would load the game again.
func (s *Server) watchSides(w http.ResponseWriter, r *http.Request) {
	if s.sides == nil {
		http.NotFound(w, r)
		return
	}
	if _, running := s.current(); running {
		s.sides.busy()
		w.Header().Set("HX-Redirect", "/ui/watch")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	v := s.sidesNow()
	if r.URL.Query().Has("saver") && v.Screensaver != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "sides", view{Sides: v})
}
