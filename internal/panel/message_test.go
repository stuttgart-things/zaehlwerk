package panel

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// maxTitleRunes is what fits on the panel: the catcher draws `{{ title }}` at
// x=2 with a 6x10 font on a 64x64 matrix, so 62px / 6px per glyph. Text past
// that is not wrapped or ellipsised, it is simply off the edge of the panel.
const maxTitleRunes = 10

func state(points, sets [2]int) scorer.State {
	return scorer.State{
		MatchID:   "m1",
		Players:   [2]string{"Anna", "Bernd"},
		Points:    points,
		Sets:      sets,
		SetNumber: sets[0] + sets[1] + 1,
	}
}

func TestTitleIsWhatThePanelShows(t *testing.T) {
	tests := []struct {
		name string
		tr   scorer.Transition
		want string
	}{
		{
			name: "a point is the score of the set in progress",
			tr:   scorer.Transition{Kind: scorer.TransitionPoint, State: state([2]int{3, 5}, [2]int{0, 0})},
			want: "3:5",
		},
		{
			name: "a taken-back point reads like any other score",
			tr:   scorer.Transition{Kind: scorer.TransitionUndo, State: state([2]int{3, 4}, [2]int{0, 0})},
			want: "3:4",
		},
		{
			name: "a won set shows sets, prefixed so it is not read as points",
			tr:   scorer.Transition{Kind: scorer.TransitionSetWon, State: state([2]int{11, 7}, [2]int{1, 0})},
			want: "SET 1:0",
		},
		{
			name: "a won match shows the final set score",
			tr:   scorer.Transition{Kind: scorer.TransitionMatchWon, State: state([2]int{11, 9}, [2]int{3, 1})},
			want: "WIN 3:1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Title(tt.tr))
		})
	}
}

// A set score of 2:1 and a point score of 2:1 are the same three characters.
// If both rendered the same, the panel would be ambiguous exactly at the moment
// it matters most.
func TestASetScoreIsNotMistakableForAPointScore(t *testing.T) {
	points := Title(scorer.Transition{Kind: scorer.TransitionPoint, State: state([2]int{2, 1}, [2]int{0, 0})})
	sets := Title(scorer.Transition{Kind: scorer.TransitionSetWon, State: state([2]int{11, 4}, [2]int{2, 1})})

	require.NotEqual(t, points, sets)
}

// The catcher does not shorten a title that does not fit, so a score that can
// occur in a real match must fit by construction. Deuce has no upper bound in
// the laws, but two digits per side covers anything that has ever been played.
func TestEveryReachableTitleFitsOnThePanel(t *testing.T) {
	kinds := []scorer.TransitionKind{
		scorer.TransitionPoint, scorer.TransitionUndo,
		scorer.TransitionSetWon, scorer.TransitionMatchWon,
	}

	checked := 0
	for _, kind := range kinds {
		for a := range 100 {
			for b := range 100 {
				// Sets are bounded by best-of-7, the longest format in the laws.
				sets := [2]int{min(a, 4), min(b, 4)}
				title := Title(scorer.Transition{Kind: kind, State: state([2]int{a, b}, sets)})

				require.LessOrEqual(t, len([]rune(title)), maxTitleRunes,
					"kind=%s points=%d:%d sets=%v renders as %q", kind, a, b, sets, title)
				checked++
			}
		}
	}
	require.Equal(t, 4*100*100, checked)
}

func TestSeverityMarksTheMomentsThatMatter(t *testing.T) {
	tests := []struct {
		kind scorer.TransitionKind
		want string
	}{
		{scorer.TransitionPoint, "INFO"},
		{scorer.TransitionUndo, "INFO"},
		{scorer.TransitionSetWon, "SUCCESS"},
		{scorer.TransitionMatchWon, "SUCCESS"},
	}

	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			require.Equal(t, tt.want, Severity(tt.kind))
		})
	}
}

