package pong

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"slices"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// servedTick paces the court at the page's 60 frames a second, so a served
// game is as fast to watch as one in the browser.
const servedTick = time.Second / 60

// MaxCourtSpeed bounds Config.CourtSpeed. Ten times is a tick every 1.7 ms,
// which is about as fast as a page following the relay still draws.
const MaxCourtSpeed = 10

// courtTickFor is the tick a court runs at speed times real time.
func courtTickFor(speed float64) time.Duration {
	if speed <= 1 {
		return servedTick
	}
	return time.Duration(float64(servedTick) / min(speed, MaxCourtSpeed))
}

// served is the driver of a game the service plays itself (ADR-0014): two bots
// on the court of web/game.js, ported to Go. It plays only while the game asks
// it for the next press, as the page plays on while its press is answered, and
// it posts every second tick to the relay, which every tab watching draws.
type served struct {
	court *court
	relay *relayBox
	// every is the pause between two ticks; zero runs the court as fast as it
	// goes, which only the tests do.
	every time.Duration
}

func newServed(seed int64, bots [2]*BotParams, relay *relayBox, every time.Duration) *served {
	var params [2]BotParams
	for i, b := range bots {
		if b != nil {
			params[i] = *b
		}
	}
	return &served{court: newCourt(rand.New(rand.NewSource(seed)), params), relay: relay, every: every}
}

func (d *served) Ready(ctx context.Context, st scorer.State) (scorer.Half, error) {
	p, err := d.next(ctx, st, PressReady)
	if err != nil {
		return "", err
	}
	return halfAt(p.half), nil
}

func (d *served) Rally(ctx context.Context, st scorer.State, standing [2]scorer.Player) (scorer.Player, error) {
	p, err := d.next(ctx, st, PressRally)
	if err != nil {
		return "", err
	}
	// As the browser's: the court reports the half the winner stands at, and
	// who stands there is where the game has the players.
	return standing[p.half], nil
}

// Over shows the last state once more: the ball off the table, the match over.
func (d *served) Over(st scorer.State) {
	d.court.apply(st)
	d.court.ball, d.court.resting = nil, -1
	_ = d.publish()
}

// next takes zaehlwerk's answer and plays until the court decides a press of
// the kind the phase wants. The court can decide two at once — both bots ready
// in the same tick — so one may wait here for the next ask; one that no
// longer fits what zaehlwerk says is dropped, as zaehlwerk would ignore it.
//
// The court stands still between two asks, while the game reports. That is
// milliseconds, except for a counter reset's quiet minute, through which the
// watching tabs show the table waiting.
func (d *served) next(ctx context.Context, st scorer.State, want PressKind) (courtPress, error) {
	c := d.court
	c.apply(st)
	var tick <-chan time.Time
	if d.every > 0 {
		t := time.NewTicker(d.every)
		defer t.Stop()
		tick = t.C
	}
	for {
		for len(c.presses) > 0 {
			p := c.presses[0]
			c.presses = c.presses[1:]
			if p.kind == want && (want != PressReady || slices.Contains(st.Waiting, halfAt(p.half))) {
				return p, nil
			}
		}
		if tick != nil {
			select {
			case <-ctx.Done():
				return courtPress{}, ctx.Err()
			case <-tick:
			}
		} else if err := ctx.Err(); err != nil {
			return courtPress{}, err
		}
		c.update()
		if c.tick%2 == 0 {
			if err := d.publish(); err != nil {
				return courtPress{}, err
			}
		}
	}
}

// publish posts the court to the relay. A frame is floats and short strings,
// and fails to marshal only on a NaN, which is a bug in the court: the game
// then fails and says so, rather than drawing nothing.
func (d *served) publish() error {
	raw, err := json.Marshal(d.court.frame())
	if err != nil {
		return fmt.Errorf("marshalling a court frame: %w", err)
	}
	d.relay.put(true, raw)
	return nil
}
