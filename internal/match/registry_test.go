package match

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// counter hands out predictable ids so the tests can name matches.
func counter() func() (string, error) {
	var n int
	return func() (string, error) {
		n++
		return fmt.Sprintf("m%d", n), nil
	}
}

func newTestRegistry(t *testing.T, opts ...Option) *Registry {
	t.Helper()
	return NewRegistry(append([]Option{WithIDs(counter())}, opts...)...)
}

// win plays a whole match out so it completes on court.
func win(t *testing.T, m *Match, p scorer.Player) {
	t.Helper()

	for i := 1; m.Scorer.State().Complete == false; i++ {
		_, err := m.Scorer.Apply(scorer.ScoreEvent{
			MatchID: m.ID, Player: p, Delta: 1, EventID: uint64(i), Source: "test",
		})
		require.NoError(t, err)
		require.Less(t, i, 500, "match did not finish")
	}
}

func TestCreateAssignsTheID(t *testing.T) {
	r := newTestRegistry(t)

	m, err := r.Create(scorer.Config{MatchID: "ignored", Players: [2]string{"Anna", "Bernd"}})
	require.NoError(t, err)
	require.Equal(t, "m1", m.ID)
	require.Equal(t, "m1", m.Scorer.State().MatchID)
	require.Equal(t, [2]string{"Anna", "Bernd"}, m.Scorer.State().Players)
}

