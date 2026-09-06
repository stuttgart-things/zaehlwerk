package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// Defaults for StreamConfig.
const (
	DefaultIdleTimeout    = 20 * time.Minute
	DefaultRequestTimeout = 5 * time.Second
	// idleCheckInterval is how often the failsafe looks at the clock. It only
	// has to be fine enough that "20 minutes" is not "20 minutes and a bit".
	idleCheckInterval = 30 * time.Second
)

// DefaultMatchStreams and DefaultIdleStreams are the two sets the panel moves
// between: ours alone while a match runs, the shared notification stream
// otherwise.
var (
	DefaultMatchStreams = []string{"tabletennis"}
	DefaultIdleStreams  = []string{"messages"}
)

// StreamConfig configures a Switcher.
type StreamConfig struct {
	// BaseURL is the catcher, e.g. http://led-catcher:8080. Empty disables
	// switching entirely — a local run should not need a catcher.
	BaseURL string
	// MatchStreams is what the catcher subscribes to for the length of a
	// match; IdleStreams is what it goes back to.
	MatchStreams []string
	IdleStreams  []string
	// IdleTimeout releases the panel after this long without a point, so a
	// match nobody finished does not leave a dead score up forever.
	IdleTimeout    time.Duration
	RequestTimeout time.Duration

	Logger *slog.Logger
	Now    func() time.Time
}

func (c StreamConfig) withDefaults() StreamConfig {
	if len(c.MatchStreams) == 0 {
		c.MatchStreams = slices.Clone(DefaultMatchStreams)
	}
	if len(c.IdleStreams) == 0 {
		c.IdleStreams = slices.Clone(DefaultIdleStreams)
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = DefaultIdleTimeout
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = DefaultRequestTimeout
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Switcher hands the panel to a match for its duration and gives it back
// afterwards, implementing ADR-0003.
//
// It gives it back only if it still holds it. Two parties can switch the
// catcher — this service and whoever has its UI open — and a set we did not
// establish belongs to someone who made a deliberate choice. Before reverting,
// the current set is read back and compared with what we left; anything else
// means we were overridden, and we leave it alone.
//
// Nothing here blocks the scorer. Every switch happens on a worker, and a
// failure is logged and dropped: if the catcher is unreachable the score still
// reaches the stream, it just competes with other events on the panel.
type Switcher struct {
	cfg    StreamConfig
	client *http.Client

	mu       sync.Mutex
	held     bool
	matchID  string
	lastSeen time.Time

	work      chan func()
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// NewSwitcher starts a switcher, or returns nil when no catcher is configured.
// A nil *Switcher is usable: every method is a no-op on it, so callers do not
// have to branch.
func NewSwitcher(cfg StreamConfig) *Switcher {
	if cfg.BaseURL == "" {
		return nil
	}
	cfg = cfg.withDefaults()

	s := &Switcher{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.RequestTimeout},
		work:   make(chan func(), 8),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go s.run()
	return s
}

// Start gives the panel to a match. Returns immediately; the switch happens on
// the worker.
func (s *Switcher) Start(matchID string) {
	if s == nil {
		return
	}
	s.submit(func() { s.acquire(matchID) })
}

// Release gives the panel back, if this match is the one holding it. A stale
// end for a match that has already been superseded must not take the panel
// away from the one running now.
func (s *Switcher) Release(matchID string) {
	if s == nil {
		return
	}
	s.submit(func() { s.release(matchID, "match ended") })
}

// Observe is the scorer observer: it keeps the inactivity clock honest and
// gives the panel back when a match is won.
func (s *Switcher) Observe(t scorer.Transition) {
	if s == nil {
		return
	}

	s.mu.Lock()
	if s.held && s.matchID == t.State.MatchID {
		s.lastSeen = s.cfg.Now()
	}
	s.mu.Unlock()

	if t.Kind == scorer.TransitionMatchWon {
		matchID := t.State.MatchID
		s.submit(func() { s.release(matchID, "match won") })
	}
}

// Close releases the panel if we still hold it, and stops the worker.
func (s *Switcher) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		// Stop the worker first and wait for it, so an acquire that is in
		// flight has finished recording itself. Reading `held` before that
		// could catch the gap between a successful switch and it being marked
		// as ours, and shutdown would then leave the panel on a score that had
		// already stopped moving. The wait is bounded by RequestTimeout,
		// because that is all the worker can be busy with.
		close(s.stop)
		<-s.done

		s.mu.Lock()
		matchID, held := s.matchID, s.held
		s.mu.Unlock()

		if held {
			// Synchronously: the worker is gone, and the process is going with
			// it, so there is nothing left to hand this to.
			s.release(matchID, "shutting down")
		}
	})
	return nil
}

