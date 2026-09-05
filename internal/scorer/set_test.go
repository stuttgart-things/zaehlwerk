package scorer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSetCompletion(t *testing.T) {
	tests := []struct {
		name          string
		seq           string
		wantPoints    [2]int
		wantSets      [2]int
		wantSetNumber int
		wantCompleted [][2]int
	}{
		{
			name:          "eleven with a clear lead takes the set",
			seq:           repeat("ab", 9) + "aa",
			wantPoints:    [2]int{0, 0},
			wantSets:      [2]int{1, 0},
			wantSetNumber: 2,
			wantCompleted: [][2]int{{11, 9}},
		},
		{
			name:          "eleven to nothing takes the set",
			seq:           repeat("a", 11),
			wantPoints:    [2]int{0, 0},
			wantSets:      [2]int{1, 0},
			wantSetNumber: 2,
			wantCompleted: [][2]int{{11, 0}},
		},
		{
			name:          "ten all is not a set",
			seq:           repeat("ab", 10),
			wantPoints:    [2]int{10, 10},
			wantSets:      [2]int{0, 0},
			wantSetNumber: 1,
		},
		{
			name:          "eleven ten is not a set",
			seq:           repeat("ab", 10) + "a",
			wantPoints:    [2]int{11, 10},
			wantSets:      [2]int{0, 0},
			wantSetNumber: 1,
		},
		{
			name:          "twelve ten takes the set",
			seq:           repeat("ab", 10) + "aa",
			wantPoints:    [2]int{0, 0},
			wantSets:      [2]int{1, 0},
			wantSetNumber: 2,
			wantCompleted: [][2]int{{12, 10}},
		},
		{
			name:          "a lead of one never ends it",
			seq:           repeat("ab", 10) + repeat("ab", 8) + "a",
			wantPoints:    [2]int{19, 18},
			wantSets:      [2]int{0, 0},
			wantSetNumber: 1,
		},
		{
			name:          "the deuce runs until someone leads by two",
			seq:           repeat("ab", 10) + repeat("ab", 4) + "bb",
			wantPoints:    [2]int{0, 0},
			wantSets:      [2]int{0, 1},
			wantSetNumber: 2,
			wantCompleted: [][2]int{{14, 16}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, Config{BestOf: 5})
			h.play(tt.seq)

			st := h.state()
			require.Equal(t, tt.wantPoints, st.Points)
			require.Equal(t, tt.wantSets, st.Sets)
			require.Equal(t, tt.wantSetNumber, st.SetNumber)
			require.Equal(t, tt.wantCompleted, st.CompletedSets)
			require.False(t, st.Complete)
		})
	}
}

func TestFullMatches(t *testing.T) {
	set := func(winner Player, loserPoints int) string {
		seq := ""
		for range loserPoints {
			seq += string(winner.Opponent())
		}
		for range 11 {
			seq += string(winner)
		}
		return seq
	}

	tests := []struct {
		name          string
		bestOf        int
		seq           string
		wantWinner    Player
		wantSets      [2]int
		wantPoints    [2]int
		wantSetNumber int
		wantCompleted [][2]int
	}{
		{
			name:          "straight sets",
			bestOf:        5,
			seq:           set(PlayerA, 4) + set(PlayerA, 9) + set(PlayerA, 0),
			wantWinner:    PlayerA,
			wantSets:      [2]int{3, 0},
			wantPoints:    [2]int{11, 0},
			wantSetNumber: 3,
			wantCompleted: [][2]int{{11, 4}, {11, 9}, {11, 0}},
		},
		{
			name:          "the full distance",
			bestOf:        5,
			seq:           set(PlayerA, 5) + set(PlayerB, 3) + set(PlayerA, 8) + set(PlayerB, 2) + set(PlayerB, 7),
			wantWinner:    PlayerB,
			wantSets:      [2]int{2, 3},
			wantPoints:    [2]int{7, 11},
			wantSetNumber: 5,
			wantCompleted: [][2]int{{11, 5}, {3, 11}, {11, 8}, {2, 11}, {7, 11}},
		},
		{
			name:          "best of three",
			bestOf:        3,
			seq:           set(PlayerB, 6) + set(PlayerB, 6),
			wantWinner:    PlayerB,
			wantSets:      [2]int{0, 2},
			wantPoints:    [2]int{6, 11},
			wantSetNumber: 2,
			wantCompleted: [][2]int{{6, 11}, {6, 11}},
		},
		{
			name:          "a single set decides a best of one",
			bestOf:        1,
			seq:           set(PlayerA, 9),
			wantWinner:    PlayerA,
			wantSets:      [2]int{1, 0},
			wantPoints:    [2]int{11, 9},
			wantSetNumber: 1,
			wantCompleted: [][2]int{{11, 9}},
		},
		{
			name:          "decided in a deuce",
			bestOf:        3,
			seq:           set(PlayerA, 2) + set(PlayerB, 4) + repeat("ab", 10) + "bb",
			wantWinner:    PlayerB,
			wantSets:      [2]int{1, 2},
			wantPoints:    [2]int{10, 12},
			wantSetNumber: 3,
			wantCompleted: [][2]int{{11, 2}, {4, 11}, {10, 12}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, Config{BestOf: tt.bestOf})
			h.play(tt.seq)

			st := h.state()
			require.True(t, st.Complete)
			require.Equal(t, tt.wantWinner, st.Winner)
			require.Equal(t, tt.wantSets, st.Sets)
			require.Equal(t, tt.wantSetNumber, st.SetNumber)
			// The final set score stays on the board rather than resetting to
			// 0:0 — it is what the panel and the result should show.
			require.Equal(t, tt.wantPoints, st.Points)
			require.Equal(t, tt.wantCompleted, st.CompletedSets)
		})
	}
}

