package figure

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
)

// roster counts how often it was asked, and fails while err is set.
type roster struct {
	mu      sync.Mutex
	players []schmetterpause.Player
	err     error
	asked   int
}

func (r *roster) Players(context.Context) ([]schmetterpause.Player, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked++
	return r.players, r.err
}

// names is a lookup on r at a clock the test moves.
func names(r *roster) (*Names, *time.Time) {
	clock := time.Date(2026, 10, 29, 17, 0, 0, 0, time.UTC)
	n := NewNames(r, quiet())
	n.now = func() time.Time { return clock }
	return n, &clock
}

var anna = schmetterpause.Player{ID: "id-anna", DisplayName: "Anna"}

func TestANameSchmetterpauseKnowsIsThatPlayer(t *testing.T) {
	n, _ := names(&roster{players: []schmetterpause.Player{anna}})

	require.Equal(t, [2]string{"id-anna", ""}, n.IDs(context.Background(), [2]string{"Anna", "Gast"}))
}

// A match is created while a point may be waiting; the list is not fetched
// for every one.
func TestTheListIsFetchedOnceAMinute(t *testing.T) {
	r := &roster{players: []schmetterpause.Player{anna}}
	n, clock := names(r)

	n.IDs(context.Background(), [2]string{"Anna", "Bernd"})
	*clock = clock.Add(fresh - time.Second)
	n.IDs(context.Background(), [2]string{"Anna", "Bernd"})
	require.Equal(t, 1, r.asked)

	r.players = append(r.players, schmetterpause.Player{ID: "id-bernd", DisplayName: "Bernd"})
	*clock = clock.Add(time.Second)
	require.Equal(t, [2]string{"id-anna", "id-bernd"}, n.IDs(context.Background(), [2]string{"Anna", "Bernd"}),
		"a player who joined is there a minute later")
	require.Equal(t, 2, r.asked)
}

// Schmetterpause down: the last list answers, and it is not asked again on
// every match.
func TestAListThatCannotBeFetchedKeepsTheLastOne(t *testing.T) {
	r := &roster{players: []schmetterpause.Player{anna}}
	n, clock := names(r)
	n.IDs(context.Background(), [2]string{"Anna", ""})

	r.err = errors.New("schmetterpause: 503")
	*clock = clock.Add(fresh)
	require.Equal(t, [2]string{"id-anna", ""}, n.IDs(context.Background(), [2]string{"Anna", ""}))
	n.IDs(context.Background(), [2]string{"Anna", ""})
	require.Equal(t, 2, r.asked)
}

func TestWithoutAnyListNobodyIsFound(t *testing.T) {
	r := &roster{err: errors.New("schmetterpause: 401")}
	n, _ := names(r)

	require.Equal(t, [2]string{}, n.IDs(context.Background(), [2]string{"Anna", "Bernd"}))
	n.IDs(context.Background(), [2]string{"Anna", "Bernd"})
	require.Equal(t, 1, r.asked)
}

// Two players under one name: whichever figure it showed would be a guess.
func TestANameTwoPlayersShareFindsNeither(t *testing.T) {
	n, _ := names(&roster{players: []schmetterpause.Player{anna, {ID: "id-anna-2", DisplayName: "Anna"}}})

	require.Equal(t, [2]string{}, n.IDs(context.Background(), [2]string{"Anna", ""}))
}
