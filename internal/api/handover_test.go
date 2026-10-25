package api

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
)

// matchesToken is MATCHES_TOKEN in these tests: what the sandbox's Pong sends
// when it starts a match that names players (ADR-0015).
const matchesToken = "matches-secret"

// pongOperatorID is the observer the sandbox's Pong names as the operator.
const pongOperatorID = "77777777-7777-4777-8777-777777777777"

// handoverClient is a server with the handover door on.
func handoverClient(t *testing.T) *client {
	t.Helper()
	registry := match.NewRegistry()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &client{
		t:        t,
		server:   New(registry, WithLogger(quiet), WithHandover(matchesToken)),
		registry: registry,
	}
}

func handoverBody(home, away, operator string) string {
	return fmt.Sprintf(`{"players":["Clara","Dora"],"home_id":%q,"away_id":%q,"operator_id":%q}`,
		home, away, operator)
}

func TestAMatchWithPlayerIdsIsAHandoverForWhoeverHoldsTheMatchesToken(t *testing.T) {
	c := handoverClient(t)
	c.bearer = matchesToken

	m, err := c.registry.Get(c.newMatch(handoverBody(claraID, doraID, pongOperatorID)))
	require.NoError(t, err)
	require.Equal(t, match.Handover{HomeID: claraID, AwayID: doraID, OperatorID: pongOperatorID}, m.Handover)
	require.True(t, m.Handover.Wanted(), "reported like a match from the page")
	require.False(t, m.Demo)
}

func TestAMatchWithPlayerIdsIsARealMatchThatADemoWaitsFor(t *testing.T) {
	c := handoverClient(t)
	c.bearer = matchesToken
	real := c.newMatch(handoverBody(claraID, doraID, pongOperatorID))

	c.bearer = ""
	body := c.failure(http.StatusConflict, http.MethodPost, "/matches", demoBody)
	require.Equal(t, real, body.State.MatchID)
}

func TestPlayerIdsWithoutTheMatchesTokenStartNothing(t *testing.T) {
	for name, bearer := range map[string]string{"no token": "", "another token": spToken} {
		t.Run(name, func(t *testing.T) {
			c := handoverClient(t)
			c.bearer = bearer

			rec, raw := c.do(http.MethodPost, "/matches", handoverBody(claraID, doraID, pongOperatorID))
			require.Equalf(t, http.StatusUnauthorized, rec.Code, "%s", raw)
			require.Equal(t, `Bearer realm="matches"`, rec.Header().Get("WWW-Authenticate"))

			_, ok := c.registry.Occupied()
			require.False(t, ok, "a refused handover creates no match")
		})
	}
}

// The office: no MATCHES_TOKEN, so the door is off and says so, whatever
// bearer comes with the body.
func TestPlayerIdsAreOffOnAZaehlwerkWithoutTheDoor(t *testing.T) {
	c := newClient(t)
	c.bearer = matchesToken

	body := c.failure(http.StatusServiceUnavailable, http.MethodPost, "/matches",
		handoverBody(claraID, doraID, pongOperatorID))
	require.Equal(t, errHandoverOff.Error(), body.Error)

	_, ok := c.registry.Occupied()
	require.False(t, ok)
}

func TestAMatchWithoutIdsNeedsNoTokenWhenTheDoorIsOn(t *testing.T) {
	c := handoverClient(t)

	m, err := c.registry.Get(c.newMatch(`{"players":["Anna","Bernd"]}`))
	require.NoError(t, err)
	require.Equal(t, match.Handover{}, m.Handover, "free text, reported nowhere, as before")
}

func TestAHandoverNamesAllThreeByTheirUUIDs(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"no operator": {
			`{"home_id":"` + claraID + `","away_id":"` + doraID + `"}`,
			"home_id, away_id and operator_id go together",
		},
		"only an operator": {
			`{"operator_id":"` + pongOperatorID + `"}`,
			"home_id, away_id and operator_id go together",
		},
		"not a UUID":     {handoverBody("clara", doraID, pongOperatorID), "home_id: must be a UUID"},
		"no dashes":      {handoverBody(claraID, "55555555555545558555555555555555", pongOperatorID), "away_id: must be a UUID"},
		"the same twice": {handoverBody(claraID, claraID, pongOperatorID), "home_id and away_id must differ"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := handoverClient(t)
			c.bearer = matchesToken

			body := c.failure(http.StatusBadRequest, http.MethodPost, "/matches", tc.body)
			require.Equal(t, tc.want, body.Error)
		})
	}
}

