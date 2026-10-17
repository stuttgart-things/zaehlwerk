package pong

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// Driver decides what happens at the table: which half presses ready, and who
// wins a rally. It decides nothing else. Phase, ends, waiting and serving come
// from zaehlwerk, and the faults, the truth and the reports are the game's, the
// same for every driver.
type Driver interface {
	// Ready is the half that presses next in the warm-up or a change of ends.
	Ready(ctx context.Context, st scorer.State) (scorer.Half, error)
	// Rally is the player who won the next rally: the one for the serve in
	// serve_rally, a point in playing. standing is where the game has the
	// players.
	Rally(ctx context.Context, st scorer.State, standing [2]scorer.Player) (scorer.Player, error)
	// Over is called once, after the game has stopped, with the last state.
	Over(st scorer.State)
}

// Drivers, as POST /games and GET /games/{id} name them.
const (
	DriverBots    = "bots"
	DriverBrowser = "browser"
	// DriverServer is two bots on the court the service plays itself
	// (ADR-0014): no page decides anything, and every page watches.
	DriverServer = "server"
)

// bots is the headless driver: two bots whose rallies are drawn by skill.
type bots struct {
	rnd    *rand.Rand
	skills [2]Skill
}

func (b *bots) Ready(_ context.Context, st scorer.State) (scorer.Half, error) {
	if len(st.Waiting) == 0 {
		return "", fmt.Errorf("zaehlwerk is in %s and waits for nobody", st.Phase)
	}
	return st.Waiting[0], nil
}

func (b *bots) Rally(_ context.Context, st scorer.State, _ [2]scorer.Player) (scorer.Player, error) {
	if st.Phase == scorer.PhaseServeRally {
		// Nobody serves the serve rally, so who hits first is drawn too.
		server := b.rnd.Intn(2)
		return playerAt(Rally(b.rnd, b.skills, server)), nil
	}
	return playerAt(Rally(b.rnd, b.skills, index(st.Serving))), nil
}

func (b *bots) Over(scorer.State) {}

// noDriver is a button check's: nobody in the game decides anything.
type noDriver struct{}

var errNoDriver = errors.New("a button check decides no rally")

func (noDriver) Ready(context.Context, scorer.State) (scorer.Half, error) { return "", errNoDriver }
func (noDriver) Rally(context.Context, scorer.State, [2]scorer.Player) (scorer.Player, error) {
	return "", errNoDriver
}
func (noDriver) Over(scorer.State) {}

// PressKind is what a browser reports.
type PressKind string

const (
	// PressReady is a half's ready key in the warm-up or a change of ends.
	PressReady PressKind = "ready"
	// PressRally is a rally won, by the player at Half.
	PressRally PressKind = "rally"
)

// Press is POST /games/{id}/presses: one thing that happened in the browser.
type Press struct {
	// Rally numbers the browser's presses, from 1 and without gaps. The same
	// number again is a retry and is answered without a second press.
	Rally uint64      `json:"rally"`
	Kind  PressKind   `json:"kind"`
	Half  scorer.Half `json:"half"`
}

func (p Press) validate() error {
	switch {
	case p.Rally == 0:
		return errors.New("rally: want a number from 1")
	case p.Kind != PressReady && p.Kind != PressRally:
		return fmt.Errorf("kind %q: want ready or rally", p.Kind)
	case p.Half != scorer.HalfLeft && p.Half != scorer.HalfRight:
		return fmt.Errorf("half %q: want left or right", p.Half)
	}
	return nil
}

var (
	// errOutOfPhase is a press that does not fit where zaehlwerk is: a rally
	// sent while it waits for confirmations, or the other way round. The
	// browser was behind; it gets the state and catches up.
	errOutOfPhase = errors.New("the press does not fit the phase")
	// errOutOfOrder is a press number that skips one.
	errOutOfOrder = errors.New("the press number skips one")
	// errIdle is a browser game nobody has pressed in for too long.
	errIdle = errors.New("nobody pressed for too long")
)

