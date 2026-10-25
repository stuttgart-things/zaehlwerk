package pong

import (
	"bytes"
	"cmp"
	"context"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/api"
	"github.com/stuttgart-things/zaehlwerk/internal/live"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// League mode plays against the real zaehlwerk handlers with the handover door
// on, as the sandbox's zaehlwerk runs (ADR-0015), and draws from a roster
// standing in for the sandbox's Schmetterpause.

const leagueToken = "matches-secret"

// The sandbox's players and its observer, by Schmetterpause id.
const (
	adaID  = "aaaaaaaa-0000-4000-8000-000000000001"
	bertID = "aaaaaaaa-0000-4000-8000-000000000002"
	pongID = "aaaaaaaa-0000-4000-8000-0000000000ff"
)

type fakeRoster struct {
	mu        sync.Mutex
	players   []schmetterpause.Player
	operators []schmetterpause.Operator
	// tournaments and places stand in for Schmetterpause's draw. Nil
	// tournaments is a Schmetterpause from before tournaments, a 404.
	tournaments []schmetterpause.Tournament
	places      map[string][]schmetterpause.Place
}

func (r *fakeRoster) Tournaments(context.Context) ([]schmetterpause.Tournament, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tournaments == nil {
		return nil, schmetterpause.ErrNoTournaments
	}
	return append([]schmetterpause.Tournament(nil), r.tournaments...), nil
}

func (r *fakeRoster) Pairings(_ context.Context, id string) ([]schmetterpause.Place, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]schmetterpause.Place(nil), r.places[id]...), nil
}

// fill takes a played place off the draw, as Schmetterpause does once its
// result arrives.
func (r *fakeRoster) fill(id string, round int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	places := r.places[id]
	for i, p := range places {
		if p.Round == round {
			r.places[id] = append(places[:i:i], places[i+1:]...)
			return
		}
	}
}

func (r *fakeRoster) Players(context.Context) ([]schmetterpause.Player, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]schmetterpause.Player(nil), r.players...), nil
}

func (r *fakeRoster) Operators(context.Context) ([]schmetterpause.Operator, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]schmetterpause.Operator(nil), r.operators...), nil
}

func sandboxRoster() *fakeRoster {
	return &fakeRoster{
		players: []schmetterpause.Player{
			{ID: adaID, DisplayName: "Ada"},
			{ID: bertID, DisplayName: "Bert"},
		},
		operators: []schmetterpause.Operator{
			{ID: adaID, DisplayName: "Ada"},
			{ID: bertID, DisplayName: "Bert"},
			{ID: pongID, DisplayName: "Pong", Observer: true},
		},
	}
}

// newSandboxTable is zaehlwerk as the sandbox runs it: taking player ids
// behind token.
func newSandboxTable(t *testing.T, token string, opts ...match.Option) *table {
	t.Helper()
	hub := live.New()
	registry := match.NewRegistry(append([]match.Option{match.WithObserver(hub.Observe)}, opts...)...)
	zw := api.New(registry, api.WithLogger(quiet()), api.WithHub(hub), api.WithHandover(token))
	srv := httptest.NewServer(zw)
	t.Cleanup(srv.Close)
	return &table{t: t, registry: registry, zaehlwerk: zw, url: srv.URL}
}

// leaguePong is a service in league mode, its court running as fast as it
// goes, started and stopped with the test. logs is what it logged.
func leaguePong(t *testing.T, zaehlwerkURL string, l League) (*Server, *lockedLog) {
	t.Helper()
	logs := &lockedLog{}
	p := New(Config{
		ZaehlwerkURL: zaehlwerkURL,
		BestOf:       1,
		Skills:       [2]Skill{skills["mittel"], skills["mittel"]},
		Seed:         7,
		League:       &l,
	}, slog.New(slog.NewTextHandler(logs, nil)))
	p.client.retryAfter = time.Millisecond
	p.tick = 0
	p.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, p.Close(ctx))
	})
	return p, logs
}

// lockedLog is a log the service writes from its goroutines while the test
// reads it.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func sandboxLeague(roster LeagueRoster) League {
	return League{
		Token:    leagueToken,
		Operator: "Pong",
		Skills:   map[string]Skill{"Ada": skills["schwer"], "Bert": skills["leicht"]},
		Pause:    time.Hour,
		Roster:   roster,
	}
}

// firstLeagueGame waits for the first league game to be won and returns it.
func firstLeagueGame(t *testing.T, p *Server) *Game {
	t.Helper()
	var won *Game
	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, g := range p.games {
			if g.cfg.Handover != nil && g.View().Status == StatusWon {
				won = g
				return true
			}
		}
		return false
	}, 20*time.Second, 10*time.Millisecond)
	return won
}

