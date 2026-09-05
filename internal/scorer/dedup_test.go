package scorer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDuplicateEventsAreDiscarded(t *testing.T) {
	h := newHarness(t, Config{})

	res, err := h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 7, Source: "button-a"})
	require.NoError(t, err)
	require.Equal(t, Applied, res.Outcome)
	require.Equal(t, [2]int{1, 0}, res.State.Points)

	// The sender did not see the answer and tries again.
	res, err = h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 7, Source: "button-a"})
	require.NoError(t, err, "a duplicate is not an error — the sender should stop, not retry harder")
	require.Equal(t, Duplicate, res.Outcome)
	require.Equal(t, [2]int{1, 0}, res.State.Points, "the answer carries the current state")

	require.Len(t, h.seen, 1, "a discarded event is not a transition")
}

func TestOutOfOrderAndSkippedEventIDs(t *testing.T) {
	h := newHarness(t, Config{})

	res, err := h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 9, Source: "button-a"})
	require.NoError(t, err)
	require.Equal(t, Applied, res.Outcome)

	// Below the watermark: an event that overtook another one on the way in.
	res, err = h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 7, Source: "button-a"})
	require.NoError(t, err)
	require.Equal(t, Duplicate, res.Outcome)

	// Discarding it must not have pulled the watermark down to 7, or the 8 that
	// was already dealt with would come back as new.
	res, err = h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 8, Source: "button-a"})
	require.NoError(t, err)
	require.Equal(t, Duplicate, res.Outcome)

	// Above it, with a gap: events were lost, not duplicated.
	res, err = h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 40, Source: "button-a"})
	require.NoError(t, err)
	require.Equal(t, Applied, res.Outcome)
	require.Equal(t, [2]int{2, 0}, h.state().Points)
}

func TestEventIDsAreCountedPerSource(t *testing.T) {
	h := newHarness(t, Config{})

	// One hub posts for several buttons; the counters belong to the buttons.
	res, err := h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 500, Source: "button-a"})
	require.NoError(t, err)
	require.Equal(t, Applied, res.Outcome)

	res, err = h.raw(ScoreEvent{Player: PlayerB, Delta: 1, EventID: 1, Source: "button-b"})
	require.NoError(t, err)
	require.Equal(t, Applied, res.Outcome, "a fresh source starts its own count")

	res, err = h.raw(ScoreEvent{Player: PlayerB, Delta: 1, EventID: 1, Source: "phone-7"})
	require.NoError(t, err)
	require.Equal(t, Applied, res.Outcome)

	res, err = h.raw(ScoreEvent{Player: PlayerB, Delta: 1, EventID: 1, Source: "phone-7"})
	require.NoError(t, err)
	require.Equal(t, Duplicate, res.Outcome)

	require.Equal(t, [2]int{1, 2}, h.state().Points)
}

func TestSourceRestartIsNotADuplicate(t *testing.T) {
	h := newHarness(t, Config{})

	res, err := h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 5000, Source: "button-a"})
	require.NoError(t, err)
	require.Equal(t, Applied, res.Outcome)

	// Reflashed. The counter starts over, and it took long enough that the
	// button cannot have been retrying.
	h.clock.advance(DefaultRestartQuietPeriod)

	res, err = h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 0, Source: "button-a"})
	require.NoError(t, err)
	require.Equal(t, Applied, res.Outcome, "the button must not be locked out until the next match")
	require.Equal(t, [2]int{2, 0}, h.state().Points)

	// Counting continues from the new watermark.
	res, err = h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 1, Source: "button-a"})
	require.NoError(t, err)
	require.Equal(t, Applied, res.Outcome)

	res, err = h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 1, Source: "button-a"})
	require.NoError(t, err)
	require.Equal(t, Duplicate, res.Outcome)
	require.Equal(t, [2]int{3, 0}, h.state().Points)
}

func TestARestartIsOnlyASilentSourceWithALowCounter(t *testing.T) {
	tests := []struct {
		name  string
		quiet time.Duration
		id    uint64
		want  Outcome
	}{
		{
			name:  "quiet and back at zero is a restart",
			quiet: DefaultRestartQuietPeriod,
			id:    0,
			want:  Applied,
		},
		{
			name:  "a busy source repeating a low id is a duplicate",
			quiet: DefaultRestartQuietPeriod - time.Second,
			id:    0,
			want:  Duplicate,
		},
		{
			name:  "a quiet source repeating a high id is a duplicate, not a restart",
			quiet: 24 * time.Hour,
			id:    4999,
			want:  Duplicate,
		},
		{
			name:  "the margin covers a few lost events after the restart",
			quiet: DefaultRestartQuietPeriod,
			id:    DefaultRestartMaxEventID,
			want:  Applied,
		},
		{
			name:  "one past the margin is still a duplicate",
			quiet: DefaultRestartQuietPeriod,
			id:    DefaultRestartMaxEventID + 1,
			want:  Duplicate,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, Config{})

			_, err := h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 5000, Source: "button-a"})
			require.NoError(t, err)

			h.clock.advance(tt.quiet)

			res, err := h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: tt.id, Source: "button-a"})
			require.NoError(t, err)
			require.Equal(t, tt.want, res.Outcome)
		})
	}
}

// TestRetriesKeepASourceFromAgeingIntoARestart pins that the quiet period is
// measured from the last event heard, not the last one applied. A button
// bouncing out the same id for minutes is doing the opposite of being silent.
func TestRetriesKeepASourceFromAgeingIntoARestart(t *testing.T) {
	h := newHarness(t, Config{})

	_, err := h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 5000, Source: "button-a"})
	require.NoError(t, err)

	for range 10 {
		h.clock.advance(DefaultRestartQuietPeriod / 2)

		res, err := h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 5000, Source: "button-a"})
		require.NoError(t, err)
		require.Equal(t, Duplicate, res.Outcome)
	}

	h.clock.advance(DefaultRestartQuietPeriod / 2)

	res, err := h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 0, Source: "button-a"})
	require.NoError(t, err)
	require.Equal(t, Duplicate, res.Outcome)
	require.Equal(t, [2]int{1, 0}, h.state().Points)
}

func TestRestartDetectionIsConfigurable(t *testing.T) {
	h := newHarness(t, Config{RestartQuietPeriod: time.Hour, RestartMaxEventID: 1})

	_, err := h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 5000, Source: "button-a"})
	require.NoError(t, err)

	// Not silent for long enough yet. Every attempt resets the silence, so
	// each case below waits out the configured hour again.
	h.clock.advance(59 * time.Minute)
	res, err := h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 0, Source: "button-a"})
	require.NoError(t, err)
	require.Equal(t, Duplicate, res.Outcome)

	h.clock.advance(time.Hour)
	res, err = h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 2, Source: "button-a"})
	require.NoError(t, err)
	require.Equal(t, Duplicate, res.Outcome, "2 is past the configured margin")

	h.clock.advance(time.Hour)
	res, err = h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 1, Source: "button-a"})
	require.NoError(t, err)
	require.Equal(t, Applied, res.Outcome)
}

// TestUndoDoesNotRollBackTheWatermark is the deliberate asymmetry between the
// two mechanisms: undo is a decision, a retry is an accident of the transport.
// Rolling the watermark back would let the next retry undo the undo.
func TestUndoDoesNotRollBackTheWatermark(t *testing.T) {
	h := newHarness(t, Config{})

	_, err := h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 7, Source: "button-a"})
	require.NoError(t, err)
	require.Equal(t, [2]int{1, 0}, h.state().Points)

	h.undo()
	require.Equal(t, [2]int{0, 0}, h.state().Points)

	res, err := h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 7, Source: "button-a"})
	require.NoError(t, err)
	require.Equal(t, Duplicate, res.Outcome)
	require.Equal(t, [2]int{0, 0}, h.state().Points, "the retry must not resurrect the point")
}

// TestUnattributedHitsAreRecorded — the piezo adapter sends Delta 0 for a hit
// it cannot pin on a player. It changes nothing, but it is still an event and
// its id must be consumed, or its retry would be treated as new.
func TestUnattributedHitsAreRecorded(t *testing.T) {
	h := newHarness(t, Config{})

	res, err := h.raw(ScoreEvent{Player: PlayerA, Delta: 0, EventID: 3, Source: "piezo-1"})
	require.NoError(t, err)
	require.Equal(t, Ignored, res.Outcome)
	require.Equal(t, [2]int{0, 0}, res.State.Points)
	require.Empty(t, h.seen, "nothing changed, so there is nothing to publish")

	res, err = h.raw(ScoreEvent{Player: PlayerA, Delta: 0, EventID: 3, Source: "piezo-1"})
	require.NoError(t, err)
	require.Equal(t, Duplicate, res.Outcome)

	res, err = h.raw(ScoreEvent{Player: PlayerA, Delta: 1, EventID: 4, Source: "piezo-1"})
	require.NoError(t, err)
	require.Equal(t, Applied, res.Outcome)
	require.Equal(t, [2]int{1, 0}, h.state().Points)
}
