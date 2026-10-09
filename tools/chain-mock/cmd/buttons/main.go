// Command buttons is the chain mock's two buttons and their hub, with a page to
// press them on: the binary behind the ghcr.io/stuttgart-things/zaehlwerk-buttons
// image.
//
// Its own entry point for the reason cmd/piezo is one: a deployment and its log
// name what is running. Configuration is the environment, as described in the
// README's chain section.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/stuttgart-things/zaehlwerk/tools/chain-mock/buttons"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := buttons.RunFromEnv(ctx, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("stopped", "error", err)
		os.Exit(1)
	}
}