// TestACorrectionCanCompleteASetForTheOtherPlayer covers the one way a set can
// be decided by an event that did not score a point: taking a point off the
// player who was one behind leaves the other one two clear. Deciding the set
// from the sender rather than from the score misses it.
func TestACorrectionCanCompleteASetForTheOtherPlayer(t *testing.T) {
	h := newHarness(t, Config{BestOf: 5})

	h.play(repeat("ab", 10) + "b")
	require.Equal(t, [2]int{10, 11}, h.state().Points, "a lead of one, still running")
	require.Equal(t, 1, h.state().SetNumber)

	// A point is taken off a — b is now two clear at eleven.
	res := h.send(ScoreEvent{Player: PlayerA, Delta: -1})
	require.Equal(t, Applied, res.Outcome)

	st := h.state()
	require.Equal(t, [2]int{0, 1}, st.Sets)
	require.Equal(t, 2, st.SetNumber)
	require.Equal(t, [][2]int{{9, 11}}, st.CompletedSets)
	require.Equal(t, TransitionSetWon, h.seen[len(h.seen)-1].Kind)

	// And it is undoable like any other point.
	require.Equal(t, [2]int{10, 11}, h.undo().Points)
	require.Equal(t, 1, h.state().SetNumber)
}

// TestACorrectionCanCompleteASetForTheSenderToo is the same thing from the
// other side: the sender is b, the set goes to a, and a's own score never
// moved. Only the gap did.
func TestACorrectionCanCompleteASetForTheSenderToo(t *testing.T) {
	h := newHarness(t, Config{BestOf: 5})

	h.play(repeat("ab", 10) + "a")
	require.Equal(t, [2]int{11, 10}, h.state().Points)

	res := h.send(ScoreEvent{Player: PlayerB, Delta: -1})
	require.Equal(t, Applied, res.Outcome)
	require.Equal(t, [2]int{1, 0}, res.State.Sets)
	require.Equal(t, [][2]int{{11, 9}}, res.State.CompletedSets)
	require.Equal(t, TransitionSetWon, h.seen[len(h.seen)-1].Kind)
}

func TestACorrectionThatLeavesTheSetRunning(t *testing.T) {
	h := newHarness(t, Config{BestOf: 5})

	h.play("ababa")
	require.Equal(t, [2]int{3, 2}, h.state().Points)

	res := h.send(ScoreEvent{Player: PlayerB, Delta: -1})
	require.Equal(t, Applied, res.Outcome)
	require.Equal(t, [2]int{3, 1}, res.State.Points)
	require.Equal(t, 1, res.State.SetNumber)
	require.Equal(t, TransitionPoint, h.seen[len(h.seen)-1].Kind)
}

func TestPointsAfterTheMatchAreRejected(t *testing.T) {
	h := newHarness(t, Config{BestOf: 1})
	h.play(repeat("a", 11))

	before := h.state()
	require.True(t, before.Complete)

	_, err := h.apply(ScoreEvent{Player: PlayerB, Delta: 1})
	require.ErrorIs(t, err, ErrMatchComplete)
	require.Equal(t, before, h.state())

	// Undo still works, because the last thing a scorekeeper does after a
	// wrongly awarded match point is press undo.
	h.undo()
	require.False(t, h.state().Complete)
	require.Equal(t, [2]int{10, 0}, h.state().Points)

	// And the match carries on from there.
	h.point(PlayerB)
	require.Equal(t, [2]int{10, 1}, h.state().Points)
}

func TestPointsPerSetMovesTheSetAndDeuceThresholds(t *testing.T) {
	h := newHarness(t, Config{BestOf: 1, PointsPerSet: 21})

	h.play(repeat("ab", 10) + "a")
	require.Equal(t, [2]int{11, 10}, h.state().Points, "eleven is not a set at 21")

	h.play(repeat("ba", 9) + "b")
	require.Equal(t, [2]int{20, 20}, h.state().Points)

	// Deuce follows the set length, so single service starts at 20:20.
	require.Equal(t, PlayerA, h.state().Serving)
	h.point(PlayerA)
	require.Equal(t, PlayerB, h.state().Serving)

	h.point(PlayerA)
	require.True(t, h.state().Complete)
	require.Equal(t, [2]int{22, 20}, h.state().Points)
}

func TestTransitionsReportWhatHappened(t *testing.T) {
	h := newHarness(t, Config{BestOf: 1})

	h.play(repeat("a", 10))
	require.Equal(t, []TransitionKind{
		TransitionPoint, TransitionPoint, TransitionPoint, TransitionPoint, TransitionPoint,
		TransitionPoint, TransitionPoint, TransitionPoint, TransitionPoint, TransitionPoint,
	}, h.kinds())

	h.point(PlayerA)
	require.Equal(t, TransitionMatchWon, h.seen[len(h.seen)-1].Kind)
	require.True(t, h.seen[len(h.seen)-1].State.Complete)

	h.undo()
	require.Equal(t, TransitionUndo, h.seen[len(h.seen)-1].Kind)
	require.False(t, h.seen[len(h.seen)-1].State.Complete)
	require.Equal(t, [2]int{10, 0}, h.seen[len(h.seen)-1].State.Points)
}

func TestSetWonAndMatchWonAreDistinctTransitions(t *testing.T) {
	h := newHarness(t, Config{BestOf: 3})

	h.play(repeat("a", 11))
	require.Equal(t, TransitionSetWon, h.seen[len(h.seen)-1].Kind)
	require.False(t, h.seen[len(h.seen)-1].State.Complete)

	h.play(repeat("a", 11))
	require.Equal(t, TransitionMatchWon, h.seen[len(h.seen)-1].Kind)
	require.True(t, h.seen[len(h.seen)-1].State.Complete)
}
