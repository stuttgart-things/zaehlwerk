package scorer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAPointGoesToThePlayerItWasFor(t *testing.T) {
	h := newHarness(t, Config{BestOf: 5})

	h.point(PlayerA)
	require.Equal(t, TransitionPoint, h.last().Kind)
	require.Equal(t, PlayerA, h.last().Side)

	h.point(PlayerB)
	require.Equal(t, PlayerB, h.last().Side)

	// A delta above one is still a point for that player.
	h.send(ScoreEvent{Player: PlayerB, Delta: 2})
	require.Equal(t, [2]int{1, 3}, h.state().Points)
	require.Equal(t, PlayerB, h.last().Side)
}

// Taking a point off one player does not give it to the other. The score of
// the opponent never moved, so naming them would flash their colour for a
// point they did not win.
func TestACorrectionThatLowersAScoreGoesToNobody(t *testing.T) {
	h := newHarness(t, Config{BestOf: 5})

	h.play("ababa")
	h.send(ScoreEvent{Player: PlayerB, Delta: -1})

	require.Equal(t, [2]int{3, 1}, h.state().Points)
	require.Equal(t, TransitionPoint, h.last().Kind)
	require.Empty(t, h.last().Side)
}

func TestAWonSetGoesToTheSideThatTookIt(t *testing.T) {
	for _, p := range []Player{PlayerA, PlayerB} {
		t.Run(string(p), func(t *testing.T) {
			h := newHarness(t, Config{BestOf: 5})

			h.play(repeat(string(p), 11))

			require.Equal(t, TransitionSetWon, h.last().Kind)
			require.Equal(t, p, h.last().Side)
			// The points are already reset, which is why Side has to be
			// carried rather than read off the state.
			require.Equal(t, [2]int{0, 0}, h.last().State.Points)
		})
	}
}

// The event is for one player and the set goes to the other: who took it is
// decided by the score, and so is the side.
func TestASetWonByACorrectionGoesToTheSideThatTookIt(t *testing.T) {
	tests := []struct {
		name     string
		seq      string
		lowered  Player
		wantSide Player
	}{
		{name: "a point off a completes it for b", seq: repeat("ab", 10) + "b", lowered: PlayerA, wantSide: PlayerB},
		{name: "a point off b completes it for a", seq: repeat("ab", 10) + "a", lowered: PlayerB, wantSide: PlayerA},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, Config{BestOf: 5})
			h.play(tt.seq)

			h.send(ScoreEvent{Player: tt.lowered, Delta: -1})

			require.Equal(t, TransitionSetWon, h.last().Kind)
			require.Equal(t, tt.wantSide, h.last().Side)
		})
	}
}

func TestAWonMatchGoesToTheWinner(t *testing.T) {
	h := newHarness(t, Config{BestOf: 3})

	h.play(repeat("b", 11) + repeat("b", 11))

	require.Equal(t, TransitionMatchWon, h.last().Kind)
	require.Equal(t, PlayerB, h.last().Side)
	require.Equal(t, h.last().State.Winner, h.last().Side)
}

func TestAMatchWonByACorrectionGoesToTheWinner(t *testing.T) {
	h := newHarness(t, Config{BestOf: 1})
	h.play(repeat("ab", 10) + "b")

	h.send(ScoreEvent{Player: PlayerA, Delta: -1})

	require.Equal(t, TransitionMatchWon, h.last().Kind)
	require.Equal(t, PlayerB, h.last().Side)
}

// An undo takes a point back from somebody; it does not go to anybody. That
// holds for an undone set too, where the set it reopens was won by a side.
func TestAnUndoGoesToNobody(t *testing.T) {
	h := newHarness(t, Config{BestOf: 5})

	h.point(PlayerA)
	h.undo()
	require.Equal(t, TransitionUndo, h.last().Kind)
	require.Empty(t, h.last().Side)

	h.play(repeat("b", 11))
	require.Equal(t, PlayerB, h.last().Side)
	h.undo()
	require.Equal(t, TransitionUndo, h.last().Kind)
	require.Empty(t, h.last().Side)
}
