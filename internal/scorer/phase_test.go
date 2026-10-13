package scorer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// press is a button at a half: the source a real button would be, one per
// half, so presses from the two halves never deduplicate against each other.
func (h *harness) press(half Half) Result {
	h.t.Helper()
	return h.send(ScoreEvent{Half: half, Delta: 1, Source: "button-" + string(half)})
}

func TestAMatchWithoutPhasesStartsPlaying(t *testing.T) {
	h := newHarness(t, Config{})

	st := h.state()
	require.Equal(t, PhasePlaying, st.Phase)
	require.Equal(t, PlayerA, st.Serving)
	require.Empty(t, st.Waiting)
}

func TestTheServeRallyDecidesWhoServesAndScoresNothing(t *testing.T) {
	h := newHarness(t, Config{Start: StartServeRally})

	require.Equal(t, PhaseServeRally, h.state().Phase)
	require.Empty(t, h.state().Serving, "nobody serves before the rally is decided")

	h.press(HalfRight)

	require.Equal(t, TransitionServeDecided, h.last().Kind)
	require.Equal(t, PlayerB, h.last().Side)
	st := h.state()
	require.Equal(t, PhasePlaying, st.Phase)
	require.Equal(t, PlayerB, st.Serving)
	require.Equal(t, [2]int{0, 0}, st.Points)

	// The next press is a point.
	h.press(HalfRight)
	require.Equal(t, [2]int{0, 1}, h.state().Points)
}

func TestAServeRallyAwardedToTheWrongSideCanBeTakenBack(t *testing.T) {
	h := newHarness(t, Config{Start: StartServeRally})

	h.press(HalfLeft)
	h.undo()

	require.Equal(t, PhaseServeRally, h.state().Phase)
	require.Empty(t, h.state().Serving)

	h.press(HalfRight)
	require.Equal(t, PlayerB, h.state().Serving)
}

func TestACorrectionDuringTheServeRallyChangesNothing(t *testing.T) {
	h := newHarness(t, Config{Start: StartServeRally})

	res := h.send(ScoreEvent{Half: HalfLeft, Delta: -1})

	require.Equal(t, Ignored, res.Outcome)
	require.Equal(t, PhaseServeRally, h.state().Phase)
	require.Empty(t, h.seen)
}

func TestAServeRallyCannotAlsoNameTheFirstServer(t *testing.T) {
	_, err := New(Config{MatchID: "m1", Start: StartServeRally, FirstServer: PlayerA})
	require.Error(t, err)
}

func TestTheWarmUpWaitsForBothHalves(t *testing.T) {
	h := newHarness(t, Config{Start: StartWarmup})

	st := h.state()
	require.Equal(t, PhaseWarmup, st.Phase)
	require.Equal(t, []Half{HalfLeft, HalfRight}, st.Waiting)

	h.press(HalfLeft)
	require.Equal(t, TransitionConfirmed, h.last().Kind)
	require.Equal(t, PlayerA, h.last().Side)
	require.Equal(t, []Half{HalfRight}, h.state().Waiting)

	// Pressing again does not stand in for the other player.
	res := h.press(HalfLeft)
	require.Equal(t, Ignored, res.Outcome)
	require.Equal(t, PhaseWarmup, h.state().Phase)

	h.press(HalfRight)
	require.Equal(t, TransitionWarmupOver, h.last().Kind)
	require.Equal(t, PhaseServeRally, h.state().Phase, "no first server named, so the rally decides it")
	require.Equal(t, [2]int{0, 0}, h.state().Points)
}

func TestAWarmUpWithANamedFirstServerGoesStraightToPlay(t *testing.T) {
	h := newHarness(t, Config{Start: StartWarmup, FirstServer: PlayerB})

	h.press(HalfLeft)
	h.press(HalfRight)

	require.Equal(t, PhasePlaying, h.state().Phase)
	require.Equal(t, PlayerB, h.state().Serving)
}

func TestTheEndsChangeAfterASetAndBothPlayersConfirmAtTheirNewHalf(t *testing.T) {
	h := newHarness(t, Config{BestOf: 3, ChangeEnds: true})

	h.play(repeat("a", 11))

	require.Equal(t, TransitionSetWon, h.last().Kind)
	st := h.state()
	require.Equal(t, PhaseChangeEnds, st.Phase)
	require.Equal(t, [2]Player{PlayerB, PlayerA}, st.Ends, "the new ends, the ones the players walk to")
	require.Equal(t, []Half{HalfLeft, HalfRight}, st.Waiting)

	// b has arrived at the left half; a press there is a confirmation, not a
	// point.
	h.press(HalfLeft)
	require.Equal(t, TransitionConfirmed, h.last().Kind)
	require.Equal(t, PlayerB, h.last().Side)
	require.Equal(t, [2]int{0, 0}, h.state().Points)

	h.press(HalfRight)
	require.Equal(t, TransitionEndsChanged, h.last().Kind)
	require.Equal(t, PhasePlaying, h.state().Phase)

	// From now on the left half scores for b.
	h.press(HalfLeft)
	require.Equal(t, [2]int{0, 1}, h.state().Points)
	require.Equal(t, PlayerB, h.last().Side)
}

func TestAnUndoDuringTheChangeOfEndsReopensTheSet(t *testing.T) {
	h := newHarness(t, Config{BestOf: 3, ChangeEnds: true})

	h.play(repeat("a", 11))
	h.press(HalfLeft)
	h.undo()

	st := h.state()
	require.Equal(t, PhasePlaying, st.Phase)
	require.Equal(t, 1, st.SetNumber)
	require.Equal(t, [2]int{10, 0}, st.Points)
	require.Equal(t, [2]Player{PlayerA, PlayerB}, st.Ends, "back at the ends of the first set")
	require.Empty(t, st.Waiting)
}

