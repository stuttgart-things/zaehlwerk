package schmetterpause

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// Reporter hands a won match over to Schmetterpause.
//
// It hangs on the scorer observer, the same seam the panel sink uses, and
// filters TransitionMatchWon — the moment scorer.State.CompletedSets holds the
// whole match, which is what that field's comment has said it existed for
// since ADR-0001.
//
// It is not the panel sink, and the difference is deliberate. The sink drops a
// transition it cannot deliver and counts it: a point that never reaches the
// matrix is gone, and the next point covers for it. A result has no next
// point. ADR-0004 accepted that one can be lost rather than give this service
// persistence, and asked in exchange that the loss be visible — so this
// records the outcome on the match, and the page offers a retry for as long as
// retention holds it.
//
// It waits a grace period after match_won before it sends (ADR-0007). The
// match point pressed for the wrong player and undone a second later is the
// mis-press that really happens at a table, and a result already sent cannot
// be taken back: Schmetterpause has no route for it. So the undo has to come
// first, and the grace period is the time it has.
//
// It is handed the match with each transition and holds no registry: looking
// the match up from inside the scorer's lock could deadlock against a match
// being created (#102), and without a registry there is nothing to look it up
// in.
type Reporter struct {
	client *Client
	log    *slog.Logger
	now    func() time.Time
	// timeout bounds one handover. The observer does not wait for it: the post
	// runs in its own goroutine, because observers are called from inside the
	// scorer's lock and must not block (see scorer.Observe).
	timeout time.Duration
	// delay is the grace period. Zero sends at match_won, as before ADR-0007.
	delay time.Duration
	// done is closed-over only by tests, which need to wait for the goroutine
	// the observer starts.
	done func()

	// sending lets one handover through at a time, so a retry pressed while
	// the timer fires finds the match already reported instead of entering
	// it twice.
	sending sync.Mutex

	mu sync.Mutex
	// waiting is the matches won and not sent yet, by id.
	waiting map[string]*waiting
	// closed is set at shutdown: from then on nothing waits.
	closed bool
	// inflight counts the handovers that are waiting or under way, so that
	// shutdown can wait for them.
	inflight sync.WaitGroup
}

// waiting is one match in its grace period. Compared by pointer: a timer that
// fires after its match was won a second time is not that match's timer any
// more, and must not send.
type waiting struct {
	match *match.Match
	timer *time.Timer
}

// ReporterOption configures a Reporter.
type ReporterOption func(*Reporter)

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) ReporterOption {
	return func(r *Reporter) {
		if l != nil {
			r.log = l
		}
	}
}

// WithClock sets the clock used for played_at.
func WithClock(now func() time.Time) ReporterOption {
	return func(r *Reporter) {
		if now != nil {
			r.now = now
		}
	}
}

// DefaultDelay is the grace period: long enough to notice the match point went
// to the wrong player and undo it, short enough that the result is over there
// before the players have left the table.
const DefaultDelay = 15 * time.Second

// WithDelay sets the grace period between match_won and the report. Zero, or
// less, sends at once.
func WithDelay(d time.Duration) ReporterOption {
	return func(r *Reporter) { r.delay = max(d, 0) }
}

// withDone is called after each handover attempt. Tests only.
func withDone(fn func()) ReporterOption {
	return func(r *Reporter) { r.done = fn }
}

