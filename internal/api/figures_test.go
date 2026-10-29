package api

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/figure"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

type players []schmetterpause.Player

func (p players) Players(context.Context) ([]schmetterpause.Player, error) { return p, nil }

func namedClient(t *testing.T) *client {
	t.Helper()
	c := newClient(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	c.server = New(c.registry, WithLogger(quiet), WithFigureNames(figure.NewNames(players{
		{ID: "id-anna", DisplayName: "Anna"}, {ID: "id-bernd", DisplayName: "Bernd"},
	}, quiet)))
	return c
}

// A match by names, as a game of Pong's or one started on the page: the
// players Schmetterpause has under them show their own figures, and the
// match is still not reported (ADR-0008).
func TestAMatchByNamesShowsThePlayersFigures(t *testing.T) {
	c := namedClient(t)

	m, err := c.registry.Get(c.newMatch(`{"players":["Anna","Gast"],"demo":true}`))
	require.NoError(t, err)

	require.Equal(t, [2]string{"id-anna", ""}, m.Figures)
	require.False(t, m.Handover.Wanted(), "a drawing, not a report")
}

func TestAMatchWithIDsShowsThoseAndLooksNothingUp(t *testing.T) {
	c := namedClient(t)

	m, err := c.server.create(scorer.Config{BestOf: 3, PointsPerSet: 11, Players: [2]string{"Anna", "Bernd"}},
		match.Setup{Handover: match.Handover{HomeID: "id-x", AwayID: "id-y", OperatorID: "o"}})
	require.NoError(t, err)

	require.Equal(t, [2]string{"id-x", "id-y"}, m.Figures)
}
