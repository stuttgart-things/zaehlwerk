package pong

import (
	"context"
	"errors"
	"time"
)

// Attract mode: a free table starts a headless bot match by itself after a
// while, so the panel and the page have something to show at a fair or in the
// office. It never takes the table from anybody:
//
//   - The wait starts at a no_match on /live/stream and nowhere else. Any
//     match becoming current stops it, and only the next no_match starts it
//     again — so after a demo is displaced by a real match, the clock runs
//     only once that match is over, and after a demo is won there is a whole
//     wait before the next one. Demo and free table take turns.
//   - A match somebody left running is never ended. It blocks the demo, and
//     says so once. The one exception is a demo that is not ours, left behind
//     by a pod that is gone (ADR-0008): once it has been quiet for the whole
//     wait it is ended, and the wait starts again at the no_match after it.
//   - A game somebody asks for always wins: start ends a running demo before
//     it starts the new game, under the same lock every start takes.

// errYielded is a demo ended because somebody started a game.
var errYielded = errors.New("gave the table to a new game")

// errIntruded is a demo ended because zaehlwerk's score parted from the truth.
// A demo has no faults, so nothing the game did explains that: somebody else is
// counting into its match — buttons pressed during a demo. The other case it
// was written for, a real match started in the instant between the demo seeing
// a free table and creating its own, zaehlwerk closes itself since ADR-0008 by
// refusing the demo. Ending the demo's match makes the newest running match
// current again, or the table free.
var errIntruded = errors.New("somebody else is counting into the demo")

// AttractStatus is where attract mode stands, for the lobby.
type AttractStatus struct {
	// On is attract mode configured at all.
	On bool
	// After is the wait on a free table.
	After time.Duration
	// Next is when the next demo starts if the table stays free; zero while
	// it is not free.
	Next time.Time
	// Blind is the table not seen: /live/stream is not connected.
	Blind bool
}

// attractState is what the table watch shares with the lobby.
type attractState struct {
	next  time.Time
	blind bool
}

// Start begins what the service does without being asked: the table watch
// for attract mode, when it is configured. Separate from New so a caller has
// the server fully set up before anything runs on it.
func (s *Server) Start() {
	if s.cfg.AttractAfter <= 0 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.watchTable(s.ctx)
	}()
}

// Attract says where attract mode stands.
func (s *Server) Attract() AttractStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return AttractStatus{
		On:    s.cfg.AttractAfter > 0,
		After: s.cfg.AttractAfter,
		Next:  s.attract.next,
		Blind: s.attract.blind,
	}
}

func (s *Server) setAttract(next time.Time, blind bool) {
	s.mu.Lock()
	s.attract = attractState{next: next, blind: blind}
	s.mu.Unlock()
}

// The pauses before /live/stream is connected again, doubling from the first
// to the last.
const (
	watchRetryFirst = time.Second
	watchRetryLast  = 30 * time.Second
)

// watchTable stays on /live/stream for as long as the service runs, and
// connects again when the stream breaks. It listens; it does not poll.
func (s *Server) watchTable(ctx context.Context) {
	s.log.Info("attract mode on", "after", s.cfg.AttractAfter.String(), "pace", s.cfg.AttractPace.String())
	retry := watchRetryFirst
	lost := false
	for ctx.Err() == nil {
		table, err := s.client.Follow(ctx)
		if err == nil {
			if lost {
				s.log.Info("attract mode sees the table again")
				lost = false
			}
			retry = watchRetryFirst
			s.followTable(ctx, table)
		} else if !lost {
			// Once per outage, not once per attempt: a zaehlwerk that is
			// down for an hour is one line, not a hundred and twenty.
			s.log.Warn("attract mode cannot see the table, no demo until it can", "error", err)
			lost = true
		}
		// Blind, the table is not known to be free, and nothing starts.
		s.setAttract(time.Time{}, true)
		if sleep(ctx, retry) != nil {
			return
		}
		retry = min(2*retry, watchRetryLast)
	}
}

