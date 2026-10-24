package pong

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
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

// leagueSalt keeps the league's draw of players apart from the rallies of the
// games it starts, whose seeds come from the same PONG_SEED.
const leagueSalt int64 = 0x1ea9e

// runLeague plays league matches for as long as the service runs. A start
// that does not happen — the table taken, Schmetterpause or zaehlwerk not
// answering, no operator — is logged and tried again after the pause; nothing
// about it stops the service.
func (s *Server) runLeague(ctx context.Context) {
	l := s.cfg.League
	s.log.Info("league mode on", "operator", l.Operator, "pause", l.Pause.String(), "player_skills", len(l.Skills))
	rnd := rand.New(rand.NewSource(s.cfg.Seed ^ leagueSalt))
	// told is the names PONG_PLAYER_SKILL gives that the roster lacked, so a
	// renamed bot is one line and not one per match.
	told := map[string]bool{}

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

// startLeagueMatch draws two players and starts their match.
func (s *Server) startLeagueMatch(ctx context.Context, rnd *rand.Rand, told map[string]bool) (*Game, error) {
	l := s.cfg.League
	players, err := l.Roster.Players(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the players: %w", err)
	}
	operatorID, err := s.leagueOperator(ctx)
	if err != nil {
		return nil, err
	}

	pool := make([]schmetterpause.Player, 0, len(players))
	present := map[string]bool{}
	for _, p := range players {
		present[p.DisplayName] = true
		// The names zaehlwerk and the panel show, so only one the game
		// would take from the lobby too.
		if p.ID == operatorID || utf8.RuneCountInString(p.DisplayName) > maxNameRunes ||
			strings.ContainsFunc(p.DisplayName, unicode.IsControl) {
			continue
		}
		pool = append(pool, p)
	}
	for name := range l.Skills {
		if !present[name] && !told[name] {
			s.log.Warn("PONG_PLAYER_SKILL names a player Schmetterpause does not have", "player", name)
			told[name] = true
		}
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
