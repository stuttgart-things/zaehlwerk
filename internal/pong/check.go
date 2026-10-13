package pong

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// A button check plays a match with the real buttons at the table. The game
// decides what is pressed and says so on the page, "LINKS drücken", and sends
// nothing itself: whatever arrives on /live/stream came from the buttons, the
// hub and the radio, and is compared with what was asked for. It is the chain
// tested against a known truth, with the hardware in it (#78, ADR-0006).

// DriverCheck is a button check, as POST /games and GET /games/{id} name it.
const DriverCheck = "check"

// CheckNames are a button check's players: free text like every game's, and
// short enough for the panel's banner.
var CheckNames = [2]string{"Test A", "Test B"}

// DefaultCheckTimeout is how long a prompt waits for its press before it is
// counted as missing.
const DefaultCheckTimeout = 10 * time.Second

// checkGap is the pause between two prompts. A press arriving in it was not
// asked for: the same button again is a bounce, the other one a press nobody
// was told to make. Long enough to catch a button that bounces or a hub that
// sends twice, short enough not to feel like waiting.
const checkGap = time.Second

// maxCheckPrompts ends a check that does not get anywhere, and maxMissed one
// nobody is pressing in.
const (
	maxCheckPrompts = 80
	maxMissed       = 3
)

// maxCheckFindings is how many findings a check keeps with their time; the
// counts go on past it.
const maxCheckFindings = 20

// errNobodyPresses is a button check whose prompts went unanswered.
var errNobodyPresses = fmt.Errorf("%d prompts in a row went unanswered", maxMissed)

// CheckOutcome is what came of one prompt, or of a press between two.
type CheckOutcome string

const (
	CheckOK         CheckOutcome = "ok"
	CheckWrongHalf  CheckOutcome = "wrong_half"
	CheckDouble     CheckOutcome = "double"
	CheckMissing    CheckOutcome = "missing"
	CheckUnexpected CheckOutcome = "unexpected"
)

var checkOutcomes = []CheckOutcome{CheckOK, CheckWrongHalf, CheckDouble, CheckMissing, CheckUnexpected}

// checkLabels are the outcomes in the summary, which is for a firmware issue
// and so English like the rest of the repo's issues.
var checkLabels = map[CheckOutcome]string{
	CheckOK:         "ok",
	CheckWrongHalf:  "wrong half",
	CheckDouble:     "double (bounce)",
	CheckMissing:    "missing",
	CheckUnexpected: "unexpected",
}

// Prompt is what the page tells the table to press.
type Prompt struct {
	N     int          `json:"n"`
	Kind  PressKind    `json:"kind"`
	Phase scorer.Phase `json:"phase"`
	Half  scorer.Half  `json:"half"`
	// For is the name of the player the press counts for: the one standing
	// at that half now, which after a change of ends is the other one.
	For   string    `json:"for"`
	Since time.Time `json:"since"`
}

func (p Prompt) String() string {
	if p.Kind == PressReady {
		return fmt.Sprintf("#%d %s, ready %s", p.N, p.Phase, p.Half)
	}
	return fmt.Sprintf("#%d %s, press %s for %s", p.N, p.Phase, p.Half, p.For)
}

// CheckFinding is one prompt that did not go as asked, or a press nobody asked
// for.
type CheckFinding struct {
	At      time.Time    `json:"at"`
	Outcome CheckOutcome `json:"outcome"`
	// Prompt is the prompt it belongs to: the one answered wrong or not at
	// all, or for a bounce the one whose press bounced. Zero for a press
	// between prompts that is not a bounce.
	Prompt int    `json:"prompt"`
	Want   string `json:"want"`
	Got    string `json:"got"`
}

// CheckView is a button check in GET /games/{id}.
type CheckView struct {
	// Prompt is what to press now; nil between two prompts and at the end.
	Prompt  *Prompt              `json:"prompt,omitempty"`
	Timeout string               `json:"timeout"`
	Prompts int                  `json:"prompts"`
	Counts  map[CheckOutcome]int `json:"counts"`
	// Findings are the first of them, with their time.
	Findings []CheckFinding `json:"findings"`
	// Summary is the check as text, to paste into a firmware issue.
	Summary string `json:"summary"`
}

