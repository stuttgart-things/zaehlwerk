package face

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

const a, b = scorer.PlayerA, scorer.PlayerB

func playing(pa, pb int) scorer.State {
	return scorer.State{Points: [2]int{pa, pb}, Phase: scorer.PhasePlaying}
}

// One row per line of the table in ADR-0018, which is the table in
// Schmetterpause ADR-0029 made final.
func TestEveryMomentOfTheMatchHasItsFace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		moment Moment
		streak Streak
		want   Faces
	}{
		{
			name:   "the warm-up is neutral",
			moment: Moment{Kind: scorer.TransitionConfirmed, State: scorer.State{Phase: scorer.PhaseWarmup}},
			want:   Faces{Neutral, Neutral},
		},
		{
			name:   "a change of ends is neutral",
			moment: Moment{Kind: scorer.TransitionEndsChanged, State: playing(5, 3)},
			want:   Faces{Neutral, Neutral},
		},
		{
			name:   "the point's winner is happy and its loser skeptical",
			moment: Moment{Kind: scorer.TransitionPoint, Side: b, State: playing(3, 4)},
			want:   Faces{Skeptical, Happy},
		},
		{
			name:   "the side facing a set point is surprised",
			moment: Moment{Kind: scorer.TransitionPoint, Side: a, State: playing(10, 7), Stake: a},
			want:   Faces{Happy, Surprised},
		},
		{
			name:   "the side facing a match point is surprised",
			moment: Moment{Kind: scorer.TransitionPoint, Side: b, State: playing(4, 10), Stake: b},
			want:   Faces{Surprised, Happy},
		},
		{
			name:   "both sides are surprised at deuce",
			moment: Moment{Kind: scorer.TransitionPoint, Side: a, State: playing(10, 10), PointsPerSet: 11},
			want:   Faces{Surprised, Surprised},
		},
		{
			name:   "the third point lost in a row is angry",
			moment: Moment{Kind: scorer.TransitionPoint, Side: a, State: playing(6, 2)},
			streak: Streak{0, 2},
			want:   Faces{Happy, Angry},
		},
		{
			name:   "angry stays angry facing a set point",
			moment: Moment{Kind: scorer.TransitionPoint, Side: a, State: playing(10, 6), Stake: a},
			streak: Streak{0, 4},
			want:   Faces{Happy, Angry},
		},
		{
			name:   "the set's loser is sad",
			moment: Moment{Kind: scorer.TransitionSetWon, Side: a, State: playing(0, 0)},
			want:   Faces{Happy, Sad},
		},
		{
			name:   "the match's winner is happy and its loser skeptical, never sad",
			moment: Moment{Kind: scorer.TransitionMatchWon, Side: b, State: scorer.State{Complete: true, Winner: b, Phase: scorer.PhaseComplete}},
			want:   Faces{Skeptical, Happy},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := Read(tc.moment, tc.streak)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestAStreakCountsOnlyThePointsLostInARow(t *testing.T) {
	var s Streak
	for _, side := range []scorer.Player{b, b, a, b, b} {
		_, s = Read(Moment{Kind: scorer.TransitionPoint, Side: side, State: playing(1, 1)}, s)
	}
	require.Equal(t, Streak{2, 0}, s, "a's two losses since its own point; b's loss reset by its points")
}

func TestASetOrAnUndoStartsTheStreakAfresh(t *testing.T) {
	for _, kind := range []scorer.TransitionKind{scorer.TransitionSetWon, scorer.TransitionUndo} {
		_, s := Read(Moment{Kind: kind, Side: a, State: playing(0, 0)}, Streak{0, 5})
		require.Equal(t, Streak{}, s, string(kind))
	}
}

// A snapshot is where a page joins: the faces a page opened mid-match shows
// are what the score says, and the streak it was handed survives it.
func TestASnapshotShowsWhatTheScoreSays(t *testing.T) {
	got, s := Read(Moment{Kind: "snapshot", State: playing(10, 9), Stake: a}, Streak{1, 0})
	require.Equal(t, Faces{Neutral, Surprised}, got)
	require.Equal(t, Streak{1, 0}, s)

	got, _ = Read(Moment{Kind: "snapshot", State: scorer.State{Complete: true, Winner: a, Phase: scorer.PhaseComplete}}, Streak{})
	require.Equal(t, Faces{Happy, Skeptical}, got, "a page opened on a decided match")
}

// A correction that lowered a score went to nobody (scorer.Transition.Side):
// there is no winner of it to be happy.
func TestACorrectionIsReadLikeAnUndo(t *testing.T) {
	got, s := Read(Moment{Kind: scorer.TransitionPoint, State: playing(4, 4)}, Streak{3, 0})
	require.Equal(t, Faces{Neutral, Neutral}, got)
	require.Equal(t, Streak{}, s)
}

// Deuce is tense only while the set is being played: a level score after a
// change of ends in the deciding set is not one.
func TestDeuceNeedsTheEdgeOfTheSet(t *testing.T) {
	got, _ := Read(Moment{Kind: scorer.TransitionPoint, Side: a, State: playing(9, 9), PointsPerSet: 11}, Streak{})
	require.Equal(t, Faces{Happy, Skeptical}, got, "9:9 is not deuce yet")
}