// NewReporter builds a Reporter around a Client.
func NewReporter(client *Client, opts ...ReporterOption) *Reporter {
	r := &Reporter{
		client:  client,
		log:     slog.Default(),
		now:     time.Now,
		timeout: DefaultTimeout,
		delay:   DefaultDelay,
		done:    func() {},
		waiting: make(map[string]*waiting),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Observe is the scorer observer. Register it with [match.WithMatchObserver],
// ahead of the live hub: the result card the hub renders at match_won says when
// the result goes, so that has to be decided first (ADR-0007).
//
// It returns immediately. The timer and the handover run outside, because this
// is called while the scorer holds its lock and a blocking observer would stall
// the next point.
func (r *Reporter) Observe(m *match.Match, t scorer.Transition) {
	switch {
	case t.Kind == scorer.TransitionMatchWon:
		r.won(m)
	case t.Kind == scorer.TransitionUndo && !t.State.Complete:
		r.reopened(m.ID)
	}
}

// won starts the grace period, or the handover itself when there is none.
func (r *Reporter) won(m *match.Match) {
	// A match played without choosing players is scored and shown like any
	// other and reported nowhere (ADR-0004). Not a failure, and not logged as
	// one: it is the ordinary case at a table with nothing else set up.
	if !m.Handover.Wanted() {
		return
	}

	if m.Report().Done {
		// Undone after the grace period and won again. The result over there
		// is the first one, and there is no route to change it (ADR-0007);
		// the page says so.
		r.log.Warn("the match was won again after its result was sent, schmetterpause keeps the first result",
			"match_id", m.ID)
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.delay == 0 || r.closed {
		r.inflight.Add(1)
		go func() {
			defer r.inflight.Done()
			defer r.done()
			// The error is already recorded on the match and logged by send,
			// and there is nobody here to return it to: this runs after the
			// last point, with no request waiting on it. The page reads the
			// outcome off the match and offers the retry.
			_ = r.send(context.Background(), m)
		}()
		return
	}

	// Won a second time, after an undo the first timer did not see stopped:
	// the clock starts again from this win.
	r.withdrawLocked(m.ID)

	due := r.now().Add(r.delay)
	w := &waiting{match: m}
	r.inflight.Add(1)
	w.timer = time.AfterFunc(r.delay, func() { r.fire(w) })
	r.waiting[m.ID] = w
	m.ReportDue(due)
	r.log.Info("report scheduled", "match_id", m.ID, "due", due, "grace_period", r.delay)
}

// reopened stops the grace period of a match an undo has opened again. What
// is sent is what holds once the match is won again.
func (r *Reporter) reopened(matchID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	w := r.withdrawLocked(matchID)
	if w == nil {
		return
	}
	w.match.ReportDue(time.Time{})
	r.log.Info("report withheld, the match was reopened", "match_id", matchID)
}

// withdrawLocked takes a match out of its grace period and returns it, or nil
// when it was not waiting. Must be called with r.mu held.
func (r *Reporter) withdrawLocked(matchID string) *waiting {
	w, ok := r.waiting[matchID]
	if !ok {
		return nil
	}
	delete(r.waiting, matchID)
	// A timer that has already fired finds itself gone from waiting and
	// counts itself out of inflight. One that has not is counted out here.
	if w.timer.Stop() {
		r.inflight.Done()
	}
	return w
}

// fire is the end of a grace period.
func (r *Reporter) fire(w *waiting) {
	defer r.inflight.Done()

	r.mu.Lock()
	mine := r.waiting[w.match.ID] == w
	if mine {
		delete(r.waiting, w.match.ID)
	}
	r.mu.Unlock()
	if !mine {
		return
	}

	defer r.done()
	// send looks at the score as it stands now, so what goes out is what
	// holds after the grace period.
	_ = r.send(context.Background(), w.match)
}

// Flush sends every result still in its grace period, at once, and waits for
// the handovers under way, bounded by ctx. It is the shutdown: a SIGTERM must
// not lose a result that was only waiting. From here on a won match is sent
// straight away.
func (r *Reporter) Flush(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	var flush []*match.Match
	for id := range r.waiting {
		w := r.waiting[id]
		delete(r.waiting, id)
		if !w.timer.Stop() {
			// Fired already, and it will find itself gone, send nothing and
			// count itself out. So it is sent from here, and counted again.
			r.inflight.Add(1)
		}
		flush = append(flush, w.match)
	}
	r.mu.Unlock()

	for _, m := range flush {
		r.log.Info("shutting down, sending a report that was waiting", "match_id", m.ID)
		_ = r.send(ctx, m)
		r.inflight.Done()
	}

	idle := make(chan struct{})
	go func() {
		r.inflight.Wait()
		close(idle)
	}()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for the reports under way: %w", ctx.Err())
	}
}

// Send performs one handover attempt now and records what became of it.
//
// Exported because the retry from the page is the same attempt, not a second
// implementation of it. A match in its grace period is taken out of it: who
// presses the button gives up the rest of it on purpose. Idempotent in the
// only way it can be here: a match already reported is left alone, so pressing
// retry after a late success does not enter the result twice.
func (r *Reporter) Send(ctx context.Context, m *match.Match) error {
	r.mu.Lock()
	if r.withdrawLocked(m.ID) != nil {
		r.log.Info("report sent before the grace period was over", "match_id", m.ID)
	}
	r.mu.Unlock()

	return r.send(ctx, m)
}

func (r *Reporter) send(ctx context.Context, m *match.Match) error {
	r.sending.Lock()
	defer r.sending.Unlock()

	if m.Report().Done {
		return nil
	}

	st := m.Scorer.State()
	if !st.Complete {
		// A match an undo has reopened, or a retry of one still running:
		// reporting it would send a score nobody has finished playing. Not
		// recorded as a failure, because nothing was sent; the match is
		// reported when it is won.
		m.ReportDue(time.Time{})
		r.log.Info("report withheld, the match was reopened", "match_id", m.ID)
		return errMatchNotComplete
	}

	bestOf, pointsPerSet := m.Scorer.Rules()
	playedAt := r.now()

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	result := Result{
		HomeID: m.Handover.HomeID,
		AwayID: m.Handover.AwayID,
		// Passed through rather than rebuilt: the field exists for this.
		Sets:        st.CompletedSets,
		BestOf:      bestOf,
		PointsToWin: pointsPerSet,
		PlayedAt:    &playedAt,
		// Unchecked here; Schmetterpause checks the place (ADR-0017).
		TournamentID: m.Handover.TournamentID,
		Round:        m.Handover.Round,
	}
	// One proof, never both: Schmetterpause refuses a result with an
	// operator and check-ins (ADR-0010).
	if m.Handover.CheckedIn() {
		result.HomeCheckinID = m.Handover.HomeCheckinID
		result.AwayCheckinID = m.Handover.AwayCheckinID
	} else {
		result.OperatorID = m.Handover.OperatorID
	}

	accepted, err := r.client.Report(ctx, result)
	if errors.Is(err, ErrAlreadyReported) && m.Handover.CheckedIn() {
		// A check-in already carries a result. Each one was made for this
		// match and nothing else here sends it, so the result that carries it
		// is this one: an earlier attempt arrived and its answer did not.
		// Recorded as delivered, without an id, because the 409 names none.
		m.Reported("", st.CompletedSets)
		r.log.Info("result already at schmetterpause, a retry of one that arrived",
			"match_id", m.ID, "error", err)
		return nil
	}
	if err != nil {
		m.ReportFailed(err)
		// Loudly, per ADR-0004. The message carries Schmetterpause's own
		// sentence — which player id is unknown, that the operator is playing —
		// because that is what somebody at the table can act on.
		// A tournament result can also find its place filled meanwhile, at
		// the kiosk or by another table: a 409 with Schmetterpause's
		// sentence, dropped like any other refusal (ADR-0017).
		r.log.Error("the result did not reach schmetterpause",
			"match_id", m.ID, "attempts", m.Report().Attempts, "error", err,
			"tournament_id", m.Handover.TournamentID, "round", m.Handover.Round)
		return err
	}

	m.Reported(accepted.MatchID, st.CompletedSets)
	r.log.Info("result reported to schmetterpause",
		"match_id", m.ID, "schmetterpause_match_id", accepted.MatchID,
		"tournament_id", m.Handover.TournamentID, "round", m.Handover.Round,
		// Pending is the expected answer during the test phase, and saying it
		// out loud is what stops somebody reading a green log as "it counts".
		"status", accepted.Status)
	return nil
}

// errMatchNotComplete is its own value so a caller can tell it from a network
// failure, which is worth retrying and this is not.
var errMatchNotComplete = errNotComplete{}

type errNotComplete struct{}

func (errNotComplete) Error() string {
	return "schmetterpause: the match is not finished, so there is no result to report"
}