// ADR-0008 still holds at this door: a demo is never reported, so a demo with
// ids is the caller's mistake, not a match.
func TestADemoWithPlayerIdsIsStillRefused(t *testing.T) {
	c := handoverClient(t)
	c.bearer = matchesToken

	c.failure(http.StatusBadRequest, http.MethodPost, "/matches",
		`{"demo":true,"home_id":"`+claraID+`","away_id":"`+doraID+`","operator_id":"`+pongOperatorID+`"}`)

	_, ok := c.registry.Occupied()
	require.False(t, ok)
}

// tournamentID is a place's tournament in Schmetterpause.
const tournamentID = "99999999-9999-4999-8999-999999999999"

func tournamentBody(round int) string {
	return fmt.Sprintf(`{"players":["Clara","Dora"],"home_id":%q,"away_id":%q,"operator_id":%q,"tournament_id":%q,"round":%d}`,
		claraID, doraID, pongOperatorID, tournamentID, round)
}

func TestAHandoverCarriesItsTournamentPlace(t *testing.T) {
	c := handoverClient(t)
	c.bearer = matchesToken

	m, err := c.registry.Get(c.newMatch(tournamentBody(3)))
	require.NoError(t, err)
	require.Equal(t, match.Handover{
		HomeID: claraID, AwayID: doraID, OperatorID: pongOperatorID,
		TournamentID: tournamentID, Round: 3,
	}, m.Handover)
	require.True(t, m.Handover.Wanted())
}

func TestATournamentPlaceComesWholeAndOnlyWithThePlayers(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"a place without players": {
			`{"tournament_id":"` + tournamentID + `","round":1}`,
			"home_id, away_id and operator_id go together",
		},
		"a tournament without a round": {
			`{"home_id":"` + claraID + `","away_id":"` + doraID + `","operator_id":"` + pongOperatorID +
				`","tournament_id":"` + tournamentID + `"}`,
			"tournament_id and round go together",
		},
		"a round without a tournament": {
			`{"home_id":"` + claraID + `","away_id":"` + doraID + `","operator_id":"` + pongOperatorID + `","round":2}`,
			"tournament_id and round go together",
		},
		"round below one": {tournamentBody(-1), "round counts from 1"},
		"not a UUID": {
			`{"home_id":"` + claraID + `","away_id":"` + doraID + `","operator_id":"` + pongOperatorID +
				`","tournament_id":"montag","round":1}`,
			"tournament_id: must be a UUID",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := handoverClient(t)
			c.bearer = matchesToken

			body := c.failure(http.StatusBadRequest, http.MethodPost, "/matches", tc.body)
			require.Equal(t, tc.want, body.Error)
		})
	}
}

// A place is part of the handover, so it is behind the same door: without the
// token it starts nothing, even with no player ids beside it.
func TestATournamentPlaceWithoutTheMatchesTokenStartsNothing(t *testing.T) {
	c := handoverClient(t)

	rec, raw := c.do(http.MethodPost, "/matches", `{"tournament_id":"`+tournamentID+`","round":1}`)
	require.Equalf(t, http.StatusUnauthorized, rec.Code, "%s", raw)
	_, ok := c.registry.Occupied()
	require.False(t, ok)
}

func TestAFinalPlaceSaysSoOnTheHandover(t *testing.T) {
	c := handoverClient(t)
	c.bearer = matchesToken

	body := fmt.Sprintf(`{"home_id":%q,"away_id":%q,"operator_id":%q,"tournament_id":%q,"round":7,"final":true}`,
		claraID, doraID, pongOperatorID, tournamentID)
	m, err := c.registry.Get(c.newMatch(body))
	require.NoError(t, err)
	require.True(t, m.Handover.Final)
	require.Equal(t, 7, m.Handover.Round)
}

func TestAFinalWithoutATournamentIsRefused(t *testing.T) {
	c := handoverClient(t)
	c.bearer = matchesToken

	body := fmt.Sprintf(`{"home_id":%q,"away_id":%q,"operator_id":%q,"final":true}`, claraID, doraID, pongOperatorID)
	failure := c.failure(http.StatusBadRequest, http.MethodPost, "/matches", body)
	require.Equal(t, "final needs tournament_id and round", failure.Error)
}
