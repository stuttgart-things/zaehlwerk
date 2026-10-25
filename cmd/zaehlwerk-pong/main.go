// Command zaehlwerk-pong is a table tennis game that plays whole matches
// against a running zaehlwerk and scores them through the real ingest path
// (ADR-0006).
//
// It is configured the way zaehlwerk is — environment variables with defaults
// in the code, no flags, no file — and this is the only place that reads them.
// The table in the README's zaehlwerk-pong section is the whole surface.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/pong"
	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
)

const (
	// Not :8080, so it runs next to a local zaehlwerk without either being
	// configured.
	defaultAddr      = ":8081"
	defaultZaehlwerk = "http://localhost:8080"
	defaultBestOf    = 3
	shutdownTimeout  = 10 * time.Second
	// One report a second can be followed on the panel: each point stays up
	// until the next replaces it.
	defaultAttractPace = time.Second
	defaultBrowserIdle = 10 * time.Minute
	// Ten seconds to find the button the page names and press it, at a
	// table somebody is setting up.
	defaultCheckTimeout = 10 * time.Second
	// The observer the sandbox's seed in Schmetterpause creates
	// (Schmetterpause ADR-0027).
	defaultLeagueOperator = "Pong"
	// Twice zaehlwerk's default SCHMETTERPAUSE_REPORT_DELAY of 15s: the last
	// league result is out before the next match starts.
	defaultLeaguePause = 30 * time.Second
)

// Set by the linker at build time; see .ko.yaml. Left empty here so /healthz
// says dev for a build that stamps nothing, as zaehlwerk-api does.
var (
	version string
	commit  string
	date    string
)

