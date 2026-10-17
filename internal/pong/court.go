package pong

import (
	"math"
	"math/rand"
	"slices"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// The court is web/game.js's table for two bots, played in the service
// (ADR-0014). It follows game.js function by function and under the same
// names — shot, step, height, bot, move, update — so a change to how the ball
// flies shows on both sides of the port. Only what a game between two bots
// needs is here: no person's paddle, no guest, no diagonal serve yet.
//
// The constants are game.js's; TestTheCourtKeepsTheConstantsOfGameJS reads
// them from there and fails when the two drift apart.

const (
	// The canvas the page draws on, and the table on it from above: its
	// length across the screen, the net down the middle, a player behind each
	// end.
	courtW, courtH = 960.0, 540.0
	tableX, tableY = 120.0, 80.0
	tableW, tableH = courtW - 240, courtH - 160
	netX           = tableX + tableW/2
	halfW          = tableW / 2
	paddleHalf     = 34.0
	ballR          = 6.0
	spinShare      = 0.25 // SPIN: paddle speed at contact, as curve
	startSpeed     = 520.0
	maxSpeed       = 1050.0
	speedup        = 1.06 // per hit, so a rally gets faster
	letShare       = 0.05 // a serve that touches the net and lands in
)

var paddleX = [2]float64{tableX - 36, tableX + tableW + 36}

// courtTick is one frame of the court, as the page's requestAnimationFrame
// is at 60 Hz. The court's clock is the tick count, not the wall: the ticker
// only paces it, so a game is the same game however busy the pod is.
const courtTick = 1.0 / 60

// courtPress is what the court decided: a half ready, or a rally won by the
// player at a half.
type courtPress struct {
	kind PressKind
	half int
}

type courtBall struct {
	x, y, z, x0, xEnd float64
	vx, vy, spin      float64
	bounces           []float64
	next              int
	hitter            int
	// net is "fault", "let" or empty, drawn when the shot is played.
	net                    string
	touched, passed, serve bool
}

type courtPaddle struct {
	y, vy float64
	// since is when the ball turned this paddle's way, and aim the error the
	// bot meets it with; each unset while the ball goes the other way.
	since    float64
	hasSince bool
	aim      float64
	hasAim   bool
}

type court struct {
	rnd  *rand.Rand
	bots [2]BotParams // player a and b

	st        *scorer.State
	shownEnds [2]scorer.Player
	// now is the court's clock in milliseconds, as performance.now() is the
	// page's.
	now, phaseSince, restingSince float64
	tick                          uint64
	warmServer                    int
	serveRallyServer              int // -1 until drawn
	awaiting                      bool
	readySent                     map[int]bool
	ball                          *courtBall
	resting                       int // -1 for nobody
	paddles                       [2]courtPaddle
	message                       string
	messageUntil                  float64
	// fx is the sounds since the last frame, and presses what the court
	// decided and the driver has not taken yet.
	fx      []float64
	presses []courtPress
}

func newCourt(rnd *rand.Rand, bots [2]BotParams) *court {
	c := &court{rnd: rnd, bots: bots, serveRallyServer: -1, resting: -1, readySent: map[int]bool{},
		shownEnds: [2]scorer.Player{scorer.PlayerA, scorer.PlayerB}, now: 1}
	for h := range c.paddles {
		c.paddles[h].y = tableY + tableH/2
	}
	return c
}

func (c *court) botAt(h int) BotParams { return c.bots[index(c.shownEnds[h])] }

func (c *court) say(text string, ms float64) { c.message, c.messageUntil = text, c.now+ms }

func (c *court) tock(freq float64) { c.fx = append(c.fx, freq) }

// apply is what zaehlwerk says, as the page's apply takes it from the stream
// or from an answer. A state arriving is also the answer to the last press, so
// the next serve may go.
func (c *court) apply(st scorer.State) {
	var before scorer.Phase
	if c.st != nil {
		before = c.st.Phase
	}
	c.st = &st
	c.awaiting = false
	if st.Phase != scorer.PhaseChangeEnds && st.Ends != ([2]scorer.Player{}) {
		c.shownEnds = st.Ends
	}
	if before != st.Phase {
		c.ball = nil
		c.resting = -1
		c.readySent = map[int]bool{}
		c.serveRallyServer = -1
		c.phaseSince = c.now
	}
}

// shot is a shot from half h: one bounce on the other half, or one on each for
// a serve. Where it bounces comes from where it met the paddle and how fast
// the rally has become; whether it clears the net is drawn, more often wrong
// off the edge of the paddle.
func (c *court) shot(h int, y, speed, off, paddleVy float64, serve bool) {
	dir := 1.0
	if h == 1 {
		dir = -1
	}
	pace := speed / startSpeed
	edge := -0.3
	depth := 0.55 + (c.rnd.Float64()-0.5)*0.3*pace
	if c.rnd.Float64() < 0.5 {
		edge = 0.42
	}
	depth += math.Abs(off) * edge
	var bounces []float64
	if serve {
		bounces = append(bounces, netX-dir*0.55*halfW)
	}
	bounces = append(bounces, netX+dir*depth*halfW)
	net := ""
	r := c.rnd.Float64()
	switch {
	case serve && r < 0.02:
		net = "fault"
	case serve && r < 0.02+letShare:
		net = "let"
	case !serve && r < 0.02+0.1*off*off:
		net = "fault"
	}
	vy := off*150 + (c.rnd.Float64()-0.5)*30
	if serve {
		vy = (c.rnd.Float64() - 0.5) * 60
	}
	c.ball = &courtBall{
		x: paddleX[h] + dir*8, y: y, z: 14, x0: paddleX[h], xEnd: paddleX[1-h] + dir*60,
		vx: dir * speed, vy: vy, spin: -paddleVy * spinShare, bounces: bounces, hitter: h, net: net, serve: serve,
	}
}

func (c *court) serve(h int) {
	if c.resting != h || c.awaiting || c.ball != nil {
		return
	}
	c.resting = -1
	c.tock(800)
	c.shot(h, c.paddles[h].y, startSpeed, 0, c.paddles[h].vy, true)
}

// end is a rally over: its winner's half is reported, except while warming up.
func (c *court) end(winner int, label string) {
	c.ball = nil
	if label != "" {
		c.say(label, 1400)
	}
	if c.st == nil || (c.st.Phase != scorer.PhaseServeRally && c.st.Phase != scorer.PhasePlaying) {
		return
	}
	c.awaiting = true
	c.presses = append(c.presses, courtPress{kind: PressRally, half: winner})
}

func (c *court) step(dt float64) {
	b := c.ball
	px := b.x
	b.x += b.vx * dt
	b.y += b.vy * dt
	b.vy += b.spin * dt
	if (px-netX)*(b.x-netX) <= 0 {
		if b.net == "fault" {
			b.x = netX
			c.end(1-b.hitter, "NETZ")
			return
		}
		if b.net == "let" {
			b.touched = true
		}
	}
	sign := math.Copysign(1, b.vx)
	for b.next < len(b.bounces) && (b.x-b.bounces[b.next])*sign >= 0 {
		bx := b.bounces[b.next]
		b.next++
		c.tock(1100)
		if b.y < tableY || b.y > tableY+tableH || bx < tableX || bx > tableX+tableW {
			c.end(1-b.hitter, "AUS")
			return
		}
		if b.next == len(b.bounces) && b.touched {
			c.ball = nil
			c.resting = b.hitter
			c.restingSince = c.now
			c.say("NETZ – Aufschlag wiederholen", 1400)
			return
		}
	}
	b.z = height(b)
	r := 1 - b.hitter
	if !b.passed && b.next == len(b.bounces) && (b.x-paddleX[r])*sign >= 0 {
		p := c.paddles[r]
		if math.Abs(b.y-p.y) <= paddleHalf+ballR {
			c.tock(700)
			off := clamp((b.y-p.y)/paddleHalf, -1, 1)
			c.shot(r, b.y, math.Min(maxSpeed, math.Abs(b.vx)*speedup), off, p.vy, false)
			return
		}
		b.passed = true
	}
	if b.passed && math.Abs(b.x-paddleX[r]) > 60 {
		c.end(b.hitter, "")
	}
}

// height along the flight: a parabola from the paddle to each bounce and on
// past the far paddle. Only drawn, as shadow and size.
func height(b *courtBall) float64 {
	xs := append(append([]float64{b.x0}, b.bounces...), b.xEnd)
	for i := 0; i < len(xs)-1; i++ {
		u := (b.x - xs[i]) / (xs[i+1] - xs[i])
		if u >= 0 && u <= 1 {
			za, zb := 0.0, 0.0
			if i == 0 {
				za = 14
			}
			if i == len(xs)-2 {
				zb = 18
			}
			return za*(1-u) + zb*u + 34*4*u*(1-u)
		}
	}
	return 0
}

// bot moves its paddle: it starts its reaction time after the ball turns its
// way, no faster than its speed, to where the ball will cross plus its aim
// error.
func (c *court) bot(h int, params BotParams, dt float64) {
	p := &c.paddles[h]
	target := tableY + tableH/2
	if c.ball != nil && c.ball.hitter != h {
		if !p.hasSince {
			p.since, p.hasSince = c.now, true
		}
		// A faster ball is harder to meet in the middle.
		if !p.hasAim {
			p.aim = (c.rnd.Float64()*2 - 1) * params.Aim * paddleHalf * math.Abs(c.ball.vx) / startSpeed
			p.hasAim = true
		}
		if c.now-p.since < float64(params.ReactionMS) {
			target = p.y
		} else {
			b := c.ball
			t := math.Max(0, (paddleX[h]-b.x)/b.vx)
			target = b.y + b.vy*t + 0.5*b.spin*t*t + p.aim
		}
	} else {
		p.hasSince, p.hasAim = false, false
	}
	move(p, target, params.Speed*tableH, dt)
}

func move(p *courtPaddle, target, maxV, dt float64) {
	v := clamp((target-p.y)/math.Max(dt, 0.001), -maxV, maxV)
	p.vy = v
	p.y = clamp(p.y+v*dt, tableY-60, tableY+tableH+60)
}

func (c *court) ready(half int) {
	if c.readySent[half] || !slices.Contains(c.st.Waiting, halfAt(half)) {
		return
	}
	c.readySent[half] = true
	c.presses = append(c.presses, courtPress{kind: PressReady, half: half})
}

// update is one tick: the bots move, press ready, serve, and the ball flies.
func (c *court) update() {
	dt := courtTick
	c.tick++
	c.now += dt * 1000
	for h := range c.paddles {
		c.bot(h, c.botAt(h), dt)
	}
	if c.st == nil {
		return
	}
	switch c.st.Phase {
	case scorer.PhaseWarmup, scorer.PhaseChangeEnds:
		// The bots confirm a moment after the phase starts, at their new half.
		if c.now-c.phaseSince > 1200 {
			for _, p := range []scorer.Player{scorer.PlayerA, scorer.PlayerB} {
				c.ready(halfIndex(halfOf(p, c.st.Ends)))
			}
		}
	}
	switch c.st.Phase {
	case scorer.PhaseWarmup:
		// Free play: nothing scores, the ball comes back from alternate sides.
		if c.ball == nil && c.resting < 0 {
			c.resting, c.restingSince = c.warmServer, c.now
			c.warmServer ^= 1
		}
		if c.resting >= 0 && c.now-c.restingSince > 900 {
			c.serve(c.resting)
		}
	case scorer.PhaseServeRally, scorer.PhasePlaying:
		if c.ball == nil && c.resting < 0 && !c.awaiting {
			switch {
			case c.st.Phase == scorer.PhasePlaying:
				c.resting = halfIndex(halfOf(c.st.Serving, c.shownEnds))
			case c.serveRallyServer < 0:
				c.serveRallyServer = c.rnd.Intn(2)
				c.resting = c.serveRallyServer
			default:
				c.resting = c.serveRallyServer
			}
			c.restingSince = c.now
		}
		if c.resting >= 0 && c.now-c.restingSince > 700 {
			c.serve(c.resting)
		}
	}
	if c.ball != nil {
		c.step(dt)
	}
}

// frame is the court as the relay carries it, in the shape the host of a game
// on two devices sends (ADR-0009), and takes the sounds since the last one.
func (c *court) frame() relayFrame {
	f := relayFrame{Tick: c.tick, Paddles: [2]float64{c.paddles[0].y, c.paddles[1].y}}
	if c.ball != nil {
		f.Ball = &relayBall{X: c.ball.x, Y: c.ball.y, Z: c.ball.z}
	}
	if c.resting >= 0 {
		r := c.resting
		f.Resting = &r
	}
	if c.now < c.messageUntil {
		f.Say = c.message
	}
	f.Fx, c.fx = c.fx, nil
	return f
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }
