package scorer

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewDefaults(t *testing.T) {
	s, err := New(Config{MatchID: "m1"})
	require.NoError(t, err)

	require.Equal(t, DefaultBestOf, s.cfg.BestOf)
	require.Equal(t, 3, s.setsToWin)
	require.Equal(t, DefaultPointsPerSet, s.cfg.PointsPerSet)
	require.Equal(t, PlayerA, s.cfg.FirstServer)
	require.Equal(t, DefaultRestartQuietPeriod, s.cfg.RestartQuietPeriod)
	require.Equal(t, uint64(DefaultRestartMaxEventID), s.cfg.RestartMaxEventID)
	require.NotNil(t, s.cfg.Now)

	require.Equal(t, State{
		MatchID:   "m1",
		Players:   [2]string{"a", "b"},
		SetNumber: 1,
		Serving:   PlayerA,
	}, s.State())
}

func TestNewRejectsImpossibleConfigurations(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "no match id", cfg: Config{}},
		{name: "even best of", cfg: Config{MatchID: "m1", BestOf: 4}},
		{name: "negative best of", cfg: Config{MatchID: "m1", BestOf: -3}},
		{name: "set shorter than the required lead", cfg: Config{MatchID: "m1", PointsPerSet: 1}},
		{name: "unknown first server", cfg: Config{MatchID: "m1", FirstServer: "c"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.cfg)
			require.Error(t, err)
		})
	}
}

func TestNamedPlayersSurviveIntoTheState(t *testing.T) {
	h := newHarness(t, Config{Players: [2]string{"Anna", "Bernd"}})
	require.Equal(t, [2]string{"Anna", "Bernd"}, h.state().Players)
}

func TestMalformedEventsAreRejectedWithTheCurrentState(t *testing.T) {
	tests := []struct {
		name    string
		ev      ScoreEvent
		wantErr error
	}{
		{
			name:    "another match",
			ev:      ScoreEvent{MatchID: "m2", Player: PlayerA, Delta: 1, EventID: 1, Source: "web-1"},
			wantErr: ErrWrongMatch,
		},
		{
			name:    "unknown player",
			ev:      ScoreEvent{Player: "c", Delta: 1, EventID: 1, Source: "web-1"},
			wantErr: ErrUnknownPlayer,
		},
		{
			name: "no player at all",
			ev:   ScoreEvent{Delta: 1, EventID: 1, Source: "web-1"}, wantErr: ErrUnknownPlayer,
		},
		{
			// Without a source there is no counter to deduplicate against, and
			// every anonymous sender would share one.
			name:    "no source",
			ev:      ScoreEvent{Player: PlayerA, Delta: 1, EventID: 1},
			wantErr: ErrMissingSource,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.play("aab")

			before := h.state()
			res, err := h.raw(tt.ev)
			require.ErrorIs(t, err, tt.wantErr)
			require.Equal(t, Rejected, res.Outcome)
			require.Equal(t, before, res.State, "a rejected event still answers with the state")
			require.Equal(t, before, h.state())
			require.Len(t, h.seen, 3)
		})
	}
}

func TestNegativeDeltaCorrectsAndStopsAtZero(t *testing.T) {
	h := newHarness(t, Config{})
	h.play("aa")

	res := h.send(ScoreEvent{Player: PlayerA, Delta: -1})
	require.Equal(t, Applied, res.Outcome)
	require.Equal(t, [2]int{1, 0}, res.State.Points)

	// A score cannot go below zero, and an event that would take it there
	// changes nothing rather than half of something.
	res = h.send(ScoreEvent{Player: PlayerB, Delta: -1})
	require.Equal(t, Ignored, res.Outcome)
	require.Equal(t, [2]int{1, 0}, res.State.Points)
}

func TestStateDoesNotAliasTheScorer(t *testing.T) {
	h := newHarness(t, Config{BestOf: 5})
	h.play(repeat("a", 11))

	st := h.state()
	require.Equal(t, [][2]int{{11, 0}}, st.CompletedSets)

	st.CompletedSets[0] = [2]int{99, 99}
	st.CompletedSets = append(st.CompletedSets, [2]int{1, 2})
	st.Points[0] = 7

	require.Equal(t, [][2]int{{11, 0}}, h.state().CompletedSets)
	require.Equal(t, [2]int{0, 0}, h.state().Points)
}

func TestObserversAllSeeEveryTransitionInOrder(t *testing.T) {
	h := newHarness(t, Config{BestOf: 1})

	var second []TransitionKind
	h.s.Observe(func(tr Transition) { second = append(second, tr.Kind) })

	h.play(repeat("a", 11))
	h.undo()

	// The harness registered the first observer; both must agree, and the
	// second must have missed the nothing that happened before it subscribed.
	require.Equal(t, h.kinds(), second)
	require.Equal(t, TransitionMatchWon, h.seen[len(h.seen)-2].Kind)
	require.Equal(t, TransitionUndo, h.seen[len(h.seen)-1].Kind)
}

func TestTransitionCarriesTheStateAsItWasThen(t *testing.T) {
	h := newHarness(t, Config{BestOf: 5})
	h.play(repeat("ab", 9) + "aa")

	setWon := h.seen[len(h.seen)-1]
	require.Equal(t, TransitionSetWon, setWon.Kind)
	require.Equal(t, [][2]int{{11, 9}}, setWon.State.CompletedSets)

	// Later points must not reach back into a transition already published.
	h.play("bbb")
	require.Equal(t, [2]int{0, 0}, setWon.State.Points)
	require.Equal(t, 2, setWon.State.SetNumber)
}

func TestConcurrentSourcesAreSerialised(t *testing.T) {
	s, err := New(Config{MatchID: "m1", BestOf: 5, Now: time.Now})
	require.NoError(t, err)

	var mu sync.Mutex
	var transitions int
	s.Observe(func(Transition) {
		mu.Lock()
		defer mu.Unlock()
		transitions++
	})

	const (
		sources = 4
		each    = 5
	)

	var wg sync.WaitGroup
	for src := range sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				_, err := s.Apply(ScoreEvent{
					MatchID: "m1",
					Player:  PlayerA,
					Delta:   1,
					EventID: uint64(i + 1),
					Source:  string(rune('a' + src)),
				})
				require.NoError(t, err)
			}
		}()
	}
	wg.Wait()

	// Twenty points to a, so: 11:0, then 9:0 of the second set.
	st := s.State()
	require.Equal(t, [2]int{1, 0}, st.Sets)
	require.Equal(t, [2]int{9, 0}, st.Points)
	require.Equal(t, sources*each, transitions)
}

func TestOutcomeString(t *testing.T) {
	require.Equal(t, "rejected", Rejected.String())
	require.Equal(t, "applied", Applied.String())
	require.Equal(t, "ignored", Ignored.String())
	require.Equal(t, "duplicate", Duplicate.String())
}
