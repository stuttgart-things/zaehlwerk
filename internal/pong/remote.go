package pong

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// A game on two devices (ADR-0009): the host is the browser game of ADR-0006
// and the only reporter; the guest joins with a code, moves player b's paddle
// and presses ready, and reports nothing.

const (
	// joinCodeDigits is short enough to type at the table. Four digits are
	// enough to name the one game a service runs, not to keep anybody out,
	// which is what single use, the expiry and the lockout are for.
	joinCodeDigits = 4
	// joinCodeTTL is how long a code nobody has entered stays valid. Long
	// enough to walk to the second device, short enough that a code read off
	// a screen an hour ago no longer joins.
	joinCodeTTL = 5 * time.Minute
	// joinCodeTries is how many wrong codes lock the code: five chances in
	// 10,000 for whoever guesses, per code the host draws.
	joinCodeTries = 5
	// GuestTokenPrefix marks the guest's token in the fragment of /play/{id},
	// so the page knows which role it plays. The header carries the token
	// without it.
	GuestTokenPrefix = "guest:"
)

var (
	errNoGuestWanted = errors.New("no game is waiting for a guest")
	errWrongCode     = errors.New("the code does not match")
	errCodeLocked    = errors.New("the code is locked after too many wrong attempts")
	errCodeExpired   = errors.New("the code has expired")
	errJoined        = errors.New("a guest has already joined")
	errNotRemote     = errors.New("not a game on two devices")
)

// remote is what a game on two devices adds to a game: the code, the guest's
// token, and the latest message in each direction.
type remote struct {
	now func() time.Time

	mu     sync.Mutex
	code   string
	issued time.Time
	wrong  int
	// guest is the guest's token, empty until somebody joins. It stays valid
	// for the whole game, so a reload carries on.
	guest string
}

func newRemote(now func() time.Time) (*remote, error) {
	r := &remote{now: now}
	if err := r.redraw(); err != nil {
		return nil, err
	}
	return r, nil
}

// RemoteView is a game on two devices as GET /games/{id} shows it. The code is
// not in it: the view is public, and the code is the host's to show.
type RemoteView struct {
	Joined bool `json:"joined"`
	// Locked and ExpiresAt say whether the code can still join, so the
	// host's page offers a new one when it cannot.
	Locked    bool       `json:"locked,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func (r *remote) view() *RemoteView {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := &RemoteView{Joined: r.guest != ""}
	if !v.Joined {
		exp := r.issued.Add(joinCodeTTL)
		v.Locked, v.ExpiresAt = r.wrong >= joinCodeTries, &exp
	}
	return v
}

// Code is the answer to POST /games/{id}/code.
type Code struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

// draw gives the host a code that joins: the current one while it still
// does, a new one once it has expired or is locked.
func (r *remote) draw() (Code, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.guest != "" {
		return Code{}, errJoined
	}
	if r.wrong >= joinCodeTries || !r.now().Before(r.issued.Add(joinCodeTTL)) {
		if err := r.redraw(); err != nil {
			return Code{}, err
		}
	}
	return Code{Code: r.code, ExpiresAt: r.issued.Add(joinCodeTTL)}, nil
}

// redraw draws a new code and starts its clock and its count of wrong tries.
// Called with r.mu held, or before r is shared.
func (r *remote) redraw() error {
	n, err := rand.Int(rand.Reader, big.NewInt(10_000))
	if err != nil {
		return fmt.Errorf("drawing a join code: %w", err)
	}
	r.code, r.issued, r.wrong = fmt.Sprintf("%0*d", joinCodeDigits, n.Int64()), r.now(), 0
	return nil
}

// join takes a code from /join and hands out the guest's token. The code is
// used up by the first correct entry, and after that no code joins.
func (r *remote) join(code string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.guest != "":
		return "", errJoined
	case r.wrong >= joinCodeTries:
		return "", errCodeLocked
	case !r.now().Before(r.issued.Add(joinCodeTTL)):
		return "", errCodeExpired
	case subtle.ConstantTimeCompare([]byte(code), []byte(r.code)) != 1:
		r.wrong++
		if r.wrong >= joinCodeTries {
			return "", errCodeLocked
		}
		return "", errWrongCode
	}
	token, err := newToken()
	if err != nil {
		return "", err
	}
	r.guest, r.code = token, ""
	return token, nil
}

func (r *remote) isGuest(token string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.guest != "" && subtle.ConstantTimeCompare([]byte(token), []byte(r.guest)) == 1
}

// remoteGame is a game on two devices that is still playing, or the error
// that says why the request has nothing to do with it.
func remoteGame(g *Game) (*remote, int, error) {
	switch {
	case g.remote == nil:
		return nil, http.StatusConflict, errNotRemote
	case isDone(g):
		return nil, http.StatusConflict, ErrMatchOver
	}
	return g.remote, 0, nil
}

func isDone(g *Game) bool {
	select {
	case <-g.Done():
		return true
	default:
		return false
	}
}

// drawCode is POST /games/{id}/code: the host's page asks for a code to show,
// the current one or a new one when it can no longer join.
func (s *Server) drawCode(w http.ResponseWriter, r *http.Request) {
	g, ok := s.tokenGame(w, r)
	if !ok {
		return
	}
	rm, status, err := remoteGame(g)
	if err != nil {
		fail(w, status, err)
		return
	}
	c, err := rm.draw()
	switch {
	case errors.Is(err, errJoined):
		fail(w, http.StatusConflict, err)
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, err)
		return
	}
	g.touch()
	writeJSON(w, http.StatusOK, c)
}

// join finds the game waiting for a guest and lets the code decide.
func (s *Server) join(code string) (*Game, string, error) {
	s.mu.Lock()
	g := s.running
	s.mu.Unlock()
	if g == nil || g.remote == nil || isDone(g) {
		return nil, "", errNoGuestWanted
	}
	token, err := g.remote.join(code)
	// A wrong try changes the view too: the host's page shows a locked code.
	g.touch()
	if err != nil {
		return nil, "", err
	}
	g.log.Info("a guest joined")
	return g, token, nil
}