// checkRun is a button check's record. Guarded by the game's mu.
type checkRun struct {
	started  time.Time
	prompt   *Prompt
	prompts  int
	counts   map[CheckOutcome]int
	findings []CheckFinding
}

// checkPlan draws the prompts. They are drawn from the seed and from where
// zaehlwerk has the match, so a run with the same seed and the same presses
// asks for the same thing, and a wrong press is followed rather than fought.
//
// What it makes sure of: both ready presses, the rally for the serve, points
// on both halves, a change of ends, and after it both halves again — the same
// button now counting for the other player, which is the mapping the change of
// ends exists to test (ADR-0005).
type checkPlan struct {
	rnd *rand.Rand
	// winner is the half that takes the first set. Drawn once; the other
	// half gets a few points so that both are seen, and few enough that the
	// set does not go to deuce.
	winner int
	// changed is a change of ends seen; after is the halves pressed right
	// since, for the players now standing there.
	changed bool
	after   [2]bool
}

func newCheckPlan(seed int64) *checkPlan {
	rnd := rand.New(rand.NewSource(seed))
	return &checkPlan{rnd: rnd, winner: rnd.Intn(2)}
}

// next is the prompt for where the match stands, or done once the plan has
// seen what it set out to.
func (c *checkPlan) next(st scorer.State) (Prompt, bool) {
	switch st.Phase {
	case scorer.PhaseWarmup, scorer.PhaseChangeEnds:
		if len(st.Waiting) == 0 {
			return Prompt{}, true
		}
		return Prompt{Kind: PressReady, Phase: st.Phase, Half: st.Waiting[c.rnd.Intn(len(st.Waiting))]}, false
	case scorer.PhaseServeRally:
		return Prompt{Kind: PressRally, Phase: st.Phase, Half: halfAt(c.rnd.Intn(2))}, false
	case scorer.PhasePlaying:
	default:
		return Prompt{}, true
	}

	if c.changed {
		switch {
		case c.after[0] && c.after[1]:
			return Prompt{}, true
		case c.after[0]:
			return Prompt{Kind: PressRally, Phase: st.Phase, Half: scorer.HalfRight}, false
		case c.after[1]:
			return Prompt{Kind: PressRally, Phase: st.Phase, Half: scorer.HalfLeft}, false
		}
		return Prompt{Kind: PressRally, Phase: st.Phase, Half: halfAt(c.rnd.Intn(2))}, false
	}

	// Before the change of ends: mostly the winner's half, the other one now
	// and then, at least once, and never so often that the set goes long.
	w := c.winner
	won := st.Points[index(st.Ends[w])]
	lost := st.Points[index(st.Ends[w^1])]
	other := false
	switch {
	case lost >= 8:
	case lost == 0 && won >= 3:
		other = true
	default:
		other = c.rnd.Float64() < 0.25
	}
	if other {
		w ^= 1
	}
	return Prompt{Kind: PressRally, Phase: st.Phase, Half: halfAt(w)}, false
}

// pressedHalf says which half a transition was pressed at, judged from the
// state before it and after it. False for a transition no single press makes,
// such as an undo.
func pressedHalf(kind string, before, after scorer.State) (scorer.Half, bool) {
	switch scorer.TransitionKind(kind) {
	case scorer.TransitionConfirmed:
		for _, h := range before.Waiting {
			if !slices.Contains(after.Waiting, h) {
				return h, true
			}
		}
	case scorer.TransitionWarmupOver, scorer.TransitionEndsChanged:
		if len(before.Waiting) == 1 {
			return before.Waiting[0], true
		}
	case scorer.TransitionServeDecided:
		if after.Serving != "" {
			return halfOf(after.Serving, before.Ends), true
		}
	case scorer.TransitionPoint, scorer.TransitionSetWon, scorer.TransitionMatchWon:
		for i := range 2 {
			if after.Sets[i] > before.Sets[i] ||
				(after.SetNumber == before.SetNumber && after.Points[i] > before.Points[i]) {
				return halfOf(playerAt(i), before.Ends), true
			}
		}
	}
	return "", false
}

// pressKind is the kind of press a phase takes.
func pressKind(ph scorer.Phase) PressKind {
	if ph == scorer.PhaseWarmup || ph == scorer.PhaseChangeEnds {
		return PressReady
	}
	return PressRally
}

