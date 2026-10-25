package pong

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
)

// League mode (ADR-0015): in the sandbox, one real match after another between
// two of Schmetterpause's players, each at the strength PONG_PLAYER_SKILL gives
// it, played by the server driver so it needs no screen and can be watched
// from /play/{id}. zaehlwerk reports every one of them; the game reports
// nothing itself, as for any game.
//
// What tells it from everything else the service plays:
//
//   - Its match is no demo. It carries a handover, so it is a real match to
//     zaehlwerk: it ends a running demo and a demo waits for it (ADR-0008).
//   - It has no faults, whatever PONG_FAULTS says. It tests what happens after
//     the score, and a score wrong on purpose would be a wrong result in
//     Schmetterpause.
//   - Between two matches it pauses for League.Pause, so the last result is
//     reported before the next match starts and the results arrive in the
//     order they were played.
//
// With League.Tournaments it plays tournaments first (ADR-0017): the first
// open place of the oldest tournament that has one, in that tournament's mode,
// and a random pair only when no tournament has a place left. The final comes
// by itself: Schmetterpause lists it once the group is played out, which the
// pause gives the last group result time to be.

// leagueSalt keeps the league's draw of players apart from the rallies of the
// games it starts, whose seeds come from the same PONG_SEED.
const leagueSalt int64 = 0x1ea9e

// runLeague plays league matches for as long as the service runs. A start
// that does not happen — the table taken, Schmetterpause or zaehlwerk not
// answering, no operator — is logged and tried again after the pause; nothing
// about it stops the service.
func (s *Server) runLeague(ctx context.Context) {
	l := s.cfg.League
	s.log.Info("league mode on", "operator", l.Operator, "pause", l.Pause.String(), "player_skills", len(l.Skills),
		"tournaments", l.Tournaments)
	rnd := rand.New(rand.NewSource(s.cfg.Seed ^ leagueSalt))
	told := &leagueTold{names: map[string]bool{}}

	for ctx.Err() == nil {
		g, err := s.startLeagueMatch(ctx, rnd, told)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			s.log.Warn("no league match, trying again after the pause", "error", err, "pause", l.Pause.String())
		default:
			select {
			case <-g.Done():
			case <-ctx.Done():
				return
			}
			v := g.View()
			s.log.Info("league match over", "game", g.ID, "match_id", v.MatchID, "status", v.Status,
				"reason", v.Reason, "players", v.Players, "unexplained", v.Unexplained)
		}
		if sleep(ctx, l.Pause) != nil {
			return
		}
	}
}

// leagueTold is what league mode has said once and will not say again, so a
// renamed bot or an old Schmetterpause is one line and not one per match.
type leagueTold struct {
	// names is the names PONG_PLAYER_SKILL gives that the roster lacked.
	names map[string]bool
	// noTournaments is a Schmetterpause without tournaments on its API.
	noTournaments bool
}

// startLeagueMatch starts the next league match: an open tournament place
// when there is one to play, else two players drawn at random.
func (s *Server) startLeagueMatch(ctx context.Context, rnd *rand.Rand, told *leagueTold) (*Game, error) {
	l := s.cfg.League
	players, err := l.Roster.Players(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the players: %w", err)
	}
	operatorID, err := s.leagueOperator(ctx)
	if err != nil {
		return nil, err
	}
	s.tellMissingSkills(players, told)

	if l.Tournaments {
		tour, place, ok, err := s.openPlace(ctx, told)
		if err != nil {
			return nil, err
		}
		if ok {
			return s.startTournamentMatch(tour, place, operatorID)
		}
	}

	pool := make([]schmetterpause.Player, 0, len(players))
	for _, p := range players {
		// The names zaehlwerk and the panel show, so only one the game
		// would take from the lobby too.
		if p.ID == operatorID || !showable(p.DisplayName) {
			continue
		}
		pool = append(pool, p)
	}
	if len(pool) < 2 {
		return nil, fmt.Errorf("%d players to draw from, a match needs two", len(pool))
	}

	i := rnd.Intn(len(pool))
	j := rnd.Intn(len(pool) - 1)
	if j >= i {
		j++
	}
	home, away := pool[i], pool[j]
	skills := [2]Skill{s.leagueSkill(home.DisplayName, 0), s.leagueSkill(away.DisplayName, 1)}
	bots := [2]BotParams{skills[0].Bot(), skills[1].Bot()}

	return s.start(GameConfig{
		Driver:   DriverServer,
		Mode:     ModeCvC,
		Players:  [2]string{home.DisplayName, away.DisplayName},
		Bots:     [2]*BotParams{&bots[0], &bots[1]},
		Skills:   skills,
		Faults:   Faults{},
		Handover: &Handover{HomeID: home.ID, AwayID: away.ID, OperatorID: operatorID},
	})
}

