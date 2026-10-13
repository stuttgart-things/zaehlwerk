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
		"browser_idle", cfg.BrowserIdle.String(), "version", cfg.Build.Version)

	if cfg.AttractAfter == 0 {
		log.Info("attract mode disabled, PONG_ATTRACT_AFTER not configured")
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

	cfg.Seed = time.Now().UnixNano()
	if raw := os.Getenv("PONG_SEED"); raw != "" {
		if cfg.Seed, err = strconv.ParseInt(raw, 10, 64); err != nil {
			return cfg, fmt.Errorf("PONG_SEED %q: want a whole number: %w", raw, err)
		}
	}
	return cfg, nil
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
