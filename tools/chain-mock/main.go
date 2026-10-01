// Command chain-mock stands in for both ends of the chain this service sits in
// the middle of, so piezo → zaehlwerk → Schmetterpause runs on a laptop with no
// board and no Schmetterpause (issue #55).
//
//	chain-mock schmetterpause   the three routes zaehlwerk calls, in memory
//	chain-mock piezo            a board that joins the running match and scores
//
// A development tool, not part of the service: cmd/zaehlwerk-api stays the
// only place the service reads its environment. It is configured the same way
// anyway — environment variables with defaults, no flags — so the Taskfile
// sets one thing for both sides. The board also ships on its own, as
// cmd/piezo, for running it where there is no Taskfile.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/stuttgart-things/zaehlwerk/tools/chain-mock/fakesp"
	"github.com/stuttgart-things/zaehlwerk/tools/chain-mock/piezo"
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
		err = fakesp.RunFromEnv(ctx, log.With("part", "schmetterpause"))
	case "piezo":
		err = piezo.RunFromEnv(ctx, log.With("part", "piezo"))
	default:
		err = fmt.Errorf("unknown part %q: want schmetterpause or piezo", os.Args[1])
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("stopped", "error", err)
		os.Exit(1)
	}
}
