// Command portal is the dev portal: one page for the chain, with the parts'
// own pages in frames — the binary behind the
// ghcr.io/stuttgart-things/zaehlwerk-portal image.
//
// Its own entry point for the reason cmd/buttons is one: a deployment and its
// log name what is running. Configuration is the environment, as described in
// the README's dev portal section.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/stuttgart-things/zaehlwerk/tools/chain-mock/portal"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := portal.RunFromEnv(ctx, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("stopped", "error", err)
		os.Exit(1)
	}
}
