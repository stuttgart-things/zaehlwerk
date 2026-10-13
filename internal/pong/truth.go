package pong

import (
	"fmt"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// Truth is who really won, kept by the game because it decided every rally
// before anything was sent. zaehlwerk's score is compared with it after every
// rally.
//
// It counts rallies, not sets. Whether ten rallies to eight is a set is
// zaehlwerk's rule, and a game that applied it as well would be a second
// scorer that can only ever agree with the first. Rallies are compared with
// what zaehlwerk has awarded over the whole match, which is arithmetic over
// its state and no rule at all.
type Truth struct {
	// Won is the rallies each player really won while points were played.
	Won [2]int
	// ServeRally is who won the rally for the first serve, empty before it.
	ServeRally scorer.Player
}

// Rally records a rally won while points are played.
func (t *Truth) Rally(winner scorer.Player) { t.Won[index(winner)]++ }

// Divergence is a rally after which the truth and zaehlwerk disagree.
type Divergence struct {
	// EventID is the rally's number, the event id it was reported with.
	EventID uint64 `json:"event_id"`
	// What disagrees: "points", "serve" or "ends".
	What      string `json:"what"`
	Truth     string `json:"truth"`
	Zaehlwerk string `json:"zaehlwerk"`
}

// Points compares the rallies won with the points zaehlwerk has awarded, and
// reports a divergence if they differ.
func (t Truth) Points(eventID uint64, st scorer.State) (Divergence, bool) {
	awarded := Awarded(st)
	if awarded == t.Won {
		return Divergence{}, false
	}
	return Divergence{EventID: eventID, What: "points",
		Truth: score(t.Won), Zaehlwerk: score(awarded)}, true
}

// Serve compares who won the serve rally with who zaehlwerk says serves first.
func (t Truth) Serve(eventID uint64, st scorer.State) (Divergence, bool) {
	if st.Serving == t.ServeRally {
		return Divergence{}, false
	}
	return Divergence{EventID: eventID, What: "serve",
		Truth: string(t.ServeRally), Zaehlwerk: string(st.Serving)}, true
}

// Ends compares where the bots stand with where zaehlwerk says they stand.
// They walk only when zaehlwerk calls a change of ends, so ends that move
// without one would put the next point on the wrong player.
func Ends(eventID uint64, standing [2]scorer.Player, st scorer.State) (Divergence, bool) {
	if st.Ends == standing {
		return Divergence{}, false
	}
	return Divergence{EventID: eventID, What: "ends",
		Truth: ends(standing), Zaehlwerk: ends(st.Ends)}, true
}

// Awarded is the points zaehlwerk has given each player over the whole match:
// every finished set, and the set in progress. Once the match is won, the
// final set is both the last of CompletedSets and Points, and counts once.
func Awarded(st scorer.State) [2]int {
	var total [2]int
	for _, set := range st.CompletedSets {
		total[0] += set[0]
		total[1] += set[1]
	}
	if !st.Complete {
		total[0] += st.Points[0]
		total[1] += st.Points[1]
	}
	return total
}

func score(s [2]int) string { return fmt.Sprintf("%d:%d", s[0], s[1]) }

func ends(e [2]scorer.Player) string { return fmt.Sprintf("%s:%s", e[0], e[1]) }

// index is 0 for a and 1 for b. The game names bot 0 as player a when it
// starts the match, so the bot's index and the player's are the same.
func index(p scorer.Player) int {
	if p == scorer.PlayerB {
		return 1
	}
	return 0
}

func playerAt(i int) scorer.Player {
	if i == 1 {
		return scorer.PlayerB
	}
	return scorer.PlayerA
}

// halfOf is the half a player stands at.
func halfOf(p scorer.Player, standing [2]scorer.Player) scorer.Half {
	if standing[1] == p {
		return scorer.HalfRight
	}
	return scorer.HalfLeft
}