// arrived is a transition as a finding names it.
func arrived(kind string, half scorer.Half, known bool, after scorer.State) string {
	if !known {
		return fmt.Sprintf("%s, now %s", kind, summary(after))
	}
	return fmt.Sprintf("%s at %s, now %s", kind, half, summary(after))
}

// runCheck is the button check from the first prompt to the last. It reads the
// table itself, rather than leaving it to watch: the transitions of its own
// match are what it is checking.
func (g *Game) runCheck(ctx context.Context, table <-chan TableEvent) error {
	plan := newCheckPlan(g.cfg.Seed)
	timeout := g.cfg.CheckTimeout
	if timeout <= 0 {
		timeout = DefaultCheckTimeout
	}
	gap := g.cfg.CheckGap
	if gap <= 0 {
		gap = checkGap
	}
	st := g.current()
	missed := 0

	// own reads the next event of the game's own match. A snapshot of
	// another match is that match taking the table, as in watch; a stream
	// that closes leaves the check blind; the match ended on zaehlwerk ends
	// the check.
	own := func(ev TableEvent, ok bool) (bool, error) {
		switch {
		case !ok:
			return false, errTableLost
		case ev.State == nil:
			return false, nil
		case ev.State.MatchID != g.matchIDNow():
			if ev.Kind == kindSnapshot {
				g.log.Info("another match is current, ending the check",
					"other", ev.State.MatchID, "reported", ev.Reported())
				return false, errDisplaced
			}
			return false, nil
		case ev.Kind == matchEnded:
			return false, ErrMatchOver
		}
		return true, nil
	}

	for {
		if g.checkPrompts() >= maxCheckPrompts {
			return fmt.Errorf("%d prompts and the check has not got through", maxCheckPrompts)
		}
		p, done := plan.next(st)
		if done || st.Phase == scorer.PhaseComplete {
			g.log.Info("button check done", "prompts", g.checkPrompts())
			return nil
		}
		p.For = st.Players[index(st.Ends[halfIndex(p.Half)])]
		p = g.ask(p)

		// The prompt, until a press answers it or it times out.
		limit := time.NewTimer(timeout)
		var answered scorer.Half
	prompt:
		for {
			select {
			case <-ctx.Done():
				limit.Stop()
				return ctx.Err()
			case <-limit.C:
				g.found(p, CheckMissing, "nothing arrived")
				missed++
				if missed >= maxMissed {
					return errNobodyPresses
				}
				break prompt
			case ev, ok := <-table:
				mine, err := own(ev, ok)
				if err != nil {
					limit.Stop()
					return err
				}
				if !mine {
					continue
				}
				before := st
				st = *ev.State
				g.setState(st)
				if ev.Kind == kindSnapshot || ev.Kind == matchStarted {
					continue
				}
				half, known := pressedHalf(ev.Kind, before, st)
				switch {
				case !known || pressKind(before.Phase) != p.Kind || before.Phase != p.Phase:
					g.found(p, CheckUnexpected, arrived(ev.Kind, half, known, st))
					// Not an answer: the prompt stays, now for a match
					// that has moved, and the plan asks again after it.
					continue
				case half != p.Half:
					g.found(p, CheckWrongHalf, arrived(ev.Kind, half, known, st))
				default:
					g.answered(p)
					if before.Phase == scorer.PhasePlaying && plan.changed {
						plan.after[halfIndex(half)] = true
					}
				}
				if scorer.TransitionKind(ev.Kind) == scorer.TransitionEndsChanged {
					plan.changed = true
				}
				missed = 0
				answered = half
				limit.Stop()
				break prompt
			}
		}

		// The gap. Nothing is asked for, so anything that arrives is a
		// finding: the button just pressed again is a bounce, any other a
		// press nobody was told to make.
		g.ask(Prompt{})
		pause := time.NewTimer(gap)
	between:
		for {
			select {
			case <-ctx.Done():
				pause.Stop()
				return ctx.Err()
			case <-pause.C:
				break between
			case ev, ok := <-table:
				mine, err := own(ev, ok)
				if err != nil {
					pause.Stop()
					return err
				}
				if !mine {
					continue
				}
				before := st
				st = *ev.State
				g.setState(st)
				if ev.Kind == kindSnapshot || ev.Kind == matchStarted {
					continue
				}
				half, known := pressedHalf(ev.Kind, before, st)
				if known && half == answered {
					g.found(p, CheckDouble, arrived(ev.Kind, half, known, st))
				} else {
					g.found(Prompt{}, CheckUnexpected, arrived(ev.Kind, half, known, st))
				}
				if scorer.TransitionKind(ev.Kind) == scorer.TransitionEndsChanged {
					plan.changed = true
				}
			}
		}
	}
}

