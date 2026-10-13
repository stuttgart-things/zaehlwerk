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
		"version", cfg.Build.Version)

	game := pong.New(cfg, log)
	srv := &http.Server{
		Addr:              env("HTTP_ADDR", defaultAddr),
		Handler:           game,
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
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

	cfg.Seed = time.Now().UnixNano()
	if raw := os.Getenv("PONG_SEED"); raw != "" {
		if cfg.Seed, err = strconv.ParseInt(raw, 10, 64); err != nil {
			return cfg, fmt.Errorf("PONG_SEED %q: want a whole number: %w", raw, err)
		}
	}
	return cfg, nil
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
