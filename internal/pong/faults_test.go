package pong

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

func TestFaultsAreReadAsNamesWithShares(t *testing.T) {
	f, err := ParseFaults("wrong_half=0.05, resend=0.1,correct=true,counter_reset=1")
	require.NoError(t, err)
	require.Equal(t, Faults{WrongHalf: 0.05, Resend: 0.1, Correct: true, CounterReset: 1}, f)
}

func TestNoFaultsIsAnEmptyValue(t *testing.T) {
	f, err := ParseFaults("")
	require.NoError(t, err)
	require.Equal(t, Faults{}, f)
}

func TestAFaultItDoesNotKnowIsRefusedRatherThanIgnored(t *testing.T) {
	for _, raw := range []string{"wrong-half=0.1", "lost", "jitter=0.2", "correct=maybe"} {
		_, err := ParseFaults(raw)
		require.Error(t, err, raw)
	}
}

func TestAShareOutsideZeroToOneIsRefused(t *testing.T) {
	for _, raw := range []string{"lost=1.5", "ghost=-0.1", "bounce=NaN", "resend=lots"} {
		_, err := ParseFaults(raw)
		require.Error(t, err, raw)
	}
}

func TestAFaultNamedTwiceIsRefused(t *testing.T) {
	_, err := ParseFaults("lost=0.1,lost=0.2")
	require.ErrorContains(t, err, "named twice")
}

func TestACounterResetWaitsOutZaehlwerksQuietPeriod(t *testing.T) {
	// zaehlwerk-api leaves both at the scorer's defaults. If either moves,
	// a reset is discarded as a duplicate and every faulted run has findings.
	require.Greater(t, resetQuiet, scorer.DefaultRestartQuietPeriod)
	require.LessOrEqual(t, uint64(resetEventID), uint64(scorer.DefaultRestartMaxEventID))
}
