// Command piezo is the chain mock's board on its own: the binary behind the
// ghcr.io/stuttgart-things/zaehlwerk-piezo image.
//
// Its own entry point rather than `chain-mock piezo` on a shared image, so a
// deployment and its log name what is running. Configuration is the
// environment, as described in the README's chain section.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/stuttgart-things/zaehlwerk/tools/chain-mock/piezo"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := piezo.RunFromEnv(ctx, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("stopped", "error", err)
		os.Exit(1)
	}
}