func TestAWonMatchDoesNotChangeEnds(t *testing.T) {
	h := newHarness(t, Config{BestOf: 3, ChangeEnds: true})

	h.play(repeat("a", 11))
	h.confirmEnds()
	h.play(repeat("a", 11))

	require.Equal(t, TransitionMatchWon, h.last().Kind)
	require.Equal(t, PhaseComplete, h.state().Phase)
	require.Empty(t, h.state().Waiting)
}

// confirmEnds plays both halves' confirmations of a change of ends.
func (h *harness) confirmEnds() {
	h.t.Helper()
	require.Equal(h.t, PhaseChangeEnds, h.state().Phase)
	h.press(HalfLeft)
	h.press(HalfRight)
}

func TestTheDecidingSetChangesEndsAtFive(t *testing.T) {
	h := newHarness(t, Config{BestOf: 3, ChangeEnds: true})

	h.play(repeat("a", 11))
	h.confirmEnds()
	h.play(repeat("b", 11))
	h.confirmEnds()

	// Set three starts at the ends of set one.
	require.Equal(t, [2]Player{PlayerA, PlayerB}, h.state().Ends)

	h.play("abaab" + "aa")
	require.Equal(t, [2]int{5, 2}, h.state().Points)
	st := h.state()
	require.Equal(t, PhaseChangeEnds, st.Phase)
	require.Equal(t, [2]Player{PlayerB, PlayerA}, st.Ends)
	serving := st.Serving

	h.confirmEnds()

	st = h.state()
	require.Equal(t, PhasePlaying, st.Phase)
	require.Equal(t, [2]int{5, 2}, st.Points, "the score carries on")
	require.Equal(t, serving, st.Serving, "only the ends change, not the service")

	// Once past five it does not happen again in the same set.
	h.play("ab")
	require.Equal(t, PhasePlaying, h.state().Phase)
}

func TestUndoingTheFifthPointTakesTheMidSetChangeBack(t *testing.T) {
	h := newHarness(t, Config{BestOf: 1, ChangeEnds: true})

	h.play("aaaaa")
	require.Equal(t, PhaseChangeEnds, h.state().Phase)

	h.undo()

	st := h.state()
	require.Equal(t, PhasePlaying, st.Phase)
	require.Equal(t, [2]int{4, 0}, st.Points)
	require.Equal(t, [2]Player{PlayerA, PlayerB}, st.Ends)
}

func TestASetThatIsNotTheDecidingOneDoesNotChangeEndsAtFive(t *testing.T) {
	h := newHarness(t, Config{BestOf: 3, ChangeEnds: true})

	h.play("aaaaa")

	require.Equal(t, PhasePlaying, h.state().Phase)
}

func TestWithoutChangeEndsAHalfAlwaysStandsForTheSamePlayer(t *testing.T) {
	h := newHarness(t, Config{BestOf: 3})

	h.play(repeat("a", 11))

	st := h.state()
	require.Equal(t, PhasePlaying, st.Phase, "no change of ends to wait for")
	require.Equal(t, [2]Player{PlayerA, PlayerB}, st.Ends)

	h.press(HalfLeft)
	require.Equal(t, [2]int{1, 0}, h.state().Points)
}

func TestTheLoserOfASetServesFirstInTheNext(t *testing.T) {
	h := newHarness(t, Config{BestOf: 5, FirstServer: PlayerA, SetOpener: OpenerLoser})

	h.play(repeat("b", 11))
	require.Equal(t, PlayerA, h.state().Serving, "a lost set one; under ITTF b would open set two")

	h.play(repeat("b", 11))
	require.Equal(t, PlayerA, h.state().Serving, "a lost again and opens again")

	h.play(repeat("a", 11))
	require.Equal(t, PlayerB, h.state().Serving, "b lost set three")
}

func TestTheLoserRuleHoldsAfterTheServeRallyToo(t *testing.T) {
	h := newHarness(t, Config{BestOf: 3, Start: StartServeRally, SetOpener: OpenerLoser})

	h.press(HalfRight)
	require.Equal(t, PlayerB, h.state().Serving)

	h.play(repeat("a", 11))
	require.Equal(t, PlayerB, h.state().Serving, "b served first and lost, so b opens set two")
}

func TestAnUnknownSetOpenerIsRejected(t *testing.T) {
	_, err := New(Config{MatchID: "m1", SetOpener: "winner"})
	require.Error(t, err)
}

func TestAnEventMustNameExactlyOneOfPlayerAndHalf(t *testing.T) {
	h := newHarness(t, Config{})

	_, err := h.apply(ScoreEvent{Player: PlayerA, Half: HalfLeft, Delta: 1})
	require.ErrorIs(t, err, ErrPlayerAndHalf)

	_, err = h.apply(ScoreEvent{Half: "middle", Delta: 1})
	require.ErrorIs(t, err, ErrUnknownHalf)

	_, err = h.apply(ScoreEvent{Delta: 1})
	require.ErrorIs(t, err, ErrUnknownPlayer)
}

// A source that names the player keeps working in every phase: the page and
// today's boards send a player, and the half is derived from the ends.
func TestAnEventByPlayerConfirmsAtTheHalfThatPlayerStandsAt(t *testing.T) {
	h := newHarness(t, Config{BestOf: 3, ChangeEnds: true})

	h.play(repeat("a", 11))
	h.point(PlayerA) // a now stands at the right

	require.Equal(t, []Half{HalfLeft}, h.state().Waiting)
}
