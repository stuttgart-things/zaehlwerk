package pong

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
)

// Skill is how well a bot plays. Headless, a rally is not played with a ball
// and paddles but drawn: the two bots hit in turn, the server first, and each
// shot misses with the bot's Miss share. The first miss loses the rally.
//
// One number is enough for a winner drawn by skill. Reaction delay, paddle
// speed and aim error are what the browser game will need, and they will be
// derived from the preset there rather than stored next to this.
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
