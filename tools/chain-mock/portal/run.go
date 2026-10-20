package portal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// RunFromEnv serves the portal until ctx ends. Every URL defaults to the port
// the Taskfile gives that part, so the portal on its own finds a chain the
// Taskfile started; the Taskfile still sets them, so a moved port moves here
// too.
func RunFromEnv(ctx context.Context, log *slog.Logger) error {
	t := Targets{
		Zaehlwerk:      baseURL("ZAEHLWERK_URL", "http://localhost:8080"),
		Simulator:      baseURL("SIMULATOR_URL", "http://localhost:8081"),
		Schmetterpause: baseURL("SCHMETTERPAUSE_URL", "http://localhost:8082"),
		Buttons:        baseURL("BUTTONS_URL", "http://localhost:8084"),
		Piezo:          baseURL("PIEZO_CONTROL_URL", "http://localhost:8085"),
		Pong:           baseURL("PONG_URL", "http://localhost:8087"),
	}
	srv := &http.Server{
		Addr:              env("PORTAL_ADDR", ":8086"),
		Handler:           New(t, log),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Info("portal up", "addr", srv.Addr, "zaehlwerk", t.Zaehlwerk, "simulator", t.Simulator,
		"schmetterpause", t.Schmetterpause, "buttons", t.Buttons, "piezo", t.Piezo, "pong", t.Pong)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving the portal: %w", err)
	}
	return ctx.Err()
}

func baseURL(key, fallback string) string { return strings.TrimRight(env(key, fallback), "/") }

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
