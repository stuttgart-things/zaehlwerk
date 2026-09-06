// Command zaehlwerk-api serves the ingest and match lifecycle endpoints.
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

	homerun "github.com/stuttgart-things/homerun-library/v4"

	"github.com/stuttgart-things/zaehlwerk/internal/api"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/panel"
)

const (
	defaultAddr      = ":8080"
	defaultRedisPort = "6379"
	shutdownTimeout  = 10 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("zaehlwerk-api stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(log)

	sink := panelSink(log)
	if sink != nil {
		// Closed after the server, not with a defer here: a point still being
		// handled when SIGTERM arrives should reach the panel, and the drain
		// only works if nothing is left writing to the scorer.
		defer func() {
			if err := sink.Close(); err != nil {
				log.Warn("closing the panel sink", "error", err)
			}
		}()
	}

	registry := match.NewRegistry(registryOptions(sink)...)
	srv := &http.Server{
		Addr:    env("HTTP_ADDR", defaultAddr),
		Handler: api.New(registry, api.WithLogger(log)),

		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		// Deliberately no WriteTimeout: the SSE endpoint holds a response open
		// for the length of a match, and a write deadline would cut it off.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdown); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}
	return <-errs
}

func registryOptions(sink *panel.Sink) []match.Option {
	var opts []match.Option

	if raw := os.Getenv("MAX_RETAINED_MATCHES"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			opts = append(opts, match.WithMaxRetained(n))
		} else {
			slog.Warn("ignoring MAX_RETAINED_MATCHES, not a positive number", "value", raw)
		}
	}
	if sink != nil {
		opts = append(opts, match.WithObserver(sink.Observe))
	}
	return opts
}

// panelSink builds the LED panel sink, or returns nil when no Redis is
// configured. A local run should not need one: without REDIS_ADDR the score
// simply never leaves the API, which is the right default for developing
// against the endpoints alone.
func panelSink(log *slog.Logger) *panel.Sink {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		log.Info("panel disabled, no REDIS_ADDR configured")
		return nil
	}

	rc := homerun.RedisConfig{
		Addr:     addr,
		Port:     env("REDIS_PORT", defaultRedisPort),
		Password: os.Getenv("REDIS_PASSWORD"),
		Stream:   env("PANEL_STREAM", panel.DefaultStream),
	}
	log.Info("panel enabled", "redis", rc.Addr+":"+rc.Port, "stream", rc.Stream)

	// One pitcher for the process lifetime. NewPitcher opens a connection pool,
	// so the per-message form would open and leak one per point.
	return panel.New(homerun.NewPitcher(rc), panel.Config{
		Stream: rc.Stream,
		Logger: log,
	})
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
