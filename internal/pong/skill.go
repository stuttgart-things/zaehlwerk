package pong

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
)

// Skill is how well a bot plays. Headless, a rally is not played with a ball
// and paddles but drawn: the two bots hit in turn, the server first, and each
// shot misses with the bot's Miss share. The first miss loses the rally.
//
// One number is enough for a winner drawn by skill. The browser's bot needs
// three, and they are derived from this one (see [Skill.Bot]) rather than
// stored next to it, so a preset means the same strength in both games.
type Skill struct {
	Name string  `json:"name"`
	Miss float64 `json:"miss"`
}

// The presets carry the names a player sees on the page, which are German like
// the panel's banners.
var skills = map[string]Skill{
	"leicht": {Name: "leicht", Miss: 0.30},
	"mittel": {Name: "mittel", Miss: 0.15},
	"schwer": {Name: "schwer", Miss: 0.06},
}

// DefaultSkill is the preset a bot plays with unless told otherwise.
const DefaultSkill = "mittel"

// ParseSkills reads one preset for both bots, or two separated by a comma for
// the bots a and b.
func ParseSkills(raw string) ([2]Skill, error) {
	names := strings.Split(raw, ",")
	if len(names) > 2 {
		return [2]Skill{}, fmt.Errorf("skill %q: one preset, or two separated by a comma", raw)
	}
	var out [2]Skill
	for i := range out {
		name := strings.TrimSpace(names[min(i, len(names)-1)])
		s, ok := skills[name]
		if !ok {
			return out, fmt.Errorf("skill %q: want one of %s", name, strings.Join(presetNames(), ", "))
		}
		out[i] = s
	}
	return out, nil
}

// ParsePlayerSkills reads PONG_PLAYER_SKILL: a strength per Schmetterpause
// display name, for league mode (ADR-0015), e.g. "Ada=schwer,Bert=0.2". A
// strength is a preset or a miss share of its own, the same Skill.Miss a preset
// stands for, so it means the same in a headless game and on the court. A share
// is bounded by what the court's bot can play: above 0.55 its Bot() aims past
// the edge of the paddle.
//
// The names are the sandbox seed's in Schmetterpause, and nothing ties the two
// together but this string: a name it gives that the roster does not have is
// logged by league mode, not refused here.
func ParsePlayerSkills(raw string) (map[string]Skill, error) {
	out := map[string]Skill{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	for _, entry := range strings.Split(raw, ",") {
		name, strength, ok := strings.Cut(entry, "=")
		name, strength = strings.TrimSpace(name), strings.TrimSpace(strength)
		if !ok || name == "" || strength == "" {
			return nil, fmt.Errorf("player skill %q: want name=preset or name=share", strings.TrimSpace(entry))
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("player skill: %q is named twice", name)
		}
		skill, err := parseStrength(strength)
		if err != nil {
			return nil, fmt.Errorf("player skill for %q: %w", name, err)
		}
		out[name] = skill
	}
	return out, nil
}

func parseStrength(raw string) (Skill, error) {
	if s, ok := skills[raw]; ok {
		return s, nil
	}
	miss, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return Skill{}, fmt.Errorf("%q: want one of %s, or a miss share such as 0.2",
			raw, strings.Join(presetNames(), ", "))
	}
	// Above zero, because two bots that never miss draw a headless rally
	// that never ends.
	s := Skill{Name: raw, Miss: miss}
	if !(miss > 0) {
		return Skill{}, fmt.Errorf("miss share %v: want more than 0", miss)
	}
	if err := s.Bot().Validate(); err != nil {
		return Skill{}, fmt.Errorf("miss share %v is more than the court's bot can play: %w", miss, err)
	}
	return s, nil
}

func presetNames() []string {
	names := make([]string, 0, len(skills))
	for n := range skills {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Rally draws one rally between the bots 0 and 1, the server hitting first, and
// returns the index of the winner.
func Rally(rnd *rand.Rand, bots [2]Skill, server int) int {
	hitter := server
	for {
		if rnd.Float64() < bots[hitter].Miss {
			return 1 - hitter
		}
		hitter = 1 - hitter
	}
}

// BotParams is how the browser's bot plays: it does not draw rallies, it moves
// a paddle, so it needs what a person at the table has instead of a miss rate.
type BotParams struct {
	// ReactionMS is how long the bot takes to start moving once the ball
	// comes its way.
	ReactionMS int `json:"reaction_ms"`
	// Speed is the fastest the paddle moves, in table widths a second.
	Speed float64 `json:"speed"`
	// Aim is how far off the bot meets the ball, as a share of half the
	// paddle: 0 is the centre every time, 1 the edge at worst.
	Aim float64 `json:"aim"`
}

// The bounds the lobby's sliders allow, and that a start is checked against.
var (
	botReactionMS = [2]int{0, 1000}
	botSpeed      = [2]float64{0.3, 4}
	botAim        = [2]float64{0, 1.2}
)

// Bot derives the browser bot from the miss rate, linearly: a bot that misses
// more reacts later, moves slower and meets the ball further off centre.
// leicht comes out at 270 ms, 1.35 and 0.70; schwer at 102 ms, 2.19 and 0.22.
func (s Skill) Bot() BotParams {
	return BotParams{
		ReactionMS: int(math.Round(60 + 700*s.Miss)),
		Speed:      math.Round((2.4-3.5*s.Miss)*100) / 100,
		Aim:        math.Round((0.1+2*s.Miss)*100) / 100,
	}
}

// Validate refuses values the game cannot play with.
func (b BotParams) Validate() error {
	switch {
	case b.ReactionMS < botReactionMS[0] || b.ReactionMS > botReactionMS[1]:
		return fmt.Errorf("bot reaction %d ms: want %d to %d", b.ReactionMS, botReactionMS[0], botReactionMS[1])
	case math.IsNaN(b.Speed) || b.Speed < botSpeed[0] || b.Speed > botSpeed[1]:
		return fmt.Errorf("bot speed %v: want %v to %v", b.Speed, botSpeed[0], botSpeed[1])
	case math.IsNaN(b.Aim) || b.Aim < botAim[0] || b.Aim > botAim[1]:
		return fmt.Errorf("bot aim %v: want %v to %v", b.Aim, botAim[0], botAim[1])
	}
	return nil
}

// Preset looks up a preset by its name.
func Preset(name string) (Skill, bool) {
	s, ok := skills[name]
	return s, ok
}

// PresetNames lists the presets, the easiest first.
func PresetNames() []string { return []string{"leicht", "mittel", "schwer"} }