// browserIdle is how long a browser game waits for a press before it ends its
// match. A tab closed in the middle of a match would otherwise keep it current
// on the panel, with nobody playing, until the service restarts.
const browserIdle = 10 * time.Minute

type pressAnswer struct {
	state scorer.State
	err   error
}

type pressRequest struct {
	press  Press
	answer chan pressAnswer
}

// browser is the driver for a game played in the browser: the page decides
// every rally and reports it, and the game turns it into presses on zaehlwerk.
//
// The answer to a press is sent when the game asks for the next one, not when
// the press itself was reported. By then whatever the game does between two
// rallies has happened too — a fault injected, a change of ends walked — and
// the state the browser gets back is the one its next rally starts from.
type browser struct {
	presses chan pressRequest
	idle    time.Duration

	// pending is the press the game is working on, answered on the next ask.
	// Only the goroutine that plays touches it.
	pending *pressRequest

	// mu orders the presses: one at a time, so a retry arriving while the
	// first is still on its way waits for it and is then a duplicate. last is
	// the number of the last press taken; atomic, because a view is read
	// while a press holds mu for as long as a counter reset waits.
	mu   sync.Mutex
	last atomic.Uint64
}

func newBrowser(idle time.Duration) *browser {
	if idle <= 0 {
		idle = browserIdle
	}
	return &browser{presses: make(chan pressRequest), idle: idle}
}

func (b *browser) Ready(ctx context.Context, st scorer.State) (scorer.Half, error) {
	p, err := b.next(ctx, st, PressReady)
	return p.Half, err
}

func (b *browser) Rally(ctx context.Context, st scorer.State, standing [2]scorer.Player) (scorer.Player, error) {
	p, err := b.next(ctx, st, PressRally)
	if err != nil {
		return "", err
	}
	// The page reports the half the winner stands at. Who stands there is
	// where the game has the players, so a browser drawing them where
	// zaehlwerk says and zaehlwerk resolving the half agree, or the ends
	// check says why not.
	return standing[halfIndex(p.Half)], nil
}

func (b *browser) Over(st scorer.State) { b.answer(st, nil) }

// next answers the press before, then waits for one of the kind the phase
// wants. A press of the other kind is answered at once and waited past.
func (b *browser) next(ctx context.Context, st scorer.State, want PressKind) (Press, error) {
	b.answer(st, nil)

	idle := time.NewTimer(b.idle)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return Press{}, ctx.Err()
		case <-idle.C:
			return Press{}, fmt.Errorf("%w: %s", errIdle, b.idle)
		case req := <-b.presses:
			if req.press.Kind != want {
				req.answer <- pressAnswer{state: st,
					err: fmt.Errorf("%w: a %s press while zaehlwerk is in %s", errOutOfPhase, req.press.Kind, st.Phase)}
				continue
			}
			b.pending = &req
			return req.press, nil
		}
	}
}

func (b *browser) answer(st scorer.State, err error) {
	if b.pending == nil {
		return
	}
	b.pending.answer <- pressAnswer{state: st, err: err}
	b.pending = nil
}

// submit hands a press to the game and waits for its answer. duplicate is a
// press number already taken, answered with nothing sent.
func (b *browser) submit(ctx context.Context, done <-chan struct{}, p Press) (duplicate bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	last := b.last.Load()
	switch {
	case p.Rally <= last:
		return true, nil
	case p.Rally != last+1:
		return false, fmt.Errorf("%w: press %d, want %d", errOutOfOrder, p.Rally, last+1)
	}

	req := pressRequest{press: p, answer: make(chan pressAnswer, 1)}
	select {
	case b.presses <- req:
	case <-done:
		return false, ErrMatchOver
	case <-ctx.Done():
		return false, ctx.Err()
	}

	// Once handed over the press is the game's: Over answers it if the game
	// stops first, so this waits for the answer and not for the request.
	a := <-req.answer
	if a.err != nil {
		return false, a.err
	}
	b.last.Store(p.Rally)
	return false, nil
}
