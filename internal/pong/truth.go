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
//
// Faults sit between the truth and zaehlwerk, so they never touch Won. What
// they should do to zaehlwerk's score is kept beside it, in Explained.
type Truth struct {
	// Won is the rallies each player really won while points were played.
	Won [2]int
	// ServeRally is who won the rally for the first serve, empty before it.
	ServeRally scorer.Player
	// Explained is how far the injected faults should have moved zaehlwerk's
	// points away from Won: a lost press is -1 for its winner, a ghost or a
	// bounce +1 for the player at its half.
	Explained [2]int
	// ExplainedServe is who zaehlwerk should say serves after the injected
	// faults: ServeRally, unless a wrong half went uncorrected.
	ExplainedServe scorer.Player
}

// Rally records a rally won while points are played.
func (t *Truth) Rally(winner scorer.Player) { t.Won[index(winner)]++ }

// Explain records what an injected fault should do to zaehlwerk's points.
func (t *Truth) Explain(effect [2]int) {
	t.Explained[0] += effect[0]
	t.Explained[1] += effect[1]
}

// Divergence is a rally after which the truth and zaehlwerk disagree.
type Divergence struct {
	// Rally is the press after which they disagree, numbered as the game
	// numbers its presses.
	Rally uint64 `json:"rally"`
	// What disagrees: "points", "serve" or "ends", or a fault whose answer
	// was not what zaehlwerk must answer — "resend", "wrong_phase",
	// "counter_reset", "undo".
	What      string `json:"what"`
	Truth     string `json:"truth"`
	Zaehlwerk string `json:"zaehlwerk"`
	// Explained is what zaehlwerk should show after the injected faults, when
	// that is not the truth.
	Explained string `json:"explained,omitempty"`
	// Expected is a divergence the injected faults explain. Every other one
	// is a finding. For points that is decided by what changed at this
	// press, so a finding is the press where something unexplained began,
	// not every press after it.
	Expected bool `json:"expected"`
}

// Points compares the rallies won with the points zaehlwerk has awarded. It
// reports a divergence when they differ, or when the faults said they should
// and they do not.
func (t Truth) Points(rally uint64, st scorer.State) (Divergence, bool) {
	awarded := Awarded(st)
	explained := [2]int{t.Won[0] + t.Explained[0], t.Won[1] + t.Explained[1]}
	if awarded == t.Won && explained == t.Won {
		return Divergence{}, false
	}
	d := Divergence{Rally: rally, What: "points",
		Truth: score(t.Won), Zaehlwerk: score(awarded), Expected: awarded == explained}
	if explained != t.Won {
		d.Explained = score(explained)
	}
	return d, true
}

// Serve compares who won the serve rally with who zaehlwerk says serves first.
func (t Truth) Serve(rally uint64, st scorer.State) (Divergence, bool) {
	explained := t.ExplainedServe
	if explained == "" {
		explained = t.ServeRally
	}
	if st.Serving == t.ServeRally && explained == t.ServeRally {
		return Divergence{}, false
	}
	d := Divergence{Rally: rally, What: "serve",
		Truth: string(t.ServeRally), Zaehlwerk: string(st.Serving), Expected: st.Serving == explained}
	if explained != t.ServeRally {
		d.Explained = string(explained)
	}
	return d, true
}

// Ends compares where the bots stand with where zaehlwerk says they stand.
// They walk only when zaehlwerk calls a change of ends, so ends that move
// without one would put the next point on the wrong player. No fault explains
// that.
func Ends(rally uint64, standing [2]scorer.Player, st scorer.State) (Divergence, bool) {
	if st.Ends == standing {
		return Divergence{}, false
	}
	return Divergence{Rally: rally, What: "ends",
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

// other is the opposite half.
func other(h scorer.Half) scorer.Half {
	if h == scorer.HalfLeft {
		return scorer.HalfRight
	}
	return scorer.HalfLeft
}

// effectAt is one point more for the player standing at a half.
func effectAt(h scorer.Half, standing [2]scorer.Player) [2]int {
	var e [2]int
	e[index(standing[halfIndex(h)])] = 1
	return e
}

func halfAt(i int) scorer.Half {
	if i == 1 {
		return scorer.HalfRight
	}
	return scorer.HalfLeft
}

func halfIndex(h scorer.Half) int {
	if h == scorer.HalfRight {
		return 1
	}
	return 0
}

// halfOf is the half a player stands at.
func halfOf(p scorer.Player, standing [2]scorer.Player) scorer.Half {
	if standing[1] == p {
		return scorer.HalfRight
	}
	return scorer.HalfLeft
}
