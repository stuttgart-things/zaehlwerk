package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTheDefaultsNeedNoEnvironment(t *testing.T) {
	for _, key := range []string{"ZAEHLWERK_URL", "PONG_BEST_OF", "PONG_BOT_SKILL", "PONG_SEED"} {
		t.Setenv(key, "")
	}
	cfg, err := config()
	require.NoError(t, err)
	require.Equal(t, "http://localhost:8080", cfg.ZaehlwerkURL)
	require.Equal(t, 3, cfg.BestOf)
	require.Equal(t, "mittel", cfg.Skills[0].Name)
	require.Equal(t, "mittel", cfg.Skills[1].Name)
	require.Equal(t, "dev", cfg.Build.Version)
}

func TestAFixedSeedIsTaken(t *testing.T) {
	t.Setenv("PONG_SEED", "42")
	cfg, err := config()
	require.NoError(t, err)
	require.EqualValues(t, 42, cfg.Seed)
}

func TestAnEvenBestOfIsRefusedAtStartup(t *testing.T) {
	t.Setenv("PONG_BEST_OF", "4")
	_, err := config()
	require.ErrorContains(t, err, "PONG_BEST_OF")
}

func TestAnUnknownSkillIsRefusedAtStartup(t *testing.T) {
	t.Setenv("PONG_BOT_SKILL", "profi")
	_, err := config()
	require.ErrorContains(t, err, "PONG_BOT_SKILL")
}

func TestFaultsAreTakenFromTheEnvironment(t *testing.T) {
	t.Setenv("PONG_FAULTS", "wrong_half=0.05,resend=0.1,correct=true")
	cfg, err := config()
	require.NoError(t, err)
	require.Equal(t, 0.05, cfg.Faults.WrongHalf)
	require.Equal(t, 0.1, cfg.Faults.Resend)
	require.True(t, cfg.Faults.Correct)
}

func TestAnUnknownFaultIsRefusedAtStartup(t *testing.T) {
	t.Setenv("PONG_FAULTS", "wrong-half=0.05")
	_, err := config()
	require.ErrorContains(t, err, "PONG_FAULTS")
}

func TestAttractModeIsOffUnlessItIsSet(t *testing.T) {
	t.Setenv("PONG_ATTRACT_AFTER", "")
	cfg, err := config()
	require.NoError(t, err)
	require.Zero(t, cfg.AttractAfter)
	require.Equal(t, time.Second, cfg.AttractPace)
	require.Equal(t, 10*time.Minute, cfg.BrowserIdle)
	require.Equal(t, 10*time.Second, cfg.CheckTimeout)
}

func TestTheWaitsAreTakenFromTheEnvironment(t *testing.T) {
	t.Setenv("PONG_ATTRACT_AFTER", "10m")
	t.Setenv("PONG_ATTRACT_PACE", "1500ms")
	t.Setenv("PONG_BROWSER_IDLE", "30m")
	t.Setenv("PONG_CHECK_TIMEOUT", "20s")
	cfg, err := config()
	require.NoError(t, err)
	require.Equal(t, 10*time.Minute, cfg.AttractAfter)
	require.Equal(t, 1500*time.Millisecond, cfg.AttractPace)
	require.Equal(t, 30*time.Minute, cfg.BrowserIdle)
	require.Equal(t, 20*time.Second, cfg.CheckTimeout)
}

func TestADurationThatIsNoneIsRefusedAtStartup(t *testing.T) {
	for _, key := range []string{"PONG_ATTRACT_AFTER", "PONG_ATTRACT_PACE", "PONG_BROWSER_IDLE", "PONG_CHECK_TIMEOUT"} {
		for _, raw := range []string{"10", "0s", "-1m"} {
			t.Setenv(key, raw)
			_, err := config()
			require.ErrorContains(t, err, key, raw)
			t.Setenv(key, "")
		}
	}
}

func TestLeagueModeIsOffWithoutZaehlwerksToken(t *testing.T) {
	t.Setenv("ZAEHLWERK_TOKEN", "")
	t.Setenv("SCHMETTERPAUSE_URL", "http://sp.invalid")
	t.Setenv("SCHMETTERPAUSE_TOKEN", "sp-secret")
	cfg, err := config()
	require.NoError(t, err)
	require.Nil(t, cfg.League)
}

