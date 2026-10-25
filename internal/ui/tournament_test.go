package ui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
)

// drawRoster is a Schmetterpause that knows tournaments (its ADR-0028).
type drawRoster struct {
	*stubRoster
	tournaments []schmetterpause.Tournament
	places      map[string][]schmetterpause.Place
	err         error
}

func (d *drawRoster) Tournaments(context.Context) ([]schmetterpause.Tournament, error) {
	return d.tournaments, d.err
}

func (d *drawRoster) Pairings(_ context.Context, id string) ([]schmetterpause.Place, error) {
	return d.places[id], nil
}

func draws() *drawRoster {
	return &drawRoster{
		stubRoster: roster(),
		tournaments: []schmetterpause.Tournament{
			{ID: "t-friday", Name: "Freitag", BestOf: 1, PointsToWin: 11, CreatedAt: time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)},
			{ID: "t-monday", Name: "Montag", BestOf: 3, PointsToWin: 21, CreatedAt: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)},
		},
		places: map[string][]schmetterpause.Place{
			"t-monday": {
				{Round: 2, HomeID: "id-bernd", AwayID: "id-anna", HomeName: "Bernd", AwayName: "Anna"},
				{Round: 4, Final: true, HomeID: "id-anna", AwayID: "id-cem", HomeName: "Anna", AwayName: "Cem"},
			},
			// No place left: not offered at all.
			"t-friday": nil,
		},
	}
}

func TestTheScoringPageOffersTheOpenPlacesOfEachTournament(t *testing.T) {
	f := newFixture(t, WithSchmetterpause(draws(), &stubHandover{}))

	_, page := f.do(http.MethodGet, "/ui", nil)

	require.Contains(t, page, "Tournament match")
	require.Contains(t, page, `name="place"`)
	require.Contains(t, page, `label="Montag · best of 3 to 21"`)
	require.Contains(t, page, `value="t-monday|2|id-bernd|id-anna"`)
	require.Contains(t, page, "Round 2: Bernd – Anna")
	require.Contains(t, page, "Final: Anna – Cem")
	require.NotContains(t, page, "Freitag", "a tournament with nothing open is not offered")
	// The ordinary form is still there beside it.
	require.Contains(t, page, `name="home_id"`)
}

func TestWithoutTournamentsThereIsNoTournamentCard(t *testing.T) {
	for name, r := range map[string]Roster{
		"a roster that knows no draws":     roster(),
		"a schmetterpause before ADR-0028": &drawRoster{stubRoster: roster(), err: schmetterpause.ErrNoTournaments},
		"no place open anywhere":           &drawRoster{stubRoster: roster(), tournaments: draws().tournaments},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, WithSchmetterpause(r, &stubHandover{}))
			_, page := f.do(http.MethodGet, "/ui", nil)
			require.NotContains(t, page, "Tournament match")
			require.NotContains(t, page, "did not all load")
		})
	}
}

// The ordinary form still works when the tournaments do not load, so the
// failure is said in the tournament card and nowhere else.
func TestAFailingTournamentListIsSaidInItsOwnCard(t *testing.T) {
	r := draws()
	r.err = errors.New("connection refused")
	f := newFixture(t, WithSchmetterpause(r, &stubHandover{}))

	_, page := f.do(http.MethodGet, "/ui", nil)

	require.Contains(t, page, "The tournaments did not all load: connection refused")
	require.Contains(t, page, `name="home_id"`)
}

func TestAPlaceBringsItsPlayersSidesAndTheTournamentsMode(t *testing.T) {
	f := newFixture(t, WithSchmetterpause(draws(), &stubHandover{}))

	_, body := f.do(http.MethodPost, "/ui/matches", url.Values{
		"place": {"t-monday|2|id-bernd|id-anna"}, "operator_id": {"id-cem"},
		// What the shared options send, and a best_of the tournament overrides.
		"best_of": {"5"}, "first_server": {"a"},
	})

	require.Equal(t, match.Handover{
		HomeID: "id-bernd", AwayID: "id-anna", OperatorID: "id-cem",
		TournamentID: "t-monday", Round: 2,
	}, f.life.handover)

	m, err := f.registry.Get(f.life.started[0])
	require.NoError(t, err)
	bestOf, points := m.Scorer.Rules()
	require.Equal(t, 3, bestOf, "the tournament's mode, not the form's")
	require.Equal(t, 21, points)
	require.Equal(t, [2]string{"Bernd", "Anna"}, m.Scorer.State().Players, "home on the left")
	require.Contains(t, body, "Bernd")
}

func TestTheFinalsPlaceIsMarkedAsTheFinal(t *testing.T) {
	f := newFixture(t, WithSchmetterpause(draws(), &stubHandover{}))

	f.do(http.MethodPost, "/ui/matches", url.Values{
		"place": {"t-monday|4|id-anna|id-cem"}, "operator_id": {"id-bernd"},
	})

	require.True(t, f.life.handover.Final, "the panel tags the final")
	require.Equal(t, 4, f.life.handover.Round)
}

func TestAPlaceFilledSinceThePageRenderedStartsNothing(t *testing.T) {
	r := draws()
	f := newFixture(t, WithSchmetterpause(r, &stubHandover{}))
	// Entered at the kiosk meanwhile.
	r.places["t-monday"] = r.places["t-monday"][1:]

	_, body := f.do(http.MethodPost, "/ui/matches", url.Values{
		"place": {"t-monday|2|id-bernd|id-anna"}, "operator_id": {"id-cem"},
	})

	require.Contains(t, body, "no longer open")
	require.Empty(t, f.life.started)
}

func TestATournamentMatchStillNeedsSomebodyToKeepScoreWhoIsNotPlaying(t *testing.T) {
	for operator, want := range map[string]string{
		"":         "name whoever is keeping score",
		"id-bernd": "whoever keeps score may not be playing",
	} {
		f := newFixture(t, WithSchmetterpause(draws(), &stubHandover{}))
		_, body := f.do(http.MethodPost, "/ui/matches", url.Values{
			"place": {"t-monday|2|id-bernd|id-anna"}, "operator_id": {operator},
		})
		require.Contains(t, body, want)
		require.Empty(t, f.life.started)
	}
}

func TestAMalformedPlaceIsRefused(t *testing.T) {
	f := newFixture(t, WithSchmetterpause(draws(), &stubHandover{}))

	for _, place := range []string{"", "t-monday|2", "t-monday|x|id-bernd|id-anna", "t-monday|0|id-bernd|id-anna"} {
		_, body := f.do(http.MethodPost, "/ui/matches", url.Values{"place": {place}, "operator_id": {"id-cem"}})
		require.Contains(t, body, "pick a place of the draw", "place %q", place)
	}
	require.Empty(t, f.life.started)
}
