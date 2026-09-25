package ui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
)

// stubRoster is the player list Schmetterpause would return.
type stubRoster struct {
	players []schmetterpause.Player
	err     error
	calls   int
	// observers are who /api/operators adds to the players; opErr fails that
	// list alone.
	observers []schmetterpause.Operator
	opErr     error
}

func (s *stubRoster) Players(context.Context) ([]schmetterpause.Player, error) {
	s.calls++
	return s.players, s.err
}

func (s *stubRoster) Operators(context.Context) ([]schmetterpause.Operator, error) {
	if s.opErr != nil {
		return nil, s.opErr
	}
	out := make([]schmetterpause.Operator, 0, len(s.players)+len(s.observers))
	for _, p := range s.players {
		out = append(out, schmetterpause.Operator{ID: p.ID, DisplayName: p.DisplayName})
	}
	return append(out, s.observers...), nil
}

type stubHandover struct {
	err   error
	calls int
}

func (s *stubHandover) Send(context.Context, *match.Match) error {
	s.calls++
	return s.err
}

func roster() *stubRoster {
	return &stubRoster{players: []schmetterpause.Player{
		{ID: "id-anna", DisplayName: "Anna", TTR: 1000},
		{ID: "id-bernd", DisplayName: "Bernd", TTR: 990},
		{ID: "id-cem", DisplayName: "Cem", TTR: 1010},
	}}
}

// TestWithoutTheCouplingTheFormAsksForNames is ADR-0004's first-class case: a
// run at the table needs none of the outbound couplings, and turning this one
// off must change nothing else.
func TestWithoutTheCouplingTheFormAsksForNames(t *testing.T) {
	f := newFixture(t)

	_, page := f.do(http.MethodGet, "/ui", nil)

	require.Contains(t, page, `name="player_a"`)
	require.NotContains(t, page, `name="home_id"`)
	require.NotContains(t, page, `name="operator_id"`)
}

func TestWithTheCouplingTheFormNamesThreePeople(t *testing.T) {
	r := roster()
	f := newFixture(t, WithSchmetterpause(r, &stubHandover{}))

	_, page := f.do(http.MethodGet, "/ui", nil)

	require.Contains(t, page, `name="home_id"`)
	require.Contains(t, page, `name="away_id"`)
	require.Contains(t, page, `name="operator_id"`)
	require.Contains(t, page, `value="id-anna"`)
	require.Contains(t, page, "Cem")
	// The free-text fields are gone, not merely hidden: a name typed beside a
	// chosen id is a second spelling of the same person.
	require.NotContains(t, page, `name="player_a"`)
	require.Positive(t, r.calls, "the list must be fetched while the page renders")
}

// TestTheListIsFetchedEveryRender pins "read, never mirrored" from ADR-0004:
// this service holds no copy, so a player who just joined is offered and one
// who was merged away is not.
func TestTheListIsFetchedEveryRender(t *testing.T) {
	r := roster()
	f := newFixture(t, WithSchmetterpause(r, &stubHandover{}))

	f.do(http.MethodGet, "/ui", nil)
	f.do(http.MethodGet, "/ui", nil)
	f.do(http.MethodGet, "/ui", nil)

	require.Equal(t, 3, r.calls, "no caching: three renders, three fetches")
}

// TestAFailingListSaysSoRatherThanFallingBackQuietly: dropping to free text
// would start a match that cannot be reported, and nobody would find out
// until the last point.
func TestAFailingListSaysSoRatherThanFallingBackQuietly(t *testing.T) {
	r := roster()
	r.err = errors.New("connection refused")
	r.players = nil
	f := newFixture(t, WithSchmetterpause(r, &stubHandover{}))

	_, page := f.do(http.MethodGet, "/ui", nil)

	require.Contains(t, page, "cannot be reported")
	require.Contains(t, page, "connection refused")
}

func TestTheChosenPlayersReachTheMatch(t *testing.T) {
	f := newFixture(t, WithSchmetterpause(roster(), &stubHandover{}))

	_, body := f.do(http.MethodPost, "/ui/matches", url.Values{
		"home_id": {"id-anna"}, "away_id": {"id-bernd"}, "operator_id": {"id-cem"},
	})

	require.Equal(t, match.Handover{
		HomeID: "id-anna", AwayID: "id-bernd", OperatorID: "id-cem",
	}, f.life.handover)
	// The panel shows the names Schmetterpause holds, not a third spelling.
	require.Contains(t, body, "Anna")
	require.Contains(t, body, "Bernd")
}