func TestALeagueMatchIsARealMatchBetweenTwoPlayersThatZaehlwerkReports(t *testing.T) {
	zw := newSandboxTable(t, leagueToken)
	p, _ := leaguePong(t, zw.url, sandboxLeague(sandboxRoster()))

	g := firstLeagueGame(t, p)
	m, err := zw.registry.Get(g.matchIDNow())
	require.NoError(t, err)

	require.False(t, m.Demo, "a league match gives way to nothing")
	require.True(t, m.Handover.Wanted(), "zaehlwerk reports it when it is won")
	require.Equal(t, pongID, m.Handover.OperatorID, "counted by the observer PONG_OPERATOR names")
	require.ElementsMatch(t, []string{adaID, bertID}, []string{m.Handover.HomeID, m.Handover.AwayID})

	v := g.View()
	require.Equal(t, DriverServer, v.Driver, "played on the court, so it can be watched")
	require.Zero(t, v.Unexplained, "no faults in a league match: %+v", v.Divergences)
	require.Equal(t, v.Truth, v.Zaehlwerk)
}

func TestEachLeaguePlayerPlaysAtTheStrengthItsNameIsGiven(t *testing.T) {
	zw := newSandboxTable(t, leagueToken)
	p, _ := leaguePong(t, zw.url, sandboxLeague(sandboxRoster()))

	v := firstLeagueGame(t, p).View()
	for i, name := range v.Players {
		want := map[string]Skill{"Ada": skills["schwer"], "Bert": skills["leicht"]}[name]
		require.Equal(t, want, v.Skills[i], "player %s", name)
	}
}

func TestALeaguePlayerWithoutAStrengthPlaysAtTheBotSkillAndTheMissingNameIsLogged(t *testing.T) {
	zw := newSandboxTable(t, leagueToken)
	l := sandboxLeague(sandboxRoster())
	l.Skills = map[string]Skill{"Ada": skills["schwer"], "Adah": skills["leicht"]}
	p, logs := leaguePong(t, zw.url, l)

	v := firstLeagueGame(t, p).View()
	for i, name := range v.Players {
		if name == "Bert" {
			require.Equal(t, skills["mittel"], v.Skills[i], "PONG_BOT_SKILL for a player it does not name")
		}
	}
	require.Contains(t, logs.String(), "PONG_PLAYER_SKILL names a player Schmetterpause does not have")
	require.Contains(t, logs.String(), "player=Adah")
}

func TestLeagueModeStartsNothingWithoutItsOperator(t *testing.T) {
	zw := newSandboxTable(t, leagueToken)
	roster := sandboxRoster()
	roster.operators = roster.operators[:2]
	_, logs := leaguePong(t, zw.url, sandboxLeague(roster))

	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), `\"Pong\" is not in Schmetterpause's /api/operators`)
	}, 5*time.Second, 10*time.Millisecond)
	_, err := zw.registry.Current()
	require.Error(t, err, "no match without somebody to name as counting")
}

func TestLeagueModeWithTheWrongTokenSaysSoAndStartsNothing(t *testing.T) {
	zw := newSandboxTable(t, "another-secret")
	_, logs := leaguePong(t, zw.url, sandboxLeague(sandboxRoster()))

	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "ZAEHLWERK_TOKEN is not this zaehlwerk's MATCHES_TOKEN")
	}, 5*time.Second, 10*time.Millisecond)
	_, err := zw.registry.Current()
	require.Error(t, err)
}

// The office zaehlwerk has no MATCHES_TOKEN. A Pong pointed at it in league
// mode by mistake starts nothing that could be reported.
func TestLeagueModeAgainstAZaehlwerkWithoutTheDoorStartsNothing(t *testing.T) {
	zw := newTable(t)
	_, logs := leaguePong(t, zw.url, sandboxLeague(sandboxRoster()))

	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "this zaehlwerk takes no player ids")
	}, 5*time.Second, 10*time.Millisecond)
	_, err := zw.registry.Current()
	require.Error(t, err)
}

func TestLeagueMatchesFollowOneAnotherAfterThePause(t *testing.T) {
	zw := newSandboxTable(t, leagueToken)
	l := sandboxLeague(sandboxRoster())
	l.Pause = time.Millisecond
	p, _ := leaguePong(t, zw.url, l)

	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		won := 0
		for _, g := range p.games {
			if g.cfg.Handover != nil && g.View().Status == StatusWon {
				won++
			}
		}
		return won >= 2
	}, 30*time.Second, 10*time.Millisecond)
}

// The sandbox's tournaments, by Schmetterpause id.
const (
	mondayID  = "bbbbbbbb-0000-4000-8000-000000000001"
	tuesdayID = "bbbbbbbb-0000-4000-8000-000000000002"
)

// tournamentRoster is the sandbox with two open tournaments, the older one
// listed second, as a list newest first would have it.
func tournamentRoster() *fakeRoster {
	r := sandboxRoster()
	r.tournaments = []schmetterpause.Tournament{
		{ID: tuesdayID, Name: "Dienstag", BestOf: 3, PointsToWin: 11, CreatedAt: time.Now()},
		{ID: mondayID, Name: "Montag", BestOf: 1, PointsToWin: 5, CreatedAt: time.Now().Add(-time.Hour)},
	}
	r.places = map[string][]schmetterpause.Place{
		mondayID: {
			{Round: 2, HomeID: bertID, AwayID: adaID, HomeName: "Bert", AwayName: "Ada"},
			{Round: 3, Final: true, HomeID: adaID, AwayID: bertID, HomeName: "Ada", AwayName: "Bert"},
		},
		tuesdayID: {
			{Round: 1, HomeID: adaID, AwayID: bertID, HomeName: "Ada", AwayName: "Bert"},
		},
	}
	return r
}

