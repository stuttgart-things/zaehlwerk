package figure

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
)

// Roster is where a name is looked up: Schmetterpause's player list.
type Roster interface {
	Players(ctx context.Context) ([]schmetterpause.Player, error)
}

const (
	// fresh is how long a fetched list answers before it is fetched again:
	// a player who joined shows their figure a minute later at the latest.
	fresh = time.Minute
	// lookup bounds a fetch. It runs while a match is being created, which
	// a Schmetterpause that has stopped answering must not hold up for long.
	lookup = 2 * time.Second
)

// Names finds the players Schmetterpause has under a match's names, for a
// match that has names only: one started on the page by name, a demo, a game
// of Pong's. Their figures are then the players' own, on the spectator view
// and the panel, rather than a random one or none.
//
// The ids it finds pick a drawing and nothing else. The match is still not
// reported, and the stream still carries no ids (ADR-0008): a match that is
// meant to be a result says so by its Handover.
type Names struct {
	roster Roster
	log    *slog.Logger
	now    func() time.Time

	// mu is held across a fetch: two matches created at once wait for the
	// one list rather than both asking for it.
	mu     sync.Mutex
	at     time.Time
	byName map[string]string
}

// NewNames looks names up in roster.
func NewNames(roster Roster, log *slog.Logger) *Names {
	return &Names{roster: roster, log: log, now: time.Now}
}

// IDs is the ids of the players called names, empty for a name nobody has.
// A list that cannot be fetched leaves the last one answering, or none.
func (n *Names) IDs(ctx context.Context, names [2]string) [2]string {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.at.IsZero() || n.now().Sub(n.at) >= fresh {
		n.fetch(ctx)
	}
	var ids [2]string
	for i, name := range names {
		ids[i] = n.byName[name]
	}
	return ids
}

// fetch replaces the list. Must be called with mu held.
func (n *Names) fetch(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, lookup)
	defer cancel()
	players, err := n.roster.Players(ctx)
	if err != nil {
		// Info, not a warning: the match goes on, its figures random or
		// none as before there was a list.
		n.log.Info("no players to look the names up in, a match by names shows no player's figure", "error", err)
		// Not asked again on every match while Schmetterpause is down.
		n.at = n.now()
		return
	}
	byName := make(map[string]string, len(players))
	for _, p := range players {
		// Two players under one name are a coin toss; neither is shown.
		if _, twice := byName[p.DisplayName]; twice {
			byName[p.DisplayName] = ""
			continue
		}
		byName[p.DisplayName] = p.ID
	}
	n.byName, n.at = byName, n.now()
}