// followTable acts on one connection to /live/stream until it closes.
func (s *Server) followTable(ctx context.Context, table <-chan TableEvent) {
	after := s.cfg.AttractAfter
	// wait fires when the table has been free for long enough; quiet when the
	// current match has had no event for as long.
	wait := time.NewTimer(time.Hour)
	quiet := time.NewTimer(time.Hour)
	wait.Stop()
	quiet.Stop()
	defer wait.Stop()
	defer quiet.Stop()

	arm := func() {
		wait.Reset(after)
		s.setAttract(time.Now().Add(after), false)
	}
	// current is the match the table shows, and demo whether it is one; told
	// is the last stale one the log has named, so a match left running for a
	// day is one line.
	var current, told string
	var demo bool

	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-table:
			if !ok {
				return
			}
			if ev.Kind == kindNoMatch {
				current = ""
				quiet.Stop()
				arm()
				continue
			}
			if ev.State == nil {
				continue
			}
			// Any match current is a table that is not free: the demo's own,
			// a browser game's, or somebody's.
			wait.Stop()
			s.setAttract(time.Time{}, false)
			current, demo = ev.State.MatchID, ev.Demo
			quiet.Reset(after)

		case <-wait.C:
			s.startDemo(arm)

		case <-quiet.C:
			if current == "" || current == told || s.ownsMatch(current) {
				continue
			}
			if demo {
				// A demo is nobody's result, and one quiet for the whole wait
				// has nobody playing it: a pod that is gone left it, or
				// another instance whose game nobody plays. Ended, the table
				// sends no_match, and the wait starts again from there.
				if _, err := s.client.EndMatch(ctx, current); err != nil {
					s.log.Warn("could not end a quiet demo that is not ours, trying again after the next wait",
						"match_id", current, "error", err)
					quiet.Reset(after)
					continue
				}
				s.log.Info("a demo that is not ours was quiet for the whole wait, ended it",
					"match_id", current, "quiet", after.String())
				continue
			}
			// Never ended, whatever it is: a match nobody scores any more
			// looks exactly like one paused between two points, and only the
			// people at the table can tell which.
			s.log.Info("a match has been quiet at the table for the whole wait, no demo while it is current",
				"match_id", current, "quiet", after.String())
			told = current
		}
	}
}

// startDemo starts a headless bot match on a table that has been free for the
// whole wait. Busy is not an error: a game started from the lobby got there
// first, and its match will be on the stream. Any other refusal waits again,
// since no event may come to start the clock otherwise.
func (s *Server) startDemo(again func()) {
	g, err := s.start(GameConfig{
		Attract: true,
		Pace:    s.cfg.AttractPace,
		// Always off, whatever PONG_FAULTS says: a demo is for watching, and
		// a fault in it would show a passer-by a score that is wrong on
		// purpose.
		Faults: Faults{},
	})
	switch {
	case err == nil:
		s.log.Info("the table was free for the whole wait, demo started", "game", g.ID, "match_id", g.matchIDNow())
	case errors.Is(err, errBusy):
		s.log.Info("no demo, the table is taken", "reason", err.Error())
		again()
	default:
		s.log.Warn("could not start a demo, waiting again", "error", err)
		again()
	}
}

// yield ends a running demo, so that next can have the table. Called with
// s.starting held: no other start comes between the demo stopping and the new
// game taking its place, and the demo cannot start again in between because
// starting it takes the same lock.
//
// A game somebody asked for ends either kind of demo. A screensaver ends only
// attract mode's: two screens at one table would otherwise take the table
// from each other in turn, and the one that is playing keeps it (ADR-0013).
func (s *Server) yield(next GameConfig) {
	s.mu.Lock()
	g := s.running
	s.mu.Unlock()
	if g == nil || !g.cfg.demo() || (next.Screensaver && g.cfg.Screensaver) {
		return
	}
	g.stop(errYielded)
	select {
	case <-g.Done():
	case <-time.After(10 * time.Second):
		// Left as it is: the new game is then refused as busy, which is the
		// honest answer, rather than started next to a demo still playing.
		s.log.Warn("the demo did not stop in time", "game", g.ID)
		return
	}
	// Done closes before the goroutine that ran the demo lets go of it, so it
	// is let go of here; that goroutine only clears s.running if it is still
	// the demo.
	s.mu.Lock()
	if s.running == g {
		s.running = nil
	}
	s.mu.Unlock()
}

// ownsMatch says a match is one of the service's own games.
func (s *Server) ownsMatch(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running != nil && s.running.matchIDNow() == id {
		return true
	}
	for _, g := range s.games {
		if g.matchIDNow() == id {
			return true
		}
	}
	return false
}
