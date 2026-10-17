package pong

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// A game on two devices (ADR-0009): the host is the browser game of ADR-0006
// and the only reporter; the guest joins with a code, moves player b's paddle
// and presses ready, and reports nothing. The service relays between the two
// and reads none of it: the physics and the rules are the host's.

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
	// relayBodyCap bounds one relay message. A frame is about 160 bytes and a
	// paddle sample about 40; anything near this is not a message of either.
	relayBodyCap = 512
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
	errNoRelay       = errors.New("no relay: neither a game on two devices nor one the service plays")
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

// relayBox is the latest message in each direction of a game's relay: the
// table's frame, from the host of a game on two devices or from the service
// playing a game itself (ADR-0014), and the guest's sample.
type relayBox struct {
	mu sync.Mutex
	// frame and sample are the latest of each, as sent. Only the latest is
	// kept: a stream that falls behind skips to it rather than building up a
	// queue. seq counts each, so a stream sends a message once and the same
	// body sent twice still goes out twice.
	frame, sample       []byte
	frameSeq, sampleSeq uint64
	wake                chan struct{}
}

func newRelayBox() *relayBox { return &relayBox{wake: make(chan struct{})} }

func (b *relayBox) put(frame bool, raw []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if frame {
		b.frame = raw
		b.frameSeq++
	} else {
		b.sample = raw
		b.sampleSeq++
	}
	close(b.wake)
	b.wake = make(chan struct{})
}

// relayed is the relay as it stands: the latest of each, and a channel closed
// at the next message.
type relayed struct {
	frame, sample       []byte
	frameSeq, sampleSeq uint64
	wake                <-chan struct{}
}

func (b *relayBox) latest() relayed {
	b.mu.Lock()
	defer b.mu.Unlock()
	return relayed{frame: b.frame, sample: b.sample, frameSeq: b.frameSeq, sampleSeq: b.sampleSeq, wake: b.wake}
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

// relayFrame is what the host sends: where the ball and the paddles are, at
// its tick. The service checks its shape and nothing else.
type relayFrame struct {
	Tick    uint64     `json:"tick"`
	Ball    *relayBall `json:"ball"`
	Resting *int       `json:"resting"`
	Paddles [2]float64 `json:"paddles"`
	Say     string     `json:"say,omitempty"`
	Fx      []float64  `json:"fx,omitempty"`
}

type relayBall struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

// guestSample is what the guest sends: its paddle at the host tick it was
// drawing, and how often it has pressed ready, a count that only goes up so a
// sample replaced before it was sent loses no press.
type guestSample struct {
	Tick  uint64  `json:"tick"`
	Y     float64 `json:"y"`
	Ready uint64  `json:"ready"`
}

// paddle is POST /games/{id}/paddle, from the guest.
func (s *Server) paddle(w http.ResponseWriter, r *http.Request) {
	g, ok := s.game(r.PathValue("id"))
	if !ok {
		fail(w, http.StatusNotFound, fmt.Errorf("no game %q", r.PathValue("id")))
		return
	}
	rm, status, err := remoteGame(g)
	if err != nil {
		fail(w, status, err)
		return
	}
	if !rm.isGuest(r.Header.Get(TokenHeader)) {
		fail(w, http.StatusForbidden, errors.New("only the guest that joined moves its paddle"))
		return
	}
	raw, status, err := relayBody(w, r, &guestSample{})
	if err != nil {
		fail(w, status, err)
		return
	}
	g.relay.put(false, raw)
	w.WriteHeader(http.StatusNoContent)
}

// frames is POST /games/{id}/frames, from the host.
func (s *Server) frames(w http.ResponseWriter, r *http.Request) {
	g, ok := s.tokenGame(w, r)
	if !ok {
		return
	}
	// A served game's frames come from its driver only (ADR-0014): it is no
	// game on two devices, and remoteGame refuses it here.
	if _, status, err := remoteGame(g); err != nil {
		fail(w, status, err)
		return
	}
	raw, status, err := relayBody(w, r, &relayFrame{})
	if err != nil {
		fail(w, status, err)
		return
	}
	g.relay.put(true, raw)
	w.WriteHeader(http.StatusNoContent)
}

// relayBody reads one relay message: capped, of the expected shape, with no
// field the shape does not have. What goes on is the body as sent, on one
// line.
func relayBody(w http.ResponseWriter, r *http.Request, into any) ([]byte, int, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, relayBodyCap))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("a relay message is at most %d bytes", relayBodyCap)
		}
		return nil, http.StatusBadRequest, fmt.Errorf("reading the message: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("decoding the message: %w", err)
	}
	if dec.More() {
		return nil, http.StatusBadRequest, errors.New("decoding the message: more than one value")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("decoding the message: %w", err)
	}
	return compact.Bytes(), 0, nil
}

// relay is GET /games/{id}/relay: the latest frame of the table as `frame`
// and the latest guest sample as `guest`, each sent once, until the page goes.
// It takes no token, like /games/{id}/events: a third device may watch the
// ball, and every tab watches a served game (ADR-0014).
func (s *Server) relay(w http.ResponseWriter, r *http.Request) {
	g, ok := s.game(r.PathValue("id"))
	if !ok {
		fail(w, http.StatusNotFound, fmt.Errorf("no game %q", r.PathValue("id")))
		return
	}
	if g.relay == nil {
		fail(w, http.StatusConflict, errNoRelay)
		return
	}
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := writeRaw(rc, w, ": relay\n\n"); err != nil {
		return
	}

	heartbeat := time.NewTicker(s.heartbeat)
	defer heartbeat.Stop()
	var sentFrame, sentSample uint64
	for {
		now := g.relay.latest()
		if now.frameSeq != sentFrame {
			if err := writeEvent(r.Context(), rc, w, "frame", string(now.frame)); err != nil {
				return
			}
			sentFrame = now.frameSeq
		}
		if now.sampleSeq != sentSample {
			if err := writeEvent(r.Context(), rc, w, "guest", string(now.sample)); err != nil {
				return
			}
			sentSample = now.sampleSeq
		}
	wait:
		for {
			select {
			case <-r.Context().Done():
				return
			case <-s.ctx.Done():
				return
			case <-now.wake:
				break wait
			case <-heartbeat.C:
				if err := writeRaw(rc, w, ": ping\n\n"); err != nil {
					return
				}
			}
		}
	}
}
