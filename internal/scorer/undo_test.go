package scorer

import (
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUndoRestoresTheExactPreviousState is the property that matters most and
// the one a hand-written expectation is worst at: after taking a point back the
// state must be *identical*, not merely have the right score. Comparing whole
// states catches a field that undo forgets — the set number, the serve, the set
// history — without the test having to guess which one it would be.
func TestUndoRestoresTheExactPreviousState(t *testing.T) {
	const (
		trials    = 200
		pointsCap = 2000
	)
	rng := rand.New(rand.NewPCG(20260905, 2))

	var setPoints, matchPoints, deucePoints int

	for trial := range trials {
		h := newHarness(t, Config{BestOf: 5})

		for played := 0; ; played++ {
			require.Less(t, played, pointsCap, "match did not finish")

			before := h.state()
			if before.Complete {
				break
			}
			if before.Points[0] >= 10 && before.Points[1] >= 10 {
				deucePoints++
			}

			p := PlayerA
			if rng.IntN(2) == 1 {
				p = PlayerB
			}

			h.point(p)
			switch after := h.state(); {
			case after.Complete:
				matchPoints++
			case after.SetNumber != before.SetNumber:
				setPoints++
			}

			restored := h.undo()
			require.Equalf(t, before, restored, "trial %d, point %d", trial, played)
			require.Equalf(t, before, h.state(), "trial %d, point %d", trial, played)

			// Put it back and carry on, so the walk covers a whole match.
			h.point(p)
		}
	}

	// The property is only worth anything if the interesting points were among
	// the ones taken back.
	require.Positive(t, setPoints, "no set-winning point was undone")
	require.Positive(t, matchPoints, "no match-winning point was undone")
	require.Positive(t, deucePoints, "no point in a deuce was undone")
	t.Logf("undone: %d set points, %d match points, %d deuce points", setPoints, matchPoints, deucePoints)
}

// TestUndoOfASetPointGoesBackIntoTheSet spells out the same thing at the one
// boundary that is easy to get subtly wrong, with the expectations written by
// hand rather than compared against a snapshot.
func TestUndoOfASetPointGoesBackIntoTheSet(t *testing.T) {
	h := newHarness(t, Config{BestOf: 5, FirstServer: PlayerA})

	// 10:7 in the first set. Seventeen points played, eight service changes, so
	// a is serving — deliberately not the same player who will be serving on
	// the other side of the set boundary.
	h.play(repeat("ab", 7) + "aaa")
	atSetPoint := h.state()
	require.Equal(t, [2]int{10, 7}, atSetPoint.Points)
	require.Equal(t, PlayerA, atSetPoint.Serving)

	h.point(PlayerA)

	won := h.state()
	require.Equal(t, [2]int{0, 0}, won.Points)
	require.Equal(t, [2]int{1, 0}, won.Sets)
	require.Equal(t, 2, won.SetNumber)
	require.Equal(t, [][2]int{{11, 7}}, won.CompletedSets)
	require.Equal(t, PlayerB, won.Serving, "b opens the second set")

	// One press of undo, and the match is back in the first set — score, set
	// number, set history and the serve all together.
	back := h.undo()
	require.Equal(t, [2]int{10, 7}, back.Points)
	require.Equal(t, [2]int{0, 0}, back.Sets)
	require.Equal(t, 1, back.SetNumber)
	require.Empty(t, back.CompletedSets)
	require.Equal(t, PlayerA, back.Serving, "the serve comes back with the score")
	require.Equal(t, atSetPoint, back)

	// And undo keeps working backwards inside that set.
	require.Equal(t, [2]int{9, 7}, h.undo().Points)
	require.Equal(t, [2]int{8, 7}, h.undo().Points)
}

// TestUndoStopsAtTheStartOfTheSetInProgress is the other half of the boundary:
// once a point of the new set has been applied, the previous set is closed.
func TestUndoStopsAtTheStartOfTheSetInProgress(t *testing.T) {
	h := newHarness(t, Config{BestOf: 5})

	h.play(repeat("ab", 9) + "aa") // a takes the first set 11:9
	require.Equal(t, 2, h.state().SetNumber)

	h.point(PlayerB)
	require.Equal(t, [2]int{0, 1}, h.state().Points)

	h.undo()
	start := h.state()
	require.Equal(t, [2]int{0, 0}, start.Points)
	require.Equal(t, [2]int{1, 0}, start.Sets)
	require.Equal(t, 2, start.SetNumber)

	// The first set is history now.
	_, err := h.s.Undo()
	require.ErrorIs(t, err, ErrNothingToUndo)
	require.Equal(t, start, h.state())
}

func TestUndoOnAFreshMatch(t *testing.T) {
	h := newHarness(t, Config{})

	before := h.state()
	_, err := h.s.Undo()
	require.ErrorIs(t, err, ErrNothingToUndo)
	require.Equal(t, before, h.state())
	require.Empty(t, h.seen, "a refused undo is not a transition")
}

func TestUndoBackToTheStartOfTheMatch(t *testing.T) {
	h := newHarness(t, Config{})
	h.play("abab")

	for range 4 {
		h.undo()
	}
	require.Equal(t, [2]int{0, 0}, h.state().Points)

	_, err := h.s.Undo()
	require.ErrorIs(t, err, ErrNothingToUndo)
}

// TestUndoIgnoresEventsThatChangedNothing keeps a phantom piezo hit from
// swallowing a press of undo that was meant for the last real point.
func TestUndoIgnoresEventsThatChangedNothing(t *testing.T) {
	h := newHarness(t, Config{})

	h.point(PlayerA)
	require.Equal(t, Ignored, h.send(ScoreEvent{Player: PlayerA, Delta: 0, Source: "piezo-1"}).Outcome)
	require.Equal(t, [2]int{1, 0}, h.state().Points)

	h.undo()
	require.Equal(t, [2]int{0, 0}, h.state().Points, "undo takes back the point, not the phantom hit")
}
