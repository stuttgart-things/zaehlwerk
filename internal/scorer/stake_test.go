package scorer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTheNextPointDecidesASetOrTheMatchOnlyWithALeadAtItsEdge(t *testing.T) {
	tests := []struct {
		name      string
		bestOf    int
		seq       string
		wantP     Player
		wantMatch bool
	}{
		{name: "nothing at the start", bestOf: 5, seq: ""},
		{name: "nothing at nine all", bestOf: 5, seq: repeat("ab", 9)},
		{name: "a set point at ten nine", bestOf: 5, seq: repeat("ab", 9) + "a", wantP: PlayerA},
		{name: "a set point for b at three ten", bestOf: 5, seq: "aaa" + repeat("b", 10), wantP: PlayerB},
		{name: "nothing at ten all", bestOf: 5, seq: repeat("ab", 10)},
		{name: "a set point again at the advantage", bestOf: 5, seq: repeat("ab", 10) + "b", wantP: PlayerB},
		{name: "a match point in one set to eleven", bestOf: 1, seq: repeat("a", 10), wantP: PlayerA, wantMatch: true},
		{name: "a set point, not a match point, one set short", bestOf: 3, seq: repeat("a", 10), wantP: PlayerA},
		{name: "a match point one set short of the match", bestOf: 3, seq: repeat("a", 11) + repeat("a", 10), wantP: PlayerA, wantMatch: true},
		{name: "a match point for whoever trails in sets", bestOf: 3, seq: repeat("a", 11) + repeat("b", 11) + repeat("b", 10), wantP: PlayerB, wantMatch: true},
		{name: "nothing once the match is over", bestOf: 1, seq: repeat("a", 11)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, Config{BestOf: tt.bestOf})
			h.play(tt.seq)

			p, match := h.s.Stake(h.state())
			require.Equal(t, tt.wantP, p)
			require.Equal(t, tt.wantMatch, match)
		})
	}
}

func TestNothingIsAtStakeWhileTheMatchWaitsForTheWarmUp(t *testing.T) {
	h := newHarness(t, Config{BestOf: 1, Start: StartWarmup})

	p, _ := h.s.Stake(h.state())
	require.Empty(t, p)
}

func TestTheStakeIsTheStateAskedAboutRatherThanTheCurrentOne(t *testing.T) {
	h := newHarness(t, Config{BestOf: 1})
	h.play(repeat("a", 10))
	at := h.state()
	h.play("a")

	p, match := h.s.Stake(at)
	require.Equal(t, PlayerA, p)
	require.True(t, match)
}
