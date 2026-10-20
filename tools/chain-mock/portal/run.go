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
//
// In a cluster the defaults are wrong for every part, so a deployment names
// each one: <PART>_URL as the portal reaches it, <PART>_PUBLIC_URL as a
// browser does, and "off" for a part the cluster does not run.
func RunFromEnv(ctx context.Context, log *slog.Logger) error {
	t := Targets{
		Zaehlwerk:      part("ZAEHLWERK", "http://localhost:8080"),
		Simulator:      part("SIMULATOR", "http://localhost:8081"),
		Schmetterpause: part("SCHMETTERPAUSE", "http://localhost:8082"),
		Buttons:        part("BUTTONS", "http://localhost:8084"),
		Piezo:          part("PIEZO_CONTROL", "http://localhost:8085"),
		Pong:           part("PONG", "http://localhost:8087"),
	}
	if !t.Zaehlwerk.On() {
		return errors.New("ZAEHLWERK_URL: the portal is zaehlwerk's, it cannot be off")
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

// part reads <name>_URL and <name>_PUBLIC_URL. "off" rather than empty
// switches a part off: an unset variable has to keep meaning the laptop
// default, and a Flux substitution cannot produce an empty value anyway.
func part(name, fallback string) Part {
	u := baseURL(name+"_URL", fallback)
	if u == "off" {
		return Part{}
	}
	return Part{URL: u, Public: baseURL(name+"_PUBLIC_URL", "")}
}

func baseURL(key, fallback string) string { return strings.TrimRight(env(key, fallback), "/") }

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