func tournamentLeague(roster LeagueRoster) League {
	l := sandboxLeague(roster)
	l.Tournaments = true
	return l
}

func TestLeagueModePlaysTheFirstOpenPlaceOfTheOldestTournamentInItsMode(t *testing.T) {
	zw := newSandboxTable(t, leagueToken)
	p, _ := leaguePong(t, zw.url, tournamentLeague(tournamentRoster()))

	g := firstLeagueGame(t, p)
	m, err := zw.registry.Get(g.matchIDNow())
	require.NoError(t, err)

	require.Equal(t, mondayID, m.Handover.TournamentID, "Montag has waited longest")
	require.Equal(t, 2, m.Handover.Round, "its first open place in the draw's order")
	require.Equal(t, bertID, m.Handover.HomeID, "home is side A, the left half")
	require.Equal(t, adaID, m.Handover.AwayID)
	require.Equal(t, pongID, m.Handover.OperatorID)

	bestOf, points := m.Scorer.Rules()
	require.Equal(t, 1, bestOf, "the tournament's mode, not PONG_BEST_OF")
	require.Equal(t, 5, points)
	require.Equal(t, [2]string{"Bert", "Ada"}, g.View().Players)
	require.Equal(t, skills["leicht"], g.View().Skills[0], "Bert at his strength")
}

// Once every tournament is played, the league goes on with random pairs.
func TestLeagueModeDrawsARandomPairWhenNoTournamentHasAPlaceLeft(t *testing.T) {
	zw := newSandboxTable(t, leagueToken)
	roster := tournamentRoster()
	roster.places = map[string][]schmetterpause.Place{}
	p, _ := leaguePong(t, zw.url, tournamentLeague(roster))

	g := firstLeagueGame(t, p)
	m, err := zw.registry.Get(g.matchIDNow())
	require.NoError(t, err)
	require.Empty(t, m.Handover.TournamentID, "a league match has no place")
	require.Zero(t, m.Handover.Round)
}

func TestLeagueModeAgainstASchmetterpauseWithoutTournamentsSaysSoOnceAndPlaysTheLeague(t *testing.T) {
	zw := newSandboxTable(t, leagueToken)
	l := tournamentLeague(sandboxRoster())
	l.Pause = time.Millisecond
	p, logs := leaguePong(t, zw.url, l)

	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		won := 0
		for _, g := range p.games {
			if g.cfg.Handover != nil && g.View().Status == StatusWon {
				won++
			}
		}
		return won >= 2
	}, 30*time.Second, 10*time.Millisecond)
	require.Equal(t, 1, strings.Count(logs.String(), "this Schmetterpause lists no tournaments"))
}

// Places are read again before every match, so a tournament is played through
// in order, and the league waits until it is.
func TestLeagueModePlaysATournamentThroughBeforeTheLeague(t *testing.T) {
	roster := tournamentRoster()
	delete(roster.places, tuesdayID)
	// Schmetterpause takes a place off once its result arrives; here that is
	// the moment the match is won.
	zw := newSandboxTable(t, leagueToken, match.WithMatchObserver(func(m *match.Match, tr scorer.Transition) {
		if tr.Kind == scorer.TransitionMatchWon && m.Handover.TournamentID != "" {
			roster.fill(m.Handover.TournamentID, m.Handover.Round)
		}
	}))
	l := tournamentLeague(roster)
	l.Pause = time.Millisecond
	p, _ := leaguePong(t, zw.url, l)

	// The games in the order they started: the seed counts up with every
	// start.
	var rounds []int
	require.Eventually(t, func() bool {
		p.mu.Lock()
		var won []*Game
		for _, g := range p.games {
			if g.cfg.Handover != nil && g.View().Status == StatusWon {
				won = append(won, g)
			}
		}
		p.mu.Unlock()
		slices.SortFunc(won, func(a, b *Game) int { return cmp.Compare(a.cfg.Seed, b.cfg.Seed) })

		rounds = rounds[:0]
		for _, g := range won {
			rounds = append(rounds, g.cfg.Handover.Round)
		}
		return len(rounds) >= 3
	}, 30*time.Second, 10*time.Millisecond)
	require.Equal(t, []int{2, 3, 0}, rounds[:3], "both places of Montag, then a league match")

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, g := range p.games {
		if g.cfg.Handover != nil && g.cfg.Handover.Round == 3 {
			m, err := zw.registry.Get(g.matchIDNow())
			require.NoError(t, err)
			require.True(t, m.Handover.Final, "the final reaches zaehlwerk as the final")
		}
	}
}
