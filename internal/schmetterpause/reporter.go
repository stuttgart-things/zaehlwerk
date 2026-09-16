package schmetterpause

import (
	"context"
	"log/slog"
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
type Reporter struct {
	client   *Client
	registry *match.Registry
	log      *slog.Logger
	now      func() time.Time
	// timeout bounds one handover. The observer does not wait for it: the post
	// runs in its own goroutine, because observers are called from inside the
	// scorer's lock and must not block (see scorer.Observe).
	timeout time.Duration
	// done is closed-over only by tests, which need to wait for the goroutine
	// the observer starts.
	done func()
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

// withDone is called after each handover attempt. Tests only.
func withDone(fn func()) ReporterOption {
	return func(r *Reporter) { r.done = fn }
}

// NewReporter builds a Reporter around a Client.
func NewReporter(client *Client, registry *match.Registry, opts ...ReporterOption) *Reporter {
	r := &Reporter{
		client:   client,
		registry: registry,
		log:      slog.Default(),
		now:      time.Now,
		timeout:  DefaultTimeout,
		done:     func() {},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Observe is the scorer observer. Register it with (*match.Registry).Observe.
//
// It returns immediately. Everything after the filter happens in a goroutine,
// because this is called while the scorer holds its lock and a blocking
// observer would stall the next point.
func (r *Reporter) Observe(t scorer.Transition) {
	if t.Kind != scorer.TransitionMatchWon {
		return
	}

	m, err := r.registry.Get(t.State.MatchID)
	if err != nil {
		// The match won a moment ago, so this should not happen. It is logged
		// rather than ignored because the only way it can is a bug.
		r.log.Error("a won match is not in the registry",
			"match_id", t.State.MatchID, "error", err)
		return
	}

	// A match played without choosing players is scored and shown like any
	// other and reported nowhere (ADR-0004). Not a failure, and not logged as
	// one: it is the ordinary case at a table with nothing else set up.
	if !m.Handover.Wanted() {
		return
	}

	go func() {
		defer r.done()
		r.Send(context.Background(), m)
	}()
}

// Send performs one handover attempt and records what became of it.
//
// Exported because the retry from the page is the same attempt, not a second
// implementation of it. Idempotent in the only way it can be here: a match
// already reported is left alone, so pressing retry after a late success does
// not enter the result twice.
func (r *Reporter) Send(ctx context.Context, m *match.Match) error {
	if m.Report().Done {
		return nil
	}

	st := m.Scorer.State()
	if !st.Complete {
		// Retrying a match that is still running would report a score nobody
		// has finished playing.
		err := errMatchNotComplete
		m.ReportFailed(err)
		return err
	}

	bestOf, pointsPerSet := m.Scorer.Rules()
	playedAt := r.now()

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	accepted, err := r.client.Report(ctx, Result{
		HomeID:     m.Handover.HomeID,
		AwayID:     m.Handover.AwayID,
		OperatorID: m.Handover.OperatorID,
		// Passed through rather than rebuilt: the field exists for this.
		Sets:        st.CompletedSets,
		BestOf:      bestOf,
		PointsToWin: pointsPerSet,
		PlayedAt:    &playedAt,
	})
	if err != nil {
		m.ReportFailed(err)
		// Loudly, per ADR-0004. The message carries Schmetterpause's own
		// sentence — which player id is unknown, that the operator is playing —
		// because that is what somebody at the table can act on.
		r.log.Error("the result did not reach schmetterpause",
			"match_id", m.ID, "attempts", m.Report().Attempts, "error", err)
		return err
	}

	m.Reported(accepted.MatchID)
	r.log.Info("result reported to schmetterpause",
		"match_id", m.ID, "schmetterpause_match_id", accepted.MatchID,
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
