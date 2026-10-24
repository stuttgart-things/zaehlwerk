package pong

import (
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
)

func rallies(t *testing.T, bots [2]Skill, n int) [2]int {
	t.Helper()
	rnd := rand.New(rand.NewSource(1))
	var won [2]int
	for i := range n {
		won[Rally(rnd, bots, i%2)]++
	}
	return won
}

func TestAStrongerBotWinsMostRallies(t *testing.T) {
	won := rallies(t, [2]Skill{skills["schwer"], skills["leicht"]}, 10000)
	require.Greater(t, won[0], 7*won[1]/2, "schwer against leicht: %v", won)
}

func TestEqualBotsShareTheRallies(t *testing.T) {
	won := rallies(t, [2]Skill{skills["mittel"], skills["mittel"]}, 10000)
	require.InDelta(t, 5000, won[0], 250, "mittel against mittel: %v", won)
}

func TestABotMissesAsOftenAsItsSkillSays(t *testing.T) {
	// Against a side that misses every return, a rally the bot serves is won
	// exactly when its own shot lands, so the share it loses is its miss
	// share. Against a wall that returns everything, it loses every rally.
	never := Skill{Name: "never", Miss: 1}
	wall := Skill{Name: "wall", Miss: 0}
	rnd := rand.New(rand.NewSource(1))

	for _, s := range skills {
		var lost int
		for range 20000 {
			lost += Rally(rnd, [2]Skill{s, never}, 0)
		}
		require.InDelta(t, s.Miss, float64(lost)/20000, 0.01, s.Name)

		for range 1000 {
			require.Equal(t, 1, Rally(rnd, [2]Skill{s, wall}, 0), s.Name)
		}
	}
}

func TestTheSameSeedDrawsTheSameRallies(t *testing.T) {
	bots := [2]Skill{skills["mittel"], skills["mittel"]}
	a, b := rand.New(rand.NewSource(42)), rand.New(rand.NewSource(42))
	for i := range 200 {
		require.Equal(t, Rally(a, bots, i%2), Rally(b, bots, i%2))
	}
}

func TestASkillIsOnePresetForBothOrOneForEach(t *testing.T) {
	both, err := ParseSkills("schwer")
	require.NoError(t, err)
	require.Equal(t, [2]Skill{skills["schwer"], skills["schwer"]}, both)

	each, err := ParseSkills("leicht, schwer")
	require.NoError(t, err)
	require.Equal(t, [2]Skill{skills["leicht"], skills["schwer"]}, each)
}

func TestAnUnknownSkillIsRefusedWithThePresets(t *testing.T) {
	_, err := ParseSkills("profi")
	require.ErrorContains(t, err, "leicht, mittel, schwer")

	_, err = ParseSkills("leicht,mittel,schwer")
	require.ErrorContains(t, err, "one preset, or two")
}

func TestAPlayerTakesAPresetOrAMissShareOfItsOwn(t *testing.T) {
	got, err := ParsePlayerSkills(" Ada=schwer, Bert=0.2 ,Cleo=leicht")
	require.NoError(t, err)
	require.Equal(t, map[string]Skill{
		"Ada":  skills["schwer"],
		"Bert": {Name: "0.2", Miss: 0.2},
		"Cleo": skills["leicht"],
	}, got)
}

func TestNoPlayerSkillsIsAnEmptyMap(t *testing.T) {
	got, err := ParsePlayerSkills("")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestAPlayerSkillTheCourtCannotPlayIsRefused(t *testing.T) {
	for raw, want := range map[string]string{
		"Ada":             "want name=preset or name=share",
		"Ada=":            "want name=preset or name=share",
		"=schwer":         "want name=preset or name=share",
		"Ada=profi":       `"profi": want one of`,
		"Ada=0":           "want more than 0",
		"Ada=-0.1":        "want more than 0",
		"Ada=NaN":         "want more than 0",
		"Ada=0.6":         "more than the court's bot can play",
		"Ada=0.1,Ada=0.2": `"Ada" is named twice`,
	} {
		_, err := ParsePlayerSkills(raw)
		require.ErrorContains(t, err, want, raw)
	}
}

// The bound in ParsePlayerSkills' comment, checked: the court's bot can still
// play the worst share it lets through.
func TestTheWorstShareAPlayerMayHaveIsOneTheCourtCanPlay(t *testing.T) {
	_, err := ParsePlayerSkills("Ada=0.55")
	require.NoError(t, err)
}
