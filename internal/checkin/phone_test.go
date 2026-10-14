package checkin

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// phone is Schmetterpause claiming a half for a player who confirmed on
// their phone, with the check-in it recorded.
func (h *harness) phone(player, checkinID string, half scorer.Half) Answer {
	return h.desk.ClaimByPhone(context.Background(),
		PhoneClaim{Half: half, PlayerID: player, CheckinID: checkinID}, h.table)
}

func TestAPhoneClaimTakesAHalfLikeAFob(t *testing.T) {
	h := newHarness(t, WithTimeout(time.Minute))

	a := h.phone("anna", "anna-phone-1", scorer.HalfLeft)
	require.Equal(t, Claimed, a.Outcome)
	require.Equal(t, "Anna", a.Name, "the name comes from the player list")
	require.Equal(t, [2]string{"Anna", ""}, a.Halves)
	require.Zero(t, h.sp.calls(), "nothing to resolve: Schmetterpause sent the ids")

	// The same rules as a fob from here on.
	require.Equal(t, Unchanged, h.phone("anna", "anna-phone-2", scorer.HalfLeft).Outcome)
	require.Equal(t, "anna-phone-2", h.desk.claims[0].CheckinID, "the newer check-in is kept")
	require.Equal(t, Moved, h.phone("anna", "anna-phone-3", scorer.HalfRight).Outcome)
	require.Equal(t, Replaced, h.phone("clara", "clara-phone-1", scorer.HalfRight).Outcome)

	h.clock.advance(time.Minute)
	require.Nil(t, h.desk.Claims()[1], "a lone phone claim times out too")
}

func TestOneHalfByFobAndTheOtherByPhoneStartTheMatch(t *testing.T) {
	h := newHarness(t)

	require.Equal(t, Claimed, h.checkIn(t, annaFob, scorer.HalfLeft).Outcome)
	a := h.phone("bernd", "bernd-phone-1", scorer.HalfRight)
	require.Equal(t, Started, a.Outcome)
	require.NotEmpty(t, a.MatchID)

	m, ok := h.table.r.Occupied()
	require.True(t, ok)
	require.Equal(t, match.Handover{
		HomeID: "anna", AwayID: "bernd",
		HomeCheckinID: "anna-checkin-1", AwayCheckinID: "bernd-phone-1",
	}, m.Handover, "both check-ins, and no operator")
	require.Equal(t, [2]string{"Anna", "Bernd"}, m.Scorer.State().Players)
}

func TestAPhoneClaimDuringARunningMatchIsRefusedAndChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.phone("clara", "clara-phone-1", scorer.HalfLeft)
	running, err := h.table.r.Create(scorer.Config{})
	require.NoError(t, err)

	a := h.phone("anna", "anna-phone-1", scorer.HalfRight)
	require.Equal(t, Refused, a.Outcome)
	require.Error(t, a.Err)

	claims := h.desk.Claims()
	require.Equal(t, "Clara", claims[0].Name)
	require.Nil(t, claims[1])
	current, err := h.table.r.Current()
	require.NoError(t, err)
	require.Same(t, running, current)
}

func TestTheSameCheckinIDTwiceClaimsOnce(t *testing.T) {
	h := newHarness(t)
	h.phone("anna", "anna-phone-1", scorer.HalfLeft)

	first := h.phone("bernd", "bernd-phone-1", scorer.HalfRight)
	require.Equal(t, Started, first.Outcome)

	// A proxy retry after the match started: the first answer again, not a
	// refusal, and not a second match.
	again := h.phone("bernd", "bernd-phone-1", scorer.HalfRight)
	require.True(t, again.Repeated)
	require.False(t, first.Repeated)
	require.Equal(t, Started, again.Outcome)
	require.Equal(t, first.MatchID, again.MatchID)
	require.Equal(t, [2]string{"", ""}, h.desk.halves(), "it claimed nothing")
}

func TestARepeatedPhoneClaimWhileTheFirstIsUnderWayWaitsForIt(t *testing.T) {
	h := newHarness(t)
	gate := make(chan struct{})
	h.sp.players = gate

	answers := make(chan Answer, 2)
	for range 2 {
		go func() { answers <- h.phone("anna", "anna-phone-1", scorer.HalfLeft) }()
	}
	time.Sleep(20 * time.Millisecond)
	close(gate)

	a, b := <-answers, <-answers
	require.Equal(t, Claimed, a.Outcome)
	require.Equal(t, Claimed, b.Outcome)
	require.NotEqual(t, a.Repeated, b.Repeated, "one is the attempt, the other its repeat")
}

func TestThePhoneClaimsRememberedAreBounded(t *testing.T) {
	h := newHarness(t)
	for i := range maxPhoneClaims + 10 {
		h.phone("anna", fmt.Sprintf("anna-phone-%d", i), scorer.HalfLeft)
	}
	require.Len(t, h.desk.phone, maxPhoneClaims)
	require.Len(t, h.desk.phoneOrder, maxPhoneClaims)
	require.NotContains(t, h.desk.phone, "anna-phone-0", "the oldest is forgotten first")
	require.Contains(t, h.desk.phone, fmt.Sprintf("anna-phone-%d", maxPhoneClaims+9))
}

func TestAPhoneClaimLogsNoIDs(t *testing.T) {
	h := newHarness(t)
	h.phone("anna", "anna-phone-1", scorer.HalfLeft)
	h.phone("anna", "anna-phone-1", scorer.HalfLeft)
	h.phone("bernd", "bernd-phone-1", scorer.HalfRight)

	require.Contains(t, h.logs.String(), `"via":"phone"`)
	require.NotContains(t, h.logs.String(), "anna-phone")
	require.NotContains(t, h.logs.String(), "bernd-phone")
}

func TestAPhoneClaimIsValidatedAndWrittenInOneSpelling(t *testing.T) {
	const player, checkin = "11111111-1111-4111-8111-111111111111", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

	c := PhoneClaim{Half: scorer.HalfRight, PlayerID: player, CheckinID: "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE"}
	require.NoError(t, c.Validate())
	require.Equal(t, checkin, c.CheckinID, "an upper-case repeat is the same check-in")

	for _, bad := range []PhoneClaim{
		{Half: "middle", PlayerID: player, CheckinID: checkin},
		{Half: "", PlayerID: player, CheckinID: checkin},
		{Half: scorer.HalfLeft, PlayerID: "anna", CheckinID: checkin},
		{Half: scorer.HalfLeft, PlayerID: player, CheckinID: ""},
		{Half: scorer.HalfLeft, PlayerID: player, CheckinID: "{" + checkin + "}"},
		{Half: scorer.HalfLeft, PlayerID: "urn:uuid:" + player, CheckinID: checkin},
		{Half: scorer.HalfLeft, PlayerID: player, CheckinID: "aaaaaaaabbbb4ccc8dddeeeeeeeeeeee"},
	} {
		require.Error(t, bad.Validate(), "%+v", bad)
	}
}
