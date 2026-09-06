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
	"strings"
	"syscall"
	"time"

	homerun "github.com/stuttgart-things/homerun-library/v4"

	"github.com/stuttgart-things/zaehlwerk/internal/api"
	"github.com/stuttgart-things/zaehlwerk/internal/live"
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

	switcher := panelSwitcher(log)
	if switcher != nil {
		// Closed after the server, so no request is still starting a match
		// while the panel is being given back.
		defer func() {
			if err := switcher.Close(); err != nil {
				log.Warn("closing the panel switcher", "error", err)
			}
		}()
	}

	hub := live.New()
	origins := allowedOrigins(log)

	registry := match.NewRegistry(registryOptions(sink, hub, switcher)...)
	srv := &http.Server{
		Addr: env("HTTP_ADDR", defaultAddr),
		Handler: api.New(registry, append([]api.Option{
			api.WithLogger(log),
			api.WithHub(hub),
			api.WithAllowedOrigins(origins),
			api.WithPanelSwitcher(switcher),
		}, streamOptions(log)...)...),

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

func registryOptions(sink *panel.Sink, hub *live.Hub, switcher *panel.Switcher) []match.Option {
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
	if hub != nil {
		opts = append(opts, match.WithObserver(hub.Observe))
	}
	if switcher != nil {
		// For the inactivity clock, and to give the panel back when a match is
		// won without anyone calling /end.
		opts = append(opts, match.WithObserver(switcher.Observe))
	}
	return opts
}

// panelSwitcher builds the LED catcher stream switcher, or returns nil when no
// catcher is configured — a local run should not need one.
func panelSwitcher(log *slog.Logger) *panel.Switcher {
	addr := os.Getenv("CATCHER_URL")
	if addr == "" {
		log.Info("panel stream switching disabled, no CATCHER_URL configured")
		return nil
	}

	cfg := panel.StreamConfig{
		BaseURL:      strings.TrimRight(addr, "/"),
		MatchStreams: splitList(os.Getenv("CATCHER_MATCH_STREAMS")),
		IdleStreams:  splitList(os.Getenv("CATCHER_IDLE_STREAMS")),
		Logger:       log,
	}
	if raw := os.Getenv("CATCHER_IDLE_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			log.Warn("ignoring CATCHER_IDLE_TIMEOUT, not a positive duration", "value", raw)
		} else {
			cfg.IdleTimeout = d
		}
	}

	s := panel.NewSwitcher(cfg)
	log.Info("panel stream switching enabled", "catcher", cfg.BaseURL)
	return s
}

func splitList(raw string) []string {
	var out []string
	for _, v := range strings.Split(raw, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// allowedOrigins reads the browser origins

// streamOptions reads the live stream tuning that depends on what sits in
// front of the service. The default heartbeat suits a proxy that gives an idle
// connection a minute; one that is stricter needs a shorter interval, and
// there is no way to find that out from in here.
func streamOptions(log *slog.Logger) []api.Option {
	raw := os.Getenv("STREAM_HEARTBEAT")
	if raw == "" {
		return nil
	}

	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Warn("ignoring STREAM_HEARTBEAT, not a positive duration", "value", raw)
		return nil
	}
	log.Info("live stream heartbeat", "interval", d)
	return []api.Option{api.WithHeartbeat(d)}
}

// allowedOrigins reads the browser origins that may read the live stream.
//
// Empty means no cross-origin browser may read it, which is the right default
// for a service that is reachable on the office network: a same-origin page
// and anything server-side still work, and Schmetterpause is named explicitly
// when it is deployed.
func allowedOrigins(log *slog.Logger) []string {
	raw := os.Getenv("ALLOWED_ORIGINS")
	if raw == "" {
		log.Info("no ALLOWED_ORIGINS configured, cross-origin browsers cannot read the live stream")
		return nil
	}

	var origins []string
	for _, o := range strings.Split(raw, ",") {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, o)
		}
	}
	log.Info("live stream origins allowed", "origins", origins)
	return origins
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