func TestCreateRejectsAConfigTheScorerWillNotTake(t *testing.T) {
	r := newTestRegistry(t)

	_, err := r.Create(scorer.Config{BestOf: 4})
	require.Error(t, err)

	// And the failed match did not land in the registry.
	_, err = r.Get("m1")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestGetUnknownID(t *testing.T) {
	r := newTestRegistry(t)

	_, err := r.Get("nope")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestRandomIDsAreDistinctAndUsable(t *testing.T) {
	r := NewRegistry()

	seen := make(map[string]bool)
	for range 50 {
		m, err := r.Create(scorer.Config{})
		require.NoError(t, err)
		require.Len(t, m.ID, 8)
		require.False(t, seen[m.ID], "duplicate id %q", m.ID)
		seen[m.ID] = true
	}
}

func TestFreeIDRetriesPastACollision(t *testing.T) {
	ids := []string{"m1", "m1", "m2"}
	var i int
	r := NewRegistry(WithIDs(func() (string, error) {
		id := ids[i]
		i++
		return id, nil
	}))

	first, err := r.Create(scorer.Config{})
	require.NoError(t, err)
	require.Equal(t, "m1", first.ID)

	second, err := r.Create(scorer.Config{})
	require.NoError(t, err)
	require.Equal(t, "m2", second.ID, "the collision was drawn again, not handed out twice")
}

func TestIDGenerationFailures(t *testing.T) {
	t.Run("the generator fails", func(t *testing.T) {
		r := NewRegistry(WithIDs(func() (string, error) {
			return "", errors.New("no entropy")
		}))

		_, err := r.Create(scorer.Config{})
		require.ErrorIs(t, err, ErrIDGeneration)
	})

	t.Run("every draw collides", func(t *testing.T) {
		r := NewRegistry(WithIDs(func() (string, error) { return "same", nil }))

		_, err := r.Create(scorer.Config{})
		require.NoError(t, err)

		_, err = r.Create(scorer.Config{})
		require.ErrorIs(t, err, ErrIDGeneration)

		// And the match that is there was not replaced by the failed attempt.
		m, err := r.Get("same")
		require.NoError(t, err)
		require.True(t, m.Running())
	})
}

func TestCurrentIsTheMostRecentRunningMatch(t *testing.T) {
	r := newTestRegistry(t)

	_, err := r.Current()
	require.ErrorIs(t, err, ErrNotFound, "nothing is running yet")

	first, err := r.Create(scorer.Config{})
	require.NoError(t, err)

	current, err := r.Current()
	require.NoError(t, err)
	require.Equal(t, first, current)

	second, err := r.Create(scorer.Config{})
	require.NoError(t, err)

	current, err = r.Current()
	require.NoError(t, err)
	require.Equal(t, second, current, "the newer match takes over")

	// The newer one finishes on court, so hardware points belong to the older
	// one again rather than to nothing.
	win(t, second, scorer.PlayerA)
	current, err = r.Current()
	require.NoError(t, err)
	require.Equal(t, first, current)

	// And when that one is ended by hand there is nothing left to score into.
	first.End(time.Now())
	_, err = r.Current()
	require.ErrorIs(t, err, ErrNotFound)
}

func TestEndIsIdempotent(t *testing.T) {
	r := newTestRegistry(t)
	m, err := r.Create(scorer.Config{})
	require.NoError(t, err)

	require.True(t, m.Running())
	_, ended := m.EndedAt()
	require.False(t, ended)

	first := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	require.Equal(t, first, m.End(first))
	require.Equal(t, first, m.End(first.Add(time.Hour)), "a retry does not move the end time")

	at, ended := m.EndedAt()
	require.True(t, ended)
	require.Equal(t, first, at)
	require.False(t, m.Running())
}

func TestAMatchWonOnCourtIsNotRunning(t *testing.T) {
	r := newTestRegistry(t)
	m, err := r.Create(scorer.Config{BestOf: 1})
	require.NoError(t, err)

	win(t, m, scorer.PlayerB)
	require.False(t, m.Running())

	// It was never ended by hand, though — the two are different things, and
	// the stream switching in #5 will care about the difference.
	_, ended := m.EndedAt()
	require.False(t, ended)
}

func TestFinishedMatchesAreEvictedOldestFirst(t *testing.T) {
	r := newTestRegistry(t, WithMaxRetained(3))

	for range 3 {
		m, err := r.Create(scorer.Config{})
		require.NoError(t, err)
		m.End(time.Now())
	}

	_, err := r.Create(scorer.Config{})
	require.NoError(t, err)

	_, err = r.Get("m1")
	require.ErrorIs(t, err, ErrNotFound, "the oldest finished match was dropped")
	for _, id := range []string{"m2", "m3", "m4"} {
		_, err := r.Get(id)
		require.NoErrorf(t, err, "%s should still be there", id)
	}
}

func TestARunningMatchIsNeverEvicted(t *testing.T) {
	r := newTestRegistry(t, WithMaxRetained(2))

	running, err := r.Create(scorer.Config{}) // m1, left running
	require.NoError(t, err)

	for range 5 {
		m, err := r.Create(scorer.Config{})
		require.NoError(t, err)
		m.End(time.Now())
	}

	_, err = r.Get(running.ID)
	require.NoError(t, err, "the match in progress outlives the limit")

	current, err := r.Current()
	require.NoError(t, err)
	require.Equal(t, running, current)
}

// TestNothingIsEvictedWhileEverythingIsRunning — the limit exists to stop a
// process that runs for months from growing without bound, not to throw away a
// match that is being played.
func TestNothingIsEvictedWhileEverythingIsRunning(t *testing.T) {
	r := newTestRegistry(t, WithMaxRetained(1))

	for range 3 {
		_, err := r.Create(scorer.Config{})
		require.NoError(t, err)
	}

	for _, id := range []string{"m1", "m2", "m3"} {
		_, err := r.Get(id)
		require.NoErrorf(t, err, "%s should still be there", id)
	}

	// And once they finish, the limit catches up.
	for _, id := range []string{"m1", "m2"} {
		m, err := r.Get(id)
		require.NoError(t, err)
		m.End(time.Now())
	}

	_, err := r.Create(scorer.Config{})
	require.NoError(t, err)

	_, err = r.Get("m1")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = r.Get("m3")
	require.NoError(t, err, "still running")
}

func TestConcurrentCreateAndLookup(t *testing.T) {
	r := NewRegistry()

	var wg sync.WaitGroup
	ids := make(chan string, 50)

	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()

			m, err := r.Create(scorer.Config{})
			require.NoError(t, err)
			ids <- m.ID

			_, _ = r.Current()
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]bool)
	for id := range ids {
		require.False(t, seen[id])
		seen[id] = true

		_, err := r.Get(id)
		require.NoError(t, err)
	}
	require.Len(t, seen, 50)
}