// Held reports whether we currently believe we own the catcher's stream set.
func (s *Switcher) Held() (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.matchID, s.held
}

func (s *Switcher) submit(fn func()) {
	select {
	case <-s.stop:
		return
	default:
	}

	select {
	case s.work <- fn:
	default:
		// The queue only fills if the catcher is slow enough that switches are
		// piling up, in which case the panel is already not keeping up.
		s.cfg.Logger.Warn("dropping a panel stream switch, the queue is full")
	}
}

func (s *Switcher) run() {
	defer close(s.done)

	idle := time.NewTicker(idleCheckInterval)
	defer idle.Stop()

	for {
		select {
		case fn := <-s.work:
			fn()
		case <-idle.C:
			s.checkIdle()
		case <-s.stop:
			return
		}
	}
}

// acquire points the catcher at our stream and records that we hold it.
func (s *Switcher) acquire(matchID string) {
	if err := s.setStreams(s.cfg.MatchStreams); err != nil {
		// Deliberately not fatal, and deliberately not recorded as held: if we
		// did not switch it, we must not switch it back.
		s.cfg.Logger.Warn("could not give the panel to the match, the score will share it",
			"error", err, "match", matchID, "catcher", s.cfg.BaseURL)
		return
	}

	s.mu.Lock()
	s.held, s.matchID, s.lastSeen = true, matchID, s.cfg.Now()
	s.mu.Unlock()

	s.cfg.Logger.Info("panel switched to the match",
		"match", matchID, "streams", s.cfg.MatchStreams)
}

// release gives the panel back, but only if we still hold it and the catcher
// is still on the set we left it on.
func (s *Switcher) release(matchID, reason string) {
	s.mu.Lock()
	held, current := s.held, s.matchID
	s.mu.Unlock()

	if !held {
		return
	}
	if matchID != "" && matchID != current {
		// An end for a match that no longer owns the panel.
		return
	}

	streams, err := s.getStreams()
	if err != nil {
		// Unreachable. Forget the override rather than hold it forever: the
		// catcher is in an unknown state, and the inactivity failsafe would
		// only keep retrying a call that is not answering.
		s.forget()
		s.cfg.Logger.Warn("could not read the panel's streams back, giving up the override",
			"error", err, "match", current, "catcher", s.cfg.BaseURL)
		return
	}

	if !sameSet(streams, s.cfg.MatchStreams) {
		s.forget()
		s.cfg.Logger.Info("panel was switched by someone else, leaving it alone",
			"match", current, "streams", streams)
		return
	}

	if err := s.setStreams(s.cfg.IdleStreams); err != nil {
		s.forget()
		s.cfg.Logger.Warn("could not give the panel back",
			"error", err, "match", current, "catcher", s.cfg.BaseURL)
		return
	}

	s.forget()
	s.cfg.Logger.Info("panel given back",
		"match", current, "reason", reason, "streams", s.cfg.IdleStreams)
}

func (s *Switcher) forget() {
	s.mu.Lock()
	s.held, s.matchID = false, ""
	s.mu.Unlock()
}

// checkIdle is the failsafe: a match that nobody finished should not hold the
// panel indefinitely, showing a score that stopped moving hours ago.
func (s *Switcher) checkIdle() {
	s.mu.Lock()
	held, matchID, last := s.held, s.matchID, s.lastSeen
	s.mu.Unlock()

	if !held || s.cfg.Now().Sub(last) < s.cfg.IdleTimeout {
		return
	}
	s.cfg.Logger.Info("no point for longer than the idle timeout, giving the panel back",
		"match", matchID, "idle_for", s.cfg.Now().Sub(last))
	s.release(matchID, "idle")
}

type streamsBody struct {
	Streams []string `json:"streams"`
}

func (s *Switcher) setStreams(streams []string) error {
	body, err := json.Marshal(streamsBody{Streams: streams})
	if err != nil {
		return fmt.Errorf("encoding the stream set: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.BaseURL+"/streams", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("posting streams: %w", err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("catcher answered %s to a stream switch", resp.Status)
	}
	return nil
}

// getStreams reads the catcher's current subscription.
//
// The catcher's GET /streams reports the set but not whether it is overridden,
// so ownership is decided by comparing the set itself with what we left. That
// is the sturdier check anyway: it looks at what the panel is actually doing
// rather than at a flag about it.
func (s *Switcher) getStreams() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.BaseURL+"/streams", nil)
	if err != nil {
		return nil, fmt.Errorf("building the request: %w", err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reading streams: %w", err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catcher answered %s to a stream read", resp.Status)
	}

	var body streamsBody
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decoding the stream set: %w", err)
	}
	return body.Streams, nil
}

func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
}

// sameSet compares two stream sets ignoring order, because the catcher is free
// to report them in whatever order it holds them.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}