// The catcher lowercases both sides before matching a rule, but its profile is
// written in upper case and so is the issue. Anything outside the set the
// profile knows about would match no rule and never be displayed.
func TestSeverityIsOneTheCatcherProfileMatches(t *testing.T) {
	known := map[string]bool{"INFO": true, "SUCCESS": true}

	for _, kind := range []scorer.TransitionKind{
		scorer.TransitionPoint, scorer.TransitionUndo,
		scorer.TransitionSetWon, scorer.TransitionMatchWon,
		scorer.TransitionKind("something-added-later"),
	} {
		require.True(t, known[Severity(kind)], "severity %q for kind %q is not in the profile", Severity(kind), kind)
	}
}

func TestSummaryNamesBothPlayers(t *testing.T) {
	tests := []struct {
		name string
		tr   scorer.Transition
		want string
	}{
		{
			name: "point",
			tr:   scorer.Transition{Kind: scorer.TransitionPoint, State: state([2]int{3, 5}, [2]int{0, 0})},
			want: "Anna 3 : 5 Bernd",
		},
		{
			name: "undo says so, because the score going down needs explaining",
			tr:   scorer.Transition{Kind: scorer.TransitionUndo, State: state([2]int{3, 4}, [2]int{0, 0})},
			want: "Anna 3 : 4 Bernd — corrected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Summary(tt.tr))
		})
	}
}

func TestSummaryOfAWonSetCountsTheSetsPlayed(t *testing.T) {
	st := state([2]int{11, 7}, [2]int{1, 0})
	st.CompletedSets = [][2]int{{11, 7}}

	require.Equal(t,
		"Anna 11 : 7 Bernd — set 1, sets 1:0",
		Summary(scorer.Transition{Kind: scorer.TransitionSetWon, State: st}))
}

func TestSummaryOfAWonMatchNamesTheWinner(t *testing.T) {
	for _, tt := range []struct {
		winner scorer.Player
		want   string
	}{
		{scorer.PlayerA, "Anna 3 : 1 Bernd — Anna wins"},
		{scorer.PlayerB, "Anna 1 : 3 Bernd — Bernd wins"},
	} {
		t.Run(string(tt.winner), func(t *testing.T) {
			sets := [2]int{3, 1}
			if tt.winner == scorer.PlayerB {
				sets = [2]int{1, 3}
			}
			st := state([2]int{11, 9}, sets)
			st.Complete, st.Winner = true, tt.winner

			require.Equal(t, tt.want, Summary(scorer.Transition{Kind: scorer.TransitionMatchWon, State: st}))
		})
	}
}

func TestMessageCarriesTheFieldsTheCatcherRoutesOn(t *testing.T) {
	st := state([2]int{3, 5}, [2]int{1, 0})
	at := time.Date(2026, 9, 5, 18, 30, 0, 0, time.UTC)

	msg := Message(scorer.Transition{Kind: scorer.TransitionPoint, State: st}, "tabletennis", "zaehlwerk", at)

	require.Equal(t, "tabletennis", msg.System)
	require.Equal(t, "INFO", msg.Severity)
	require.Equal(t, "3:5", msg.Title)
	require.Equal(t, "Anna 3 : 5 Bernd", msg.Message)
	require.Equal(t, "zaehlwerk", msg.Author)
	require.Equal(t, "match=m1,set=2,transition=point", msg.Tags)
	require.Equal(t, "2026-09-05T18:30:00Z", msg.Timestamp)
}

func TestTagsSayWhatHappenedAndWhichSideItWentTo(t *testing.T) {
	tests := []struct {
		name string
		tr   scorer.Transition
		want string
	}{
		{
			name: "a point",
			tr:   scorer.Transition{Kind: scorer.TransitionPoint, Side: scorer.PlayerA, State: state([2]int{3, 5}, [2]int{1, 0})},
			want: "match=m1,set=2,transition=point,side=a",
		},
		{
			name: "a won set",
			tr:   scorer.Transition{Kind: scorer.TransitionSetWon, Side: scorer.PlayerB, State: state([2]int{0, 0}, [2]int{0, 1})},
			want: "match=m1,set=2,transition=set_won,side=b",
		},
		{
			name: "a won match",
			tr:   scorer.Transition{Kind: scorer.TransitionMatchWon, Side: scorer.PlayerA, State: state([2]int{11, 9}, [2]int{2, 0})},
			want: "match=m1,set=3,transition=match_won,side=a",
		},
		{
			name: "an undo went to nobody",
			tr:   scorer.Transition{Kind: scorer.TransitionUndo, State: state([2]int{3, 4}, [2]int{1, 0})},
			want: "match=m1,set=2,transition=undo",
		},
		{
			name: "a correction that lowered a score went to nobody",
			tr:   scorer.Transition{Kind: scorer.TransitionPoint, State: state([2]int{3, 1}, [2]int{1, 0})},
			want: "match=m1,set=2,transition=point",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Message(tt.tr, "tabletennis", "zaehlwerk", time.Now()).Tags)
		})
	}
}

