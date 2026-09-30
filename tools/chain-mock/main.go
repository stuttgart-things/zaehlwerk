// Command chain-mock stands in for both ends of the chain this service sits in
// the middle of, so piezo → zaehlwerk → Schmetterpause runs on a laptop with no
// board and no Schmetterpause (issue #55).
//
//	chain-mock schmetterpause   the three routes zaehlwerk calls, in memory
//	chain-mock piezo            a board that joins the running match and scores
//
// A development tool, not part of the service: nothing here is deployed, and
// cmd/zaehlwerk-api stays the only place the service reads its environment.
// It is configured the same way anyway — environment variables with defaults,
// no flags — so the Taskfile sets one thing for both sides.
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
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: chain-mock schmetterpause | piezo")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "schmetterpause":
		err = runSchmetterpause(ctx, log.With("part", "schmetterpause"))
	case "piezo":
		err = runPiezo(ctx, log.With("part", "piezo"))
	default:
		err = fmt.Errorf("unknown part %q: want schmetterpause or piezo", os.Args[1])
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("stopped", "error", err)
		os.Exit(1)
	}
}

func runSchmetterpause(ctx context.Context, log *slog.Logger) error {
	token := os.Getenv("SCHMETTERPAUSE_TOKEN")
	fake, err := newFakeSchmetterpause(token, env("SCHMETTERPAUSE_MODE", modeAccept), log)
	if err != nil {
		return fmt.Errorf("SCHMETTERPAUSE_MODE: %w", err)
	}
	if token == "" {
		log.Warn("SCHMETTERPAUSE_TOKEN not set, so /api does not exist — as with the real one")
	}

	srv := &http.Server{
		Addr:              env("SCHMETTERPAUSE_ADDR", ":8082"),
		Handler:           fake.handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	log.Info("listening", "addr", srv.Addr, "mode", fake.currentMode())
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving: %w", err)
	}
	return nil
}

func runPiezo(ctx context.Context, log *slog.Logger) error {
	cfg := boardConfig{
		API:    strings.TrimRight(env("ZAEHLWERK_URL", "http://localhost:8080"), "/"),
		Source: env("PIEZO_SOURCE", "piezo-mock"),
	}

	var err error
	if cfg.Pace, err = time.ParseDuration(env("PIEZO_PACE", "1s")); err != nil {
		return fmt.Errorf("PIEZO_PACE: %w", err)
	}
	if cfg.Ambiguous, err = share("PIEZO_AMBIGUOUS", "0.1"); err != nil {
		return err
	}
	if cfg.Resend, err = share("PIEZO_RESEND", "0.1"); err != nil {
		return err
	}
	if cfg.Undo, err = share("PIEZO_UNDO", "0.05"); err != nil {
		return err
	}
	if cfg.Seed, err = strconv.ParseInt(env("PIEZO_SEED", "1"), 10, 64); err != nil {
		return fmt.Errorf("PIEZO_SEED: %w", err)
	}
	if cfg.Matches, err = strconv.Atoi(env("PIEZO_MATCHES", "1")); err != nil {
		return fmt.Errorf("PIEZO_MATCHES: %w", err)
	}

	// PIEZO_CREATE=home,away,operator starts the match through the page
	// rather than waiting for somebody to. Unset is the board's real
	// behaviour: it never creates a match, it joins one.
	if raw := os.Getenv("PIEZO_CREATE"); raw != "" {
		ids := strings.Split(raw, ",")
		if len(ids) != 3 {
			return fmt.Errorf("PIEZO_CREATE %q: want home,away,operator", raw)
		}
		bestOf, err := strconv.Atoi(env("PIEZO_BEST_OF", "3"))
		if err != nil {
			return fmt.Errorf("PIEZO_BEST_OF: %w", err)
		}
		cfg.Create = &createForm{HomeID: ids[0], AwayID: ids[1], OperatorID: ids[2], BestOf: bestOf}
	}

	log.Info("board up", "api", cfg.API, "source", cfg.Source, "pace", cfg.Pace,
		"creates_match", cfg.Create != nil)
	return newBoard(cfg, log).run(ctx)
}

func share(key, fallback string) (float64, error) {
	v, err := strconv.ParseFloat(env(key, fallback), 64)
	if err != nil || v < 0 || v > 1 {
		return 0, fmt.Errorf("%s: want a share between 0 and 1", key)
	}
	return v, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