// A token with nobody to draw players from starts nothing that could be
// wrong, so it is off, not refused.
func TestLeagueModeIsOffWithoutTheSchmetterpauseItDrawsFrom(t *testing.T) {
	t.Setenv("ZAEHLWERK_TOKEN", "matches-secret")
	t.Setenv("SCHMETTERPAUSE_URL", "")
	cfg, err := config()
	require.NoError(t, err)
	require.Nil(t, cfg.League)

	t.Setenv("SCHMETTERPAUSE_URL", "http://sp.invalid")
	t.Setenv("SCHMETTERPAUSE_TOKEN", "")
	cfg, err = config()
	require.NoError(t, err)
	require.Nil(t, cfg.League)
}

func TestLeagueModeIsOnWithTheTokenAndSchmetterpause(t *testing.T) {
	t.Setenv("ZAEHLWERK_TOKEN", "matches-secret")
	t.Setenv("SCHMETTERPAUSE_URL", "http://sp.invalid")
	t.Setenv("SCHMETTERPAUSE_TOKEN", "sp-secret")
	t.Setenv("PONG_PLAYER_SKILL", "Ada=schwer,Bert=0.2")
	t.Setenv("PONG_OPERATOR", "")
	t.Setenv("PONG_LEAGUE_PAUSE", "")
	cfg, err := config()
	require.NoError(t, err)
	require.NotNil(t, cfg.League)
	require.Equal(t, "matches-secret", cfg.League.Token)
	require.Equal(t, "Pong", cfg.League.Operator, "the sandbox seed's observer")
	require.Equal(t, 30*time.Second, cfg.League.Pause, "outlasts zaehlwerk's 15s grace period")
	require.Equal(t, "schwer", cfg.League.Skills["Ada"].Name)
	require.Equal(t, 0.2, cfg.League.Skills["Bert"].Miss)
	require.False(t, cfg.League.Tournaments, "tournaments wait for PONG_TOURNAMENTS")
}

func TestPongTournamentsIsOnOrOffAndNothingElse(t *testing.T) {
	t.Setenv("ZAEHLWERK_TOKEN", "matches-secret")
	t.Setenv("SCHMETTERPAUSE_URL", "http://sp.invalid")
	t.Setenv("SCHMETTERPAUSE_TOKEN", "sp-secret")

	for raw, want := range map[string]bool{"on": true, "off": false, "": false} {
		t.Setenv("PONG_TOURNAMENTS", raw)
		cfg, err := config()
		require.NoError(t, err)
		require.Equal(t, want, cfg.League.Tournaments, "PONG_TOURNAMENTS=%q", raw)
	}

	t.Setenv("PONG_TOURNAMENTS", "true")
	_, err := config()
	require.ErrorContains(t, err, `PONG_TOURNAMENTS "true": want on or off`)
}

func TestLeagueModeNextToAttractModeIsRefusedAtStartup(t *testing.T) {
	t.Setenv("ZAEHLWERK_TOKEN", "matches-secret")
	t.Setenv("SCHMETTERPAUSE_URL", "http://sp.invalid")
	t.Setenv("SCHMETTERPAUSE_TOKEN", "sp-secret")
	t.Setenv("PONG_ATTRACT_AFTER", "10m")
	_, err := config()
	require.ErrorContains(t, err, "league mode and attract mode both start matches by themselves")
}

func TestABadPlayerSkillIsRefusedAtStartup(t *testing.T) {
	t.Setenv("ZAEHLWERK_TOKEN", "matches-secret")
	t.Setenv("SCHMETTERPAUSE_URL", "http://sp.invalid")
	t.Setenv("SCHMETTERPAUSE_TOKEN", "sp-secret")
	t.Setenv("PONG_PLAYER_SKILL", "Ada=profi")
	_, err := config()
	require.ErrorContains(t, err, "PONG_PLAYER_SKILL")
}

func TestPongCourtSpeedIsAFactorFromOneToTen(t *testing.T) {
	for raw, want := range map[string]float64{"": 0, "1": 1, "3": 3, "2.5": 2.5, "10": 10} {
		t.Setenv("PONG_COURT_SPEED", raw)
		cfg, err := config()
		require.NoError(t, err, "PONG_COURT_SPEED=%q", raw)
		require.Equal(t, want, cfg.CourtSpeed, "PONG_COURT_SPEED=%q", raw)
	}
	for _, raw := range []string{"0", "0.5", "11", "fast"} {
		t.Setenv("PONG_COURT_SPEED", raw)
		_, err := config()
		require.ErrorContains(t, err, "PONG_COURT_SPEED", raw)
	}
}
