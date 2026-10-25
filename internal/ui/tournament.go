package ui

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// A tournament match from the scoring page (ADR-0017, "The doors"): pick an
// open place of a Schmetterpause draw, and the two players, their sides and the
// mode come with it. The result is reported with the place, and Schmetterpause
// checks it there, as it checks Pong's.
//
// Both lists are read from Schmetterpause while the page renders and again when
// the form is posted, never kept (ADR-0004): a place can be filled at the kiosk
// in between, and the final exists only once the table names it.

// Tournaments is the half of the Schmetterpause client that knows the draws.
// Asked of the Roster by type assertion, so a roster without it — and every
// Schmetterpause before its ADR-0028 — leaves the page as it was.
type Tournaments interface {
	Tournaments(ctx context.Context) ([]schmetterpause.Tournament, error)
	Pairings(ctx context.Context, tournamentID string) ([]schmetterpause.Place, error)
}

// tournamentView is one open tournament with its open places, as the picker
// groups them.
type tournamentView struct {
	Name   string
	Mode   string
	Places []placeView
}

// placeView is one option of the picker: the value names the place, the label
// says who plays and in which round.
type placeView struct {
	Value string
	Label string
}

// errPlaceGone is a place that was open when the page rendered and is not any
// more: filled at the kiosk or by another table, or a tournament closed.
var errPlaceGone = errors.New("that place is no longer open — the list is reloaded, pick again")

// fillTournaments offers the open places, when the roster can name them.
func (s *Server) fillTournaments(ctx context.Context, v *view) {
	draws, ok := s.roster.(Tournaments)
	if !ok || len(v.Roster) == 0 {
		return
	}

	tours, err := draws.Tournaments(ctx)
	if errors.Is(err, schmetterpause.ErrNoTournaments) {
		return
	}
	if err != nil {
		s.log.Warn("fetching the tournaments failed", "error", err)
		v.TournamentErr = err.Error()
		return
	}
	// Oldest first, as Pong plays them, whatever order they came in.
	slices.SortStableFunc(tours, func(a, b schmetterpause.Tournament) int { return a.CreatedAt.Compare(b.CreatedAt) })

	for _, t := range tours {
		places, err := draws.Pairings(ctx, t.ID)
		if err != nil {
			s.log.Warn("fetching a tournament's places failed", "tournament", t.Name, "error", err)
			v.TournamentErr = err.Error()
			continue
		}
		if len(places) == 0 {
			continue
		}
		tv := tournamentView{Name: t.Name, Mode: fmt.Sprintf("best of %d to %d", t.BestOf, t.PointsToWin)}
		for _, p := range places {
			tv.Places = append(tv.Places, placeView{Value: placeValue(t.ID, p), Label: placeLabel(p)})
		}
		v.Tournaments = append(v.Tournaments, tv)
	}
}

// placeValue names a place in the form: the tournament, the round and the pair,
// home first. The pair is in it so that a place re-read at the post is the one
// that was picked, not whatever now sits in that round.
func placeValue(tournamentID string, p schmetterpause.Place) string {
	return strings.Join([]string{tournamentID, strconv.Itoa(p.Round), p.HomeID, p.AwayID}, "|")
}

func placeLabel(p schmetterpause.Place) string {
	round := fmt.Sprintf("Round %d", p.Round)
	if p.Final {
		round = "Final"
	}
	return fmt.Sprintf("%s: %s – %s", round, p.HomeName, p.AwayName)
}

// tournamentMatch turns a posted place into the match to start: the config,
// with the tournament's mode and the players' names, and the handover with the
// place. The rest of the form — first serve, warm-up, ends — is taken from cfg.
func (s *Server) tournamentMatch(ctx context.Context, form url.Values, cfg scorer.Config) (
	scorer.Config, match.Handover, error,
) {
	draws, ok := s.roster.(Tournaments)
	if !ok || s.handover == nil {
		return cfg, match.Handover{}, errors.New("this instance does not report tournament matches to schmetterpause")
	}

	parts := strings.Split(strings.TrimSpace(form.Get("place")), "|")
	if len(parts) != 4 {
		return cfg, match.Handover{}, errors.New("pick a place of the draw")
	}
	tournamentID, home, away := parts[0], parts[2], parts[3]
	round, err := strconv.Atoi(parts[1])
	if err != nil || round < 1 {
		return cfg, match.Handover{}, errors.New("pick a place of the draw")
	}

	operator := strings.TrimSpace(form.Get("operator_id"))
	switch operator {
	case "":
		return cfg, match.Handover{}, errors.New("name whoever is keeping score")
	case home, away:
		return cfg, match.Handover{}, errors.New("whoever keeps score may not be playing")
	}

	// Read again: the place, its names and its mode are Schmetterpause's, as
	// they are now, not as the page saw them.
	tours, err := draws.Tournaments(ctx)
	if err != nil {
		return cfg, match.Handover{}, fmt.Errorf("reading the tournaments: %w", err)
	}
	i := slices.IndexFunc(tours, func(t schmetterpause.Tournament) bool { return t.ID == tournamentID })
	if i < 0 {
		return cfg, match.Handover{}, errPlaceGone
	}
	places, err := draws.Pairings(ctx, tournamentID)
	if err != nil {
		return cfg, match.Handover{}, fmt.Errorf("reading the places: %w", err)
	}
	j := slices.IndexFunc(places, func(p schmetterpause.Place) bool {
		return p.Round == round && p.HomeID == home && p.AwayID == away
	})
	if j < 0 {
		return cfg, match.Handover{}, errPlaceGone
	}
	place, tour := places[j], tours[i]

	// The tournament's mode, whatever the form said: Schmetterpause refuses a
	// result in any other, and the scorer has to count by the one it checks.
	cfg.BestOf, cfg.PointsPerSet = tour.BestOf, tour.PointsToWin
	cfg.Players = [2]string{place.HomeName, place.AwayName}
	return cfg, match.Handover{
		HomeID: place.HomeID, AwayID: place.AwayID, OperatorID: operator,
		TournamentID: tour.ID, Round: place.Round, Final: place.Final,
	}, nil
}