// tellMissingSkills logs, once each, a name PONG_PLAYER_SKILL gives that the
// roster lacks: that bot was renamed there, and plays at PONG_BOT_SKILL now.
func (s *Server) tellMissingSkills(players []schmetterpause.Player, told *leagueTold) {
	present := make(map[string]bool, len(players))
	for _, p := range players {
		present[p.DisplayName] = true
	}
	for name := range s.cfg.League.Skills {
		if !present[name] && !told.names[name] {
			s.log.Warn("PONG_PLAYER_SKILL names a player Schmetterpause does not have", "player", name)
			told.names[name] = true
		}
	}
}

// showable is a name the game would take from the lobby: short enough for the
// panel, and nothing in it that is not text.
func showable(name string) bool {
	return utf8.RuneCountInString(name) <= maxNameRunes && !strings.ContainsFunc(name, unicode.IsControl)
}

// openPlace is the place league mode plays next: the first open place, in draw
// order, of the oldest open tournament that has one. ok is false when no
// tournament has a place left, and for a Schmetterpause that lists no
// tournaments at all, which is said once.
//
// Read again before every match, never kept: a place is filled at the kiosk as
// well, and the final exists only once the table names it.
func (s *Server) openPlace(ctx context.Context, told *leagueTold) (
	schmetterpause.Tournament, schmetterpause.Place, bool, error,
) {
	roster := s.cfg.League.Roster
	tours, err := roster.Tournaments(ctx)
	if errors.Is(err, schmetterpause.ErrNoTournaments) {
		if !told.noTournaments {
			s.log.Warn("PONG_TOURNAMENTS is on, but this Schmetterpause lists no tournaments; playing the league")
			told.noTournaments = true
		}
		return schmetterpause.Tournament{}, schmetterpause.Place{}, false, nil
	}
	if err != nil {
		return schmetterpause.Tournament{}, schmetterpause.Place{}, false, fmt.Errorf("reading the tournaments: %w", err)
	}
	// Oldest first whatever order they came in: the one that has waited
	// longest is played to its end before the next begins.
	slices.SortStableFunc(tours, func(a, b schmetterpause.Tournament) int { return a.CreatedAt.Compare(b.CreatedAt) })

	for _, t := range tours {
		places, err := roster.Pairings(ctx, t.ID)
		if err != nil {
			return schmetterpause.Tournament{}, schmetterpause.Place{}, false,
				fmt.Errorf("reading the places of tournament %q: %w", t.Name, err)
		}
		for _, p := range places {
			// A name the panel cannot show would be refused by zaehlwerk at
			// every start, and the tournament would stand still on it.
			if showable(p.HomeName) && showable(p.AwayName) {
				return t, p, true, nil
			}
			s.log.Warn("skipping a tournament place whose names the panel cannot show",
				"tournament", t.Name, "round", p.Round)
		}
	}
	return schmetterpause.Tournament{}, schmetterpause.Place{}, false, nil
}

// startTournamentMatch starts the match for a place: its two players, home on
// the left, in the tournament's mode, each at their strength.
func (s *Server) startTournamentMatch(t schmetterpause.Tournament, p schmetterpause.Place, operatorID string) (*Game, error) {
	skills := [2]Skill{s.leagueSkill(p.HomeName, 0), s.leagueSkill(p.AwayName, 1)}
	bots := [2]BotParams{skills[0].Bot(), skills[1].Bot()}
	s.log.Info("tournament match", "tournament", t.Name, "tournament_id", t.ID,
		"round", p.Round, "final", p.Final, "players", []string{p.HomeName, p.AwayName},
		"best_of", t.BestOf, "points_to_win", t.PointsToWin)

	return s.start(GameConfig{
		Driver:       DriverServer,
		Mode:         ModeCvC,
		BestOf:       t.BestOf,
		PointsPerSet: t.PointsToWin,
		Players:      [2]string{p.HomeName, p.AwayName},
		Bots:         [2]*BotParams{&bots[0], &bots[1]},
		Skills:       skills,
		Faults:       Faults{},
		Handover: &Handover{
			HomeID: p.HomeID, AwayID: p.AwayID, OperatorID: operatorID,
			TournamentID: t.ID, Round: p.Round, Final: p.Final,
		},
	})
}

// errNoOperator is a Schmetterpause without the account PONG_OPERATOR names.
var errNoOperator = errors.New("no operator by that name")

// leagueOperator is the id of the account PONG_OPERATOR names, looked up at
// every start rather than once: the sandbox can be seeded after Pong is up.
func (s *Server) leagueOperator(ctx context.Context) (string, error) {
	ops, err := s.cfg.League.Roster.Operators(ctx)
	if err != nil {
		return "", fmt.Errorf("reading the operators: %w", err)
	}
	for _, op := range ops {
		if op.DisplayName == s.cfg.League.Operator {
			return op.ID, nil
		}
	}
	return "", fmt.Errorf("%w: %q is not in Schmetterpause's /api/operators", errNoOperator, s.cfg.League.Operator)
}

// leagueSkill is a player's strength: PONG_PLAYER_SKILL's for its name, else
// PONG_BOT_SKILL's for the side it plays on.
func (s *Server) leagueSkill(name string, side int) Skill {
	if skill, ok := s.cfg.League.Skills[name]; ok {
		return skill
	}
	return s.cfg.Skills[side]
}