func main() {
	if err := run(); err != nil {
		slog.Error("zaehlwerk-pong stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(log)

	cfg, err := config()
	if err != nil {
		return err
	}
	// The seed is logged because a time-drawn one is otherwise lost, and it
	// is the one thing needed to play a run again.
	log.Info("zaehlwerk-pong up", "zaehlwerk", cfg.ZaehlwerkURL, "best_of", cfg.BestOf,
		"skills", []string{cfg.Skills[0].Name, cfg.Skills[1].Name}, "faults", cfg.Faults, "seed", cfg.Seed,
		"attract_after", cfg.AttractAfter.String(), "attract_pace", cfg.AttractPace.String(),
		"browser_idle", cfg.BrowserIdle.String(), "check_timeout", cfg.CheckTimeout.String(),
		"court_speed", max(cfg.CourtSpeed, 1), "version", cfg.Build.Version)

	if cfg.AttractAfter == 0 {
		log.Info("attract mode disabled, PONG_ATTRACT_AFTER not configured")
	}
	switch {
	case cfg.League != nil:
		log.Info("league mode enabled, real matches between Schmetterpause's players",
			"operator", cfg.League.Operator, "pause", cfg.League.Pause.String())
	case os.Getenv("ZAEHLWERK_TOKEN") == "":
		log.Info("league mode disabled, ZAEHLWERK_TOKEN not configured")
	default:
		log.Info("league mode disabled, it needs SCHMETTERPAUSE_URL and SCHMETTERPAUSE_TOKEN for the players")
	}
	if cfg.Roster == nil {
		log.Info("the lobby offers no players, SCHMETTERPAUSE_URL not configured")
	} else {
		log.Info("the lobby offers Schmetterpause's players", "schmetterpause", os.Getenv("SCHMETTERPAUSE_URL"),
			"token", os.Getenv("SCHMETTERPAUSE_TOKEN") != "")
	}

	game := pong.New(cfg, log)
	game.Start()
	srv := &http.Server{
		Addr:              env("HTTP_ADDR", defaultAddr),
		Handler:           game,
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		// Deliberately no WriteTimeout, as in zaehlwerk-api: a page's stream
		// stays open for the whole game, and a press is answered only after
		// a counter reset's quiet minute. Writes to a stream are bounded one
		// at a time instead.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serving: %w", err)
		}
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("shutting down the listener", "error", err)
	}
	// After the listener, so no request starts a game while the running one
	// is ending its match.
	if err := game.Close(shutdownCtx); err != nil {
		log.Warn("ending the running game", "error", err)
	}
	return nil
}

func config() (pong.Config, error) {
	cfg := pong.Config{
		ZaehlwerkURL: env("ZAEHLWERK_URL", defaultZaehlwerk),
		BestOf:       defaultBestOf,
		Build:        buildInfo(),
	}

	if raw := os.Getenv("PONG_BEST_OF"); raw != "" {
		n, err := strconv.Atoi(raw)
		// zaehlwerk would refuse it too, but only when the first game
		// starts. Said at startup, it is a typo; said at the first game,
		// it is a broken demo.
		if err != nil || n < 1 || n%2 == 0 {
			return cfg, fmt.Errorf("PONG_BEST_OF %q: want an odd number, 1 or more", raw)
		}
		cfg.BestOf = n
	}

	skills, err := pong.ParseSkills(env("PONG_BOT_SKILL", pong.DefaultSkill))
	if err != nil {
		return cfg, fmt.Errorf("PONG_BOT_SKILL: %w", err)
	}
	cfg.Skills = skills

	faults, err := pong.ParseFaults(os.Getenv("PONG_FAULTS"))
	if err != nil {
		return cfg, fmt.Errorf("PONG_FAULTS: %w", err)
	}
	cfg.Faults = faults

	if cfg.AttractAfter, err = duration("PONG_ATTRACT_AFTER", 0); err != nil {
		return cfg, err
	}
	if cfg.AttractPace, err = duration("PONG_ATTRACT_PACE", defaultAttractPace); err != nil {
		return cfg, err
	}
	if cfg.BrowserIdle, err = duration("PONG_BROWSER_IDLE", defaultBrowserIdle); err != nil {
		return cfg, err
	}
	if cfg.CheckTimeout, err = duration("PONG_CHECK_TIMEOUT", defaultCheckTimeout); err != nil {
		return cfg, err
	}
	if raw := os.Getenv("PONG_COURT_SPEED"); raw != "" {
		speed, err := strconv.ParseFloat(raw, 64)
		if err != nil || speed < 1 || speed > pong.MaxCourtSpeed {
			return cfg, fmt.Errorf("PONG_COURT_SPEED %q: want a number from 1 to %d", raw, pong.MaxCourtSpeed)
		}
		cfg.CourtSpeed = speed
	}

	// The lobby's names. The same two variables zaehlwerk reads, and the
	// same client: without a token Schmetterpause has no /api, and every
	// lobby load says so in the log while the fields stay free.
	if url := os.Getenv("SCHMETTERPAUSE_URL"); url != "" {
		client, err := schmetterpause.New(schmetterpause.Config{BaseURL: url, Token: os.Getenv("SCHMETTERPAUSE_TOKEN")})
		if err != nil {
			return cfg, fmt.Errorf("SCHMETTERPAUSE_URL: %w", err)
		}
		cfg.Roster = client
	}

	if cfg.League, err = league(cfg); err != nil {
		return cfg, err
	}

	cfg.Seed = time.Now().UnixNano()
	if raw := os.Getenv("PONG_SEED"); raw != "" {
		if cfg.Seed, err = strconv.ParseInt(raw, 10, 64); err != nil {
			return cfg, fmt.Errorf("PONG_SEED %q: want a whole number: %w", raw, err)
		}
	}
	return cfg, nil
}

// league reads league mode (ADR-0015): on with ZAEHLWERK_TOKEN, zaehlwerk's
// MATCHES_TOKEN, and with the players to draw from, SCHMETTERPAUSE_URL and
// SCHMETTERPAUSE_TOKEN. Without the token it is off and run says so, which is
// every deployment but the sandbox. A token without the players is off too:
// nothing it could start is wrong, so it is a log line, not a refusal.
//
// Next to attract mode it is refused: both start matches by themselves, and a
// sandbox that played demos between its league matches would put matches on
// the panel that are reported nowhere.
func league(cfg pong.Config) (*pong.League, error) {
	token := os.Getenv("ZAEHLWERK_TOKEN")
	if token == "" {
		return nil, nil
	}
	roster, ok := cfg.Roster.(pong.LeagueRoster)
	if !ok || os.Getenv("SCHMETTERPAUSE_TOKEN") == "" {
		return nil, nil
	}
	if cfg.AttractAfter > 0 {
		return nil, errors.New("ZAEHLWERK_TOKEN and PONG_ATTRACT_AFTER: league mode and attract mode both start matches by themselves, set one")
	}
	skills, err := pong.ParsePlayerSkills(os.Getenv("PONG_PLAYER_SKILL"))
	if err != nil {
		return nil, fmt.Errorf("PONG_PLAYER_SKILL: %w", err)
	}
	pause, err := duration("PONG_LEAGUE_PAUSE", defaultLeaguePause)
	if err != nil {
		return nil, err
	}
	tournaments, err := onOff("PONG_TOURNAMENTS")
	if err != nil {
		return nil, err
	}
	return &pong.League{
		Token:       token,
		Operator:    env("PONG_OPERATOR", defaultLeagueOperator),
		Skills:      skills,
		Pause:       pause,
		Roster:      roster,
		Tournaments: tournaments,
	}, nil
}

// onOff reads a switch that is off unless set to on. Anything else is refused
// rather than read as off: a typo in the sandbox's manifest would otherwise
// look like a tournament nobody plays.
func onOff(key string) (bool, error) {
	switch raw := os.Getenv(key); raw {
	case "", "off":
		return false, nil
	case "on":
		return true, nil
	default:
		return false, fmt.Errorf("%s %q: want on or off", key, raw)
	}
}

// duration reads a positive duration, or the fallback when the variable is
// unset. Zero is refused rather than taken: for the wait it would start a demo
// the moment the table is free, and for the pace or the idle time it means
// nothing anybody wants. Unset is how a duration is switched off.
func duration(key string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s %q: want a duration such as 10m or 1s", key, raw)
	}
	return d, nil
}

func buildInfo() pong.BuildInfo {
	b := pong.BuildInfo{Version: version, Commit: commit, Date: date}
	if b.Version == "" {
		b.Version = "dev"
	}
	if b.Commit == "" {
		b.Commit = "unknown"
	}
	if b.Date == "" {
		b.Date = "unknown"
	}
	return b
}

func logLevel() slog.Level {
	var level slog.Level
	if raw := os.Getenv("LOG_LEVEL"); raw != "" {
		if err := level.UnmarshalText([]byte(raw)); err != nil {
			slog.Warn("ignoring LOG_LEVEL, not a level", "value", raw)
		}
	}
	return level
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
