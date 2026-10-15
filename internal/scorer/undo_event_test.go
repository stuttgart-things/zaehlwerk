package scorer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// undoEvent sends an undo from source with id, filling in the match id.
func (h *harness) undoEvent(source string, id uint64) (Result, error) {
	h.t.Helper()
	return h.s.UndoEvent(ScoreEvent{MatchID: h.s.cfg.MatchID, Source: source, EventID: id})
}

func TestAnUndoEventSentTwiceTakesBackOnePoint(t *testing.T) {
	h := newHarness(t, Config{})
	h.play("bb")

	res, err := h.undoEvent("button-b", 7)
	require.NoError(t, err)
	require.Equal(t, Applied, res.Outcome)
	require.Equal(t, [2]int{0, 1}, res.State.Points)

	res, err = h.undoEvent("button-b", 7)
	require.NoError(t, err)
	require.Equal(t, Duplicate, res.Outcome)
	require.Equal(t, [2]int{0, 1}, h.state().Points)
}

func TestAnUndoEventAfterASetPointReopensTheSet(t *testing.T) {
	h := newHarness(t, Config{})
	h.play("aaaaaaaaaa")
	before := h.state()
	h.point(PlayerA)
	require.Equal(t, 2, h.state().SetNumber)

	res, err := h.undoEvent("button-a", 1)
	require.NoError(t, err)
	require.Equal(t, Applied, res.Outcome)
	require.Equal(t, before, res.State)
}

func TestAnUndoEventTakesTheMatchPointBack(t *testing.T) {
	h := newHarness(t, Config{BestOf: 1})
	h.play("aaaaaaaaaa")
	h.point(PlayerA)
	require.True(t, h.state().Complete)

	res, err := h.undoEvent("button-a", 1)
	require.NoError(t, err)
	require.Equal(t, Applied, res.Outcome)
	require.False(t, res.State.Complete)
	require.Equal(t, TransitionUndo, h.seen[len(h.seen)-1].Kind)
}

// A retry of an undo that found nothing must stay a duplicate, or it would
// take back the next point scored before it arrived.
func TestAnUndoEventWithNothingToTakeBackIsRecordedAndChangesNothing(t *testing.T) {
	h := newHarness(t, Config{})

	res, err := h.undoEvent("button-a", 1)
	require.NoError(t, err)
	require.Equal(t, Ignored, res.Outcome)
	require.Empty(t, h.seen)

	h.point(PlayerB)
	res, err = h.undoEvent("button-a", 1)
	require.NoError(t, err)
	require.Equal(t, Duplicate, res.Outcome)
	require.Equal(t, [2]int{0, 1}, h.state().Points)
}

// One counter per source: an undo uses up its id, and a point the same source
// sends with that id afterwards is a duplicate.
func TestAnUndoEventSharesItsSourcesCounterWithItsPoints(t *testing.T) {
	h := newHarness(t, Config{})
	res := h.send(ScoreEvent{Source: "button-a", Player: PlayerA, Delta: 1, EventID: 1})
	require.Equal(t, Applied, res.Outcome)

	_, err := h.undoEvent("button-a", 1)
	require.NoError(t, err)
	require.Equal(t, [2]int{1, 0}, h.state().Points, "an undo with a point's id is that point resent")

	res2, err := h.undoEvent("button-a", 2)
	require.NoError(t, err)
	require.Equal(t, Applied, res2.Outcome)

	res = h.send(ScoreEvent{Source: "button-a", Player: PlayerA, Delta: 1, EventID: 2})
	require.Equal(t, Duplicate, res.Outcome)
	require.Equal(t, [2]int{0, 0}, h.state().Points)
}

func TestAnUndoEventNeedsASourceAndItsMatch(t *testing.T) {
	h := newHarness(t, Config{})

	_, err := h.undoEvent("", 1)
	require.ErrorIs(t, err, ErrMissingSource)

	_, err = h.s.UndoEvent(ScoreEvent{MatchID: "other", Source: "button-a", EventID: 1})
	require.ErrorIs(t, err, ErrWrongMatch)
}