// A `tags_contain` rule written before transition and side existed still has to
// match, so the tags it was written against keep their place at the front.
func TestTheNewTagsDoNotDisturbTheOnesRulesAlreadyMatch(t *testing.T) {
	for _, kind := range []scorer.TransitionKind{
		scorer.TransitionPoint, scorer.TransitionUndo,
		scorer.TransitionSetWon, scorer.TransitionMatchWon,
	} {
		for _, side := range []scorer.Player{"", scorer.PlayerA, scorer.PlayerB} {
			tags := Tags(scorer.Transition{Kind: kind, Side: side, State: state([2]int{0, 0}, [2]int{1, 0})})

			require.True(t, strings.HasPrefix(tags, "match=m1,set=2,"), "kind=%s side=%q: %q", kind, side, tags)
			require.NotContains(t, strings.Split(tags, ","), "side=", "an empty side is left out, not sent blank")
		}
	}
}

// The tags come from a real scorer here, not from a hand-built transition: the
// side of a set won by a correction is the whole point, and it is decided in
// Apply.
func TestASetWonByACorrectionIsTaggedWithTheSideThatTookIt(t *testing.T) {
	sc, err := scorer.New(scorer.Config{MatchID: "m1", Players: [2]string{"Anna", "Bernd"}, BestOf: 5})
	require.NoError(t, err)

	var tags []string
	sc.Observe(func(tr scorer.Transition) {
		tags = append(tags, Message(tr, DefaultSystem, DefaultAuthor, time.Now()).Tags)
	})

	id := uint64(0)
	send := func(p scorer.Player, delta int) {
		t.Helper()
		id++
		_, err := sc.Apply(scorer.ScoreEvent{MatchID: "m1", Player: p, Delta: delta, EventID: id, Source: "test"})
		require.NoError(t, err)
	}

	// 10:11, then a point is taken off Anna: Bernd is two clear at eleven.
	for range 10 {
		send(scorer.PlayerA, 1)
		send(scorer.PlayerB, 1)
	}
	send(scorer.PlayerB, 1)
	send(scorer.PlayerA, -1)

	require.Equal(t, "match=m1,set=1,transition=point,side=b", tags[len(tags)-2])
	require.Equal(t, "match=m1,set=2,transition=set_won,side=b", tags[len(tags)-1])

	_, err = sc.Undo()
	require.NoError(t, err)
	require.Equal(t, "match=m1,set=1,transition=undo", tags[len(tags)-1])
}

// Every field the catcher reads must be non-empty: homerun marshals with
// omitempty, so a field left blank here is a field that is missing from the
// JSON document the catcher resolves, not one that arrives empty.
func TestMessageLeavesNoRoutingFieldEmpty(t *testing.T) {
	for _, kind := range []scorer.TransitionKind{
		scorer.TransitionPoint, scorer.TransitionUndo,
		scorer.TransitionSetWon, scorer.TransitionMatchWon,
	} {
		t.Run(string(kind), func(t *testing.T) {
			msg := Message(
				scorer.Transition{Kind: kind, State: state([2]int{0, 0}, [2]int{0, 0})},
				"tabletennis", "zaehlwerk", time.Now(),
			)

			require.NotEmpty(t, msg.System)
			require.NotEmpty(t, msg.Severity)
			require.NotEmpty(t, msg.Title)
			require.NotEmpty(t, msg.Message)
			require.NotEmpty(t, msg.Author)
			require.NotEmpty(t, msg.Tags)
			require.NotEmpty(t, msg.Timestamp)
			require.True(t, strings.HasPrefix(msg.Tags, "match="), "tags: %q", msg.Tags)
		})
	}
}