// The table kinds a check reads besides transitions, spelled out like the
// others in client.go.
const (
	matchStarted = "match_started"
	matchEnded   = "match_ended"
)

// ask shows a prompt, numbered and timed here; a zero prompt is the gap.
func (g *Game) ask(p Prompt) Prompt {
	g.mu.Lock()
	defer g.mu.Unlock()
	if p.Half == "" {
		g.check.prompt = nil
	} else {
		g.check.prompts++
		p.N, p.Since = g.check.prompts, time.Now()
		g.check.prompt = &p
		g.log.Info("prompt", "n", p.N, "phase", p.Phase, "kind", p.Kind, "half", p.Half, "for", p.For)
	}
	g.changed()
	return p
}

func (g *Game) answered(p Prompt) {
	g.mu.Lock()
	g.check.counts[CheckOK]++
	g.changed()
	g.mu.Unlock()
	g.log.Info("prompt answered", "n", p.N, "half", p.Half)
}

// found records a finding. want is the prompt it belongs to, zero for none.
func (g *Game) found(want Prompt, o CheckOutcome, got string) {
	f := CheckFinding{At: time.Now(), Outcome: o, Prompt: want.N, Got: got}
	if want.N != 0 {
		f.Want = want.String()
	}
	g.mu.Lock()
	g.check.counts[o]++
	if len(g.check.findings) < maxCheckFindings {
		g.check.findings = append(g.check.findings, f)
	}
	g.changed()
	g.mu.Unlock()
	g.log.Warn("button check finding", "outcome", o, "prompt", f.Prompt, "want", f.Want, "got", got)
}

func (g *Game) checkPrompts() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.check.prompts
}

// checkView is the check as the view shows it. Called with g.mu held.
func (g *Game) checkView() *CheckView {
	if g.check == nil {
		return nil
	}
	c := g.check
	v := &CheckView{
		Prompts:  c.prompts,
		Timeout:  g.checkTimeout().String(),
		Counts:   map[CheckOutcome]int{},
		Findings: append([]CheckFinding{}, c.findings...),
	}
	for _, o := range checkOutcomes {
		v.Counts[o] = c.counts[o]
	}
	if c.prompt != nil {
		p := *c.prompt
		v.Prompt = &p
	}
	v.Summary = g.checkSummary(v)
	return v
}

func (g *Game) checkTimeout() time.Duration {
	if g.cfg.CheckTimeout > 0 {
		return g.cfg.CheckTimeout
	}
	return DefaultCheckTimeout
}

// checkSummary is the check as text for a firmware issue: what was checked,
// the counts, and the first findings with their time. Called with g.mu held.
func (g *Game) checkSummary(v *CheckView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "zaehlwerk-pong button check %s, match %s, seed %d\n", g.ID, g.matchID, g.cfg.Seed)
	status := string(g.status)
	if g.reason != "" {
		status += ": " + g.reason
	}
	fmt.Fprintf(&b, "started %s, timeout %s, %s\n",
		g.check.started.Format(time.RFC3339), v.Timeout, status)
	fmt.Fprintf(&b, "prompts %d:", v.Prompts)
	for i, o := range checkOutcomes {
		sep := ","
		if i == 0 {
			sep = ""
		}
		fmt.Fprintf(&b, "%s %s %d", sep, checkLabels[o], v.Counts[o])
	}
	b.WriteString("\n")
	if len(v.Findings) == 0 {
		b.WriteString("no findings\n")
		return b.String()
	}
	b.WriteString("\nfirst findings:\n")
	for _, f := range v.Findings {
		want := f.Want
		if want == "" {
			want = "no prompt"
		}
		fmt.Fprintf(&b, "%s  %-16s %s -> %s\n", f.At.Format("15:04:05.000"), checkLabels[f.Outcome], want, f.Got)
	}
	return b.String()
}
