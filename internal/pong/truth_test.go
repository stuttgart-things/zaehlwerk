package pong

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

func TestAwardedCountsTheFinishedSetsAndTheOneInProgress(t *testing.T) {
	st := scorer.State{
		CompletedSets: [][2]int{{11, 7}, {9, 11}},
		Points:        [2]int{4, 6},
	}
	require.Equal(t, [2]int{24, 24}, Awarded(st))
}

func TestAwardedCountsTheFinalSetOnceWhenTheMatchIsWon(t *testing.T) {
	// The final set is both the last completed set and the points.
	st := scorer.State{
		CompletedSets: [][2]int{{11, 7}, {11, 9}},
		Points:        [2]int{11, 9},
		Complete:      true,
	}
	require.Equal(t, [2]int{22, 16}, Awarded(st))
}

func TestTheTruthAgreesWhenEveryRallyWasAwarded(t *testing.T) {
	var truth Truth
	for range 11 {
		truth.Rally(scorer.PlayerA)
	}
	for range 3 {
		truth.Rally(scorer.PlayerB)
	}
	truth.Rally(scorer.PlayerB)

	// 11:3 was a set, and the next one has started 0:1.
	st := scorer.State{CompletedSets: [][2]int{{11, 3}}, Points: [2]int{0, 1}}
	_, off := truth.Points(15, st)
	require.False(t, off)
}

func TestALostPointIsADivergenceAtTheRallyItWasLost(t *testing.T) {
	var truth Truth
	truth.Rally(scorer.PlayerA)
	truth.Rally(scorer.PlayerB)

	d, off := truth.Points(7, scorer.State{Points: [2]int{1, 0}})
	require.True(t, off)
	require.Equal(t, Divergence{EventID: 7, What: "points", Truth: "1:1", Zaehlwerk: "1:0"}, d)
}

func TestAServeRallyWonByOneSideMakesThatSideServe(t *testing.T) {
	truth := Truth{ServeRally: scorer.PlayerB}

	_, off := truth.Serve(3, scorer.State{Serving: scorer.PlayerB})
	require.False(t, off)

	d, off := truth.Serve(3, scorer.State{Serving: scorer.PlayerA})
	require.True(t, off)
	require.Equal(t, "serve", d.What)
}

func TestEndsThatMoveWithoutAChangeOfEndsAreADivergence(t *testing.T) {
	standing := [2]scorer.Player{scorer.PlayerA, scorer.PlayerB}

	_, off := Ends(9, standing, scorer.State{Ends: standing})
	require.False(t, off)

	d, off := Ends(9, standing, scorer.State{Ends: [2]scorer.Player{scorer.PlayerB, scorer.PlayerA}})
	require.True(t, off)
	require.Equal(t, Divergence{EventID: 9, What: "ends", Truth: "a:b", Zaehlwerk: "b:a"}, d)
}

func TestAPlayerIsReportedForTheHalfTheyStandAt(t *testing.T) {
	swapped := [2]scorer.Player{scorer.PlayerB, scorer.PlayerA}
	require.Equal(t, scorer.HalfRight, halfOf(scorer.PlayerA, swapped))
	require.Equal(t, scorer.HalfLeft, halfOf(scorer.PlayerB, swapped))
}
