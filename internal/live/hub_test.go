package live

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

func transition(matchID string, kind scorer.TransitionKind, a, b int) scorer.Transition {
	return scorer.Transition{
		Kind: kind,
		State: scorer.State{
			MatchID: matchID,
			Players: [2]string{"Anna", "Bernd"},
			Points:  [2]int{a, b},
		},
	}
}

func receive(t *testing.T, sub *Subscription) Event {
	t.Helper()
	select {
	case ev := <-sub.Events():
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an event")
		return Event{}
	}
}

func requireQuiet(t *testing.T, sub *Subscription) {
	t.Helper()
	select {
	case ev := <-sub.Events():
		t.Fatalf("unexpected event: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestASubscriberGetsTheTransitionsOfItsMatch(t *testing.T) {
	h := New()
	sub := h.Subscribe("m1")
	defer sub.Close()

	h.Observe(transition("m1", scorer.TransitionPoint, 1, 0))
	h.Observe(transition("m1", scorer.TransitionPoint, 1, 1))

	require.Equal(t, Event{Kind: "point", State: transition("m1", scorer.TransitionPoint, 1, 0).State}, receive(t, sub))
	require.Equal(t, [2]int{1, 1}, receive(t, sub).State.Points)
}

// Two people scoring different matches on the same server is the case this
// guards: a point in one must not appear in the other's feed.
func TestASubscriberOnlyGetsItsOwnMatch(t *testing.T) {
	h := New()
	one, two := h.Subscribe("m1"), h.Subscribe("m2")
	defer one.Close()
	defer two.Close()

	h.Observe(transition("m2", scorer.TransitionPoint, 0, 1))

	require.Equal(t, "m2", receive(t, two).State.MatchID)
	requireQuiet(t, one)
}

func TestEveryWatcherOfAMatchGetsEveryTransition(t *testing.T) {
	h := New()
	subs := make([]*Subscription, 5)
	for i := range subs {
		subs[i] = h.Subscribe("m1")
		defer subs[i].Close()
	}

	h.Observe(transition("m1", scorer.TransitionPoint, 3, 2))

	for i, sub := range subs {
		require.Equal(t, [2]int{3, 2}, receive(t, sub).State.Points, "subscriber %d", i)
	}
	require.Equal(t, 5, h.Subscribers("m1"))
}

func TestTheKindOfEveryTransitionSurvives(t *testing.T) {
	h := New()
	sub := h.Subscribe("m1")
	defer sub.Close()

	for _, kind := range []scorer.TransitionKind{
		scorer.TransitionPoint, scorer.TransitionSetWon,
		scorer.TransitionMatchWon, scorer.TransitionUndo,
	} {
		h.Observe(transition("m1", kind, 0, 0))
		require.Equal(t, string(kind), receive(t, sub).Kind)
	}
}

// The property the whole design turns on: the scorer calls Observe under its
// own lock, so a watcher that has stopped reading must not hold up the match.
func TestAWatcherThatStoppedReadingDoesNotBlockTheMatch(t *testing.T) {
	h := New(WithBuffer(4))
	stalled := h.Subscribe("m1")
	defer stalled.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 500 {
			h.Observe(transition("m1", scorer.TransitionPoint, i, 0))
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publishing blocked on a subscriber that was not reading")
	}
	require.Positive(t, stalled.Dropped())
}

// And what it keeps is the newest, because the state is complete rather than a
// delta: a client that missed three points and gets the fourth is not missing
// anything the score depends on.
func TestAWatcherThatFellBehindKeepsTheNewestScore(t *testing.T) {
	h := New(WithBuffer(2))
	sub := h.Subscribe("m1")
	defer sub.Close()

	for i := 1; i <= 10; i++ {
		h.Observe(transition("m1", scorer.TransitionPoint, i, 0))
	}

	var last Event
	for {
		select {
		case ev := <-sub.Events():
			last = ev
			continue
		case <-time.After(50 * time.Millisecond):
		}
		break
	}

	require.Equal(t, [2]int{10, 0}, last.State.Points, "the newest score was dropped in favour of an older one")
	require.Equal(t, uint64(8), sub.Dropped())
}

// A slow watcher must not cost a fast one anything.
func TestOneStalledWatcherDoesNotAffectAnother(t *testing.T) {
	h := New(WithBuffer(2))
	stalled, healthy := h.Subscribe("m1"), h.Subscribe("m1")
	defer stalled.Close()
	defer healthy.Close()

	for i := 1; i <= 6; i++ {
		h.Observe(transition("m1", scorer.TransitionPoint, i, 0))
		require.Equal(t, [2]int{i, 0}, receive(t, healthy).State.Points)
	}
	require.Zero(t, healthy.Dropped())
	require.Positive(t, stalled.Dropped())
}

func TestClosingReleasesTheSubscription(t *testing.T) {
	h := New()
	sub := h.Subscribe("m1")
	require.Equal(t, 1, h.Subscribers("m1"))

	sub.Close()

	require.Equal(t, 0, h.Subscribers("m1"))
	require.Equal(t, 0, h.Matches(), "the match kept an empty entry in the hub")

	select {
	case <-sub.Done():
	default:
		t.Fatal("Done was not closed")
	}
}

func TestClosingTwiceIsHarmless(t *testing.T) {
	h := New()
	sub := h.Subscribe("m1")
	sub.Close()
	require.NotPanics(t, sub.Close)
}

func TestAClosedSubscriptionGetsNothingMore(t *testing.T) {
	h := New()
	sub := h.Subscribe("m1")
	sub.Close()

	h.Observe(transition("m1", scorer.TransitionPoint, 1, 0))
	requireQuiet(t, sub)
}

// The leak the issue names: a match watched all afternoon from a phone that
// sleeps and wakes repeatedly must not accumulate anything.
func TestManyConnectionsOpenedAndDroppedLeaveNothingBehind(t *testing.T) {
	h := New()

	for range 1000 {
		sub := h.Subscribe("m1")
		h.Observe(transition("m1", scorer.TransitionPoint, 1, 0))
		sub.Close()
	}

	require.Equal(t, 0, h.Subscribers("m1"))
	require.Equal(t, 0, h.Matches())
}

func TestSubscribingToManyMatchesAndLeavingCleansUpEveryEntry(t *testing.T) {
	h := New()
	subs := make([]*Subscription, 0, 200)
	for i := range 200 {
		subs = append(subs, h.Subscribe(string(rune('a'+i%26))+string(rune('0'+i/26))))
	}
	require.Positive(t, h.Matches())

	for _, sub := range subs {
		sub.Close()
	}
	require.Equal(t, 0, h.Matches())
}

// Subscribing, publishing and closing all race in normal use: a browser can
// disconnect in the middle of a point. Run with -race.
func TestSubscribeObserveAndCloseUnderRace(t *testing.T) {
	h := New(WithBuffer(2))
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			h.Observe(transition("m1", scorer.TransitionPoint, i, 0))
		}
	}()

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				sub := h.Subscribe("m1")
				select {
				case <-sub.Events():
				default:
				}
				sub.Close()
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	require.Equal(t, 0, h.Subscribers("m1"))
}

func TestABufferOfZeroFallsBackToTheDefault(t *testing.T) {
	h := New(WithBuffer(0))
	require.Equal(t, DefaultBuffer, h.buffer)
}

func TestATransitionForAMatchNobodyWatchesIsDropped(t *testing.T) {
	h := New()
	require.NotPanics(t, func() {
		h.Observe(transition("nobody-is-watching", scorer.TransitionPoint, 1, 0))
	})
	require.Equal(t, 0, h.Matches())
}