// TestTheFormRefusesWhatSchmetterpauseWouldRefuse catches it at the start of
// the match rather than twenty minutes later at the post.
func TestTheFormRefusesWhatSchmetterpauseWouldRefuse(t *testing.T) {
	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{
			"operator is playing",
			url.Values{"home_id": {"id-anna"}, "away_id": {"id-bernd"}, "operator_id": {"id-anna"}},
			"may not be playing",
		},
		{
			"same player twice",
			url.Values{"home_id": {"id-anna"}, "away_id": {"id-anna"}, "operator_id": {"id-cem"}},
			"two different players",
		},
		{
			"only some named",
			url.Values{"home_id": {"id-anna"}, "away_id": {"id-bernd"}},
			"or none of the three",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, WithSchmetterpause(roster(), &stubHandover{}))
			_, body := f.do(http.MethodPost, "/ui/matches", tc.form)
			require.Contains(t, body, tc.want)
			require.Empty(t, f.life.started, "no match may start")
		})
	}
}

// TestAMatchWithNoNamesStillStarts: naming nobody is allowed even with the
// coupling on, and is how somebody plays a quick one without touching the
// ranking.
func TestAMatchWithNoNamesStillStarts(t *testing.T) {
	f := newFixture(t, WithSchmetterpause(roster(), &stubHandover{}))

	f.do(http.MethodPost, "/ui/matches", url.Values{})

	require.Len(t, f.life.started, 1)
	require.False(t, f.life.handover.Wanted())
}

func TestTheRetryButtonSendsAgain(t *testing.T) {
	h := &stubHandover{}
	f := newFixture(t, WithSchmetterpause(roster(), h))

	rec, _ := f.do(http.MethodPost, "/ui/matches", url.Values{
		"home_id": {"id-anna"}, "away_id": {"id-bernd"}, "operator_id": {"id-cem"},
	})
	require.Equal(t, http.StatusOK, rec.Code)
	m, err := f.registry.Current()
	require.NoError(t, err)

	rec, _ = f.do(http.MethodPost, "/ui/matches/"+m.ID+"/report", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, h.calls)
}

// operatorPicker is the scorekeeper select of the page, and sides the two
// player selects, so a test can say which list somebody appears in.
func operatorPicker(t *testing.T, page string) (picker, sides string) {
	t.Helper()
	start := strings.Index(page, `name="operator_id"`)
	require.Positive(t, start, "the page has no scorekeeper picker")
	end := strings.Index(page[start:], "</select>")
	require.Positive(t, end)
	return page[start : start+end], page[:start]
}

// TestAnObserverIsOfferedToKeepScoreAndNeverToPlay is Schmetterpause's
// ADR-0023: an observer never plays, and keeping score is exactly what they
// are for.
func TestAnObserverIsOfferedToKeepScoreAndNeverToPlay(t *testing.T) {
	r := roster()
	r.observers = []schmetterpause.Operator{{ID: "id-timo", DisplayName: "timoboll", Observer: true}}
	f := newFixture(t, WithSchmetterpause(r, &stubHandover{}))

	_, page := f.do(http.MethodGet, "/ui", nil)
	picker, sides := operatorPicker(t, page)

	require.Contains(t, picker, `value="id-timo"`)
	require.Contains(t, picker, "timoboll (observer)")
	require.Contains(t, picker, `value="id-anna"`, "a player who is not playing may still keep score")
	require.Less(t, strings.Index(picker, "id-timo"), strings.Index(picker, "id-anna"),
		"observers come first: keeping score is what they are for")
	require.NotContains(t, sides, "id-timo", "an observer is never offered as a side")
}

// TestAnObserverChosenToKeepScoreReachesTheMatch: handoverFrom takes the id
// from the form whether or not it is in the player list.
func TestAnObserverChosenToKeepScoreReachesTheMatch(t *testing.T) {
	r := roster()
	r.observers = []schmetterpause.Operator{{ID: "id-timo", DisplayName: "timoboll", Observer: true}}
	f := newFixture(t, WithSchmetterpause(r, &stubHandover{}))

	_, body := f.do(http.MethodPost, "/ui/matches", url.Values{
		"home_id": {"id-anna"}, "away_id": {"id-bernd"}, "operator_id": {"id-timo"},
	})

	require.Equal(t, match.Handover{
		HomeID: "id-anna", AwayID: "id-bernd", OperatorID: "id-timo",
	}, f.life.handover)
	require.Contains(t, body, "Anna")
}

// TestAFailingOperatorListOffersThePlayers: the players are still somebody who
// may keep score, so one failed list must not stop a match from starting.
func TestAFailingOperatorListOffersThePlayers(t *testing.T) {
	r := roster()
	r.opErr = errors.New("connection reset")
	f := newFixture(t, WithSchmetterpause(r, &stubHandover{}))

	_, page := f.do(http.MethodGet, "/ui", nil)
	picker, _ := operatorPicker(t, page)

	require.Contains(t, picker, `value="id-anna"`)
	require.Contains(t, picker, `value="id-cem"`)
	require.NotContains(t, page, "cannot be reported")
}
