// Package face decides the faces the two players' figures pull at a moment of
// the match (ADR-0018).
//
// Schmetterpause lets every player assemble a figure — fur, headdress, handle,
// blade, bling — but not its face: the face belongs to the game, and this
// service is the one that knows the game (Schmetterpause ADR-0029). It sends a
// face per side on GET /live/stream, and its own screen shows the same ones.
//
// Rules only, like the scorer, and outside it for the same reason: a face is
// not a rule of table tennis, and the scorer stays the rules and nothing else
// (invariant 5). The one thing it remembers between moments, how many points
// in a row a side has lost, is handed in and handed back by the caller rather
// than kept here.
package face

import "github.com/stuttgart-things/zaehlwerk/internal/scorer"

// Face is one of the expressions a Schmetterpause figure carries. The keys are
// exactly Schmetterpause's avatar.Faces; a key it does not know shows neutral
// there, so a new one here is harmless but useless until it exists there.
type Face string

const (
	Neutral   Face = "neutral"
	Happy     Face = "happy"
	Surprised Face = "surprised"
	Skeptical Face = "skeptical"
	Angry     Face = "angry"
	Sad       Face = "sad"
)

// Faces is one face per side, in the order of scorer.State.Players: a, then b.
type Faces [2]Face

// Streak counts the points each side has lost in a row, in the current set.
type Streak [2]int

// angryAfter is how many points in a row a side loses before its face turns
// from skeptical to angry.
const angryAfter = 3

// Moment is what the faces are read from: one event of the match.
type Moment struct {
	// Kind is the transition, or a kind of the live stream that is not one —
	// a snapshot, a match ended early. Anything that is not a point, a set or
	// a match won, or an undo is read as the match at rest.
	Kind scorer.TransitionKind
	// Side is who the transition went to, from scorer.Transition. Empty for
	// a correction that lowered a score, which is read like an undo.
	Side  scorer.Player
	State scorer.State
	// Stake is who the next point would give the set or the match, from
	// scorer.Scorer.Stake at State. Empty when nobody holds one.
	Stake scorer.Player
	// PointsPerSet is the match's rule, for telling deuce.
	PointsPerSet int
}

// Read is the faces at m, and the streak after it.
//
// A face is meant briefly: it stays until the next event, which at a table is
// the next rally. A won set or match is the exception that stays, because
// nothing follows it until the next set begins or the match is gone.
func Read(m Moment, streak Streak) (Faces, Streak) {
	st := m.State
	switch m.Kind {
	case scorer.TransitionMatchWon:
		return won(st.Winner, Happy, Skeptical), Streak{}

	case scorer.TransitionSetWon:
		// Sad, briefly. The Schmetterpause briefing rules out a sad face
		// after a lost match; after a lost set, with the match still open,
		// it is the moment the table actually has.
		return won(m.Side, Happy, Sad), Streak{}

	case scorer.TransitionPoint:
		if index(m.Side) < 0 {
			return rest(m), Streak{}
		}
		w := index(m.Side)
		l := 1 - w
		streak[w] = 0
		streak[l]++

		var f Faces
		f[w], f[l] = Happy, Skeptical
		if streak[l] >= angryAfter {
			f[l] = Angry
		}
		return tense(f, m), streak

	case scorer.TransitionUndo:
		// The point is gone, and with it whatever it set off. A streak that
		// was only counted up by it would be wrong by one, and telling which
		// is not worth a field: an undo is rare and the next point counts
		// afresh.
		return rest(m), Streak{}
	}
	return rest(m), streak
}

// rest is the faces of a match nobody has just scored in: neutral, except
// where the score itself says more — a decided match, a set or match point,
// deuce.
func rest(m Moment) Faces {
	if m.State.Complete {
		return won(m.State.Winner, Happy, Skeptical)
	}
	return tense(Faces{Neutral, Neutral}, m)
}

// tense marks the moments everybody holds their breath. At deuce both sides
// are surprised; at a set or match point the side facing it is, while the
// side holding it keeps the face the point gave it. Angry stays angry: three
// points lost in a row and a set point against is not surprise.
func tense(f Faces, m Moment) Faces {
	if m.State.Complete || m.State.Phase != scorer.PhasePlaying {
		return f
	}
	surprise := func(i int) {
		if f[i] != Angry {
			f[i] = Surprised
		}
	}
	switch {
	case deuce(m.State, m.PointsPerSet):
		surprise(0)
		surprise(1)
	case index(m.Stake) >= 0:
		surprise(1 - index(m.Stake))
	}
	return f
}

// deuce is a level score at the edge of the set, where nobody can win it with
// the next point.
func deuce(st scorer.State, pointsPerSet int) bool {
	p := st.Points
	return pointsPerSet > 0 && p[0] == p[1] && p[0] >= pointsPerSet-1
}

// won is the faces after p took something: win on p's side, lose on the other.
// Neutral both when p is nobody, which a state that says it is complete never
// is, but a stream can hand over anything.
func won(p scorer.Player, win, lose Face) Faces {
	i := index(p)
	if i < 0 {
		return Faces{Neutral, Neutral}
	}
	var f Faces
	f[i], f[1-i] = win, lose
	return f
}

func index(p scorer.Player) int {
	switch p {
	case scorer.PlayerA:
		return 0
	case scorer.PlayerB:
		return 1
	}
	return -1
}
