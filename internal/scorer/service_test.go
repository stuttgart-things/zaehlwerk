package scorer

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestServiceRotationFollowsTheRulebook pins the serve order of a single set
// against a table written out from the rules rather than derived from the
// implementation. ITTF law 2.13.3: service alternates after every two points
// until both players reach ten, and after every point from then on.
func TestServiceRotationFollowsTheRulebook(t *testing.T) {
	want := []Player{
		// Two serves each, from 0:0 to 10:10.
		PlayerA, PlayerA, PlayerB, PlayerB,
		PlayerA, PlayerA, PlayerB, PlayerB,
		PlayerA, PlayerA, PlayerB, PlayerB,
		PlayerA, PlayerA, PlayerB, PlayerB,
		PlayerA, PlayerA, PlayerB, PlayerB,
		// 10:10. Single service from here on, starting with the player who
		// opened the set — ten changes have been made, which is even.
		PlayerA, PlayerB, PlayerA, PlayerB, PlayerA, PlayerB,
	}

	h := newHarness(t, Config{FirstServer: PlayerA})

	// Alternating winners keep the set alive: 10:10, then 11:10, 11:11 and so
	// on, so the deuce serves in the table are actually reached.
	for i, server := range want {
		st := h.state()
		require.Equalf(t, server, st.Serving,
			"point %d at %d:%d", i+1, st.Points[0], st.Points[1])

		if i%2 == 0 {
			h.point(PlayerA)
		} else {
			h.point(PlayerB)
		}
	}

	require.Equal(t, [2]int{13, 13}, h.state().Points)
	require.Equal(t, 1, h.state().SetNumber, "set must still be running at 13:13")
}

// TestServiceAtDeuceReturnsToTheOpenerOfTheSet is the same invariant stated
// directly, for both first servers and in a later set, because it is the point
// at which the two-point rule hands over to the one-point rule.
func TestServiceAtDeuceReturnsToTheOpenerOfTheSet(t *testing.T) {
	for _, first := range []Player{PlayerA, PlayerB} {
		t.Run(string(first), func(t *testing.T) {
			h := newHarness(t, Config{FirstServer: first})

			h.play(repeat("ab", 10))
			require.Equal(t, [2]int{10, 10}, h.state().Points)
			require.Equal(t, first, h.state().Serving)

			// And it changes with every point from here.
			h.point(PlayerA)
			require.Equal(t, first.Opponent(), h.state().Serving)
			h.point(PlayerB)
			require.Equal(t, first, h.state().Serving)
		})
	}
}

// TestFirstServerAlternatesBetweenSets covers ITTF law 2.13.6: the player who
// served first in a game receives first in the next one.
func TestFirstServerAlternatesBetweenSets(t *testing.T) {
	h := newHarness(t, Config{FirstServer: PlayerA, BestOf: 5})

	require.Equal(t, PlayerA, h.state().Serving, "set 1 opens with a")

	h.play(repeat("a", 11))
	require.Equal(t, 2, h.state().SetNumber)
	require.Equal(t, PlayerB, h.state().Serving, "set 2 opens with b")

	h.play(repeat("a", 11))
	require.Equal(t, 3, h.state().SetNumber)
	require.Equal(t, PlayerA, h.state().Serving, "set 3 opens with a again")
}

// serveWalker is an independent model of service rotation. It walks a match
// point by point and counts down to the next change, where the scorer derives
// the server from the score in closed form. Agreement between two differently
// shaped implementations is worth more than a test that repeats the formula.
//
// Both are written against ITTF laws 2.13.3 and 2.13.6 — that is where a
// reviewer should check them, not against each other.
type serveWalker struct {
	pointsPerSet int
	setFirst     Player
	serving      Player
	since        int
}

func newServeWalker(first Player, pointsPerSet int) *serveWalker {
	return &serveWalker{pointsPerSet: pointsPerSet, setFirst: first, serving: first}
}

// point records a point played, at the score a and b reached after it.
func (w *serveWalker) point(a, b int) {
	w.since++

	every := 2
	if deuce := w.pointsPerSet - 1; a >= deuce && b >= deuce {
		every = 1 // ITTF law 2.13.3
	}
	if w.since >= every {
		w.serving = w.serving.Opponent()
		w.since = 0
	}
}

// setEnded starts the next set. Whoever served first in the set just finished
// receives first in the next one.
func (w *serveWalker) setEnded() {
	w.setFirst = w.setFirst.Opponent()
	w.serving = w.setFirst
	w.since = 0
}

func TestServiceRotationMatchesAnIndependentWalk(t *testing.T) {
	const (
		trials    = 300
		pointsCap = 2000
	)
	rng := rand.New(rand.NewPCG(20260905, 1))

	var deuces, longDeuces, decidingSets int

	for trial := range trials {
		first := PlayerA
		if trial%2 == 1 {
			first = PlayerB
		}

		h := newHarness(t, Config{FirstServer: first, BestOf: 5})
		w := newServeWalker(first, DefaultPointsPerSet)

		for played := 0; ; played++ {
			require.Less(t, played, pointsCap, "match did not finish")

			before := h.state()
			if before.Complete {
				break
			}
			require.Equalf(t, w.serving, before.Serving,
				"trial %d: set %d at %d:%d",
				trial, before.SetNumber, before.Points[0], before.Points[1])

			if before.Points[0] >= 10 && before.Points[1] >= 10 {
				deuces++
				if before.Points[0] >= 12 && before.Points[1] >= 12 {
					longDeuces++
				}
			}

			p := PlayerA
			if rng.IntN(2) == 1 {
				p = PlayerB
			}
			h.point(p)

			switch after := h.state(); {
			case after.Complete:
			case after.SetNumber != before.SetNumber:
				w.setEnded()
			default:
				w.point(after.Points[0], after.Points[1])
			}
		}

		if st := h.state(); st.Sets[0]+st.Sets[1] == 5 {
			decidingSets++
		}
	}

	// A rotation test that never reaches deuce or a fifth set proves very
	// little, so the coverage the trials happened to produce is asserted too.
	require.Positive(t, deuces, "no set reached 10:10")
	require.Positive(t, longDeuces, "no set went past 12:12")
	require.Positive(t, decidingSets, "no match went the distance")
	t.Logf("deuce points %d, past 12:12 %d, five-set matches %d", deuces, longDeuces, decidingSets)
}

// TestServiceIsDerivedNotStored walks a set forwards, recording the server at
// every score, and then takes every point back, requiring the same server at
// each score on the way down. A server that is stored and toggled survives the
// way up and fails here.
func TestServiceIsDerivedNotStored(t *testing.T) {
	h := newHarness(t, Config{FirstServer: PlayerB})

	// Alternating winners, so the set runs to 13:13 without ever completing
	// and the walk covers both sides of 10:10.
	const points = 26

	var up []string
	for i := range points {
		st := h.state()
		up = append(up, fmt.Sprintf("%d:%d %s", st.Points[0], st.Points[1], st.Serving))

		if i%2 == 0 {
			h.point(PlayerA)
		} else {
			h.point(PlayerB)
		}
	}
	require.Equal(t, [2]int{13, 13}, h.state().Points)

	for i := points - 1; i >= 0; i-- {
		h.undo()

		st := h.state()
		require.Equal(t, up[i], fmt.Sprintf("%d:%d %s", st.Points[0], st.Points[1], st.Serving))
	}

	require.Equal(t, [2]int{0, 0}, h.state().Points)
}
