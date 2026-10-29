package pong

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// FaultKind is one way a press goes wrong between the table and zaehlwerk. The
// names follow the knobs the button and piezo mocks already have.
type FaultKind string

const (
	// FaultWrongHalf is the point sent for the other half: the mis-press.
	// With Correct, an undo follows and then the right half.
	FaultWrongHalf FaultKind = "wrong_half"
	// FaultResend is the same event sent twice with the same event id, as a
	// sender does that never heard the answer. zaehlwerk discards the second.
	FaultResend FaultKind = "resend"
	// FaultBounce is a second press with a new event id, as a button without
	// debounce sends. zaehlwerk counts it, which is why debounce belongs in
	// the button.
	FaultBounce FaultKind = "bounce"
	// FaultLost is a rally whose press never arrives.
	FaultLost FaultKind = "lost"
	// FaultGhost is a press no rally produced.
	FaultGhost FaultKind = "ghost"
	// FaultWrongPhase is a second confirmation from a half that already
	// pressed, in the warm-up or a change of ends. zaehlwerk ignores it.
	FaultWrongPhase FaultKind = "wrong_phase"
	// FaultCounterReset is the counter starting again at 1 after a quiet gap,
	// as after a reflash. zaehlwerk accepts it (ADR-0002).
	FaultCounterReset FaultKind = "counter_reset"
	// FaultLatency is a press that arrives late, after the next press
	// overtook it: a radio retrying while the table plays on. It carries the
	// older event id, and zaehlwerk discards it as already seen (ADR-0002).
	// A delay that overtakes nothing changes nothing, so this is the delay
	// that matters.
	FaultLatency FaultKind = "latency"
)

// Faults is how often each fault is injected, each a share from 0 to 1. A
// share is a chance per opportunity, and what an opportunity is differs:
//
//   - wrong_half: every rally reported, the serve rally included
//   - lost, ghost, bounce, latency: every rally played for a point
//   - wrong_phase: every confirmation zaehlwerk answers still waiting
//   - resend, counter_reset: every press sent
//
// lost, ghost and bounce leave the serve rally alone, because what they do to
// it is not a divergence in points: a lost serve rally is played again, and a
// bounce after it would be the first point.
type Faults struct {
	WrongHalf float64 `json:"wrong_half"`
	// Correct takes every wrong half back with an undo and sends the right
	// one. That also exercises undo in serve_rally and change_ends, wherever
	// the wrong press left the match.
	Correct      bool    `json:"correct"`
	Resend       float64 `json:"resend"`
	Bounce       float64 `json:"bounce"`
	Lost         float64 `json:"lost"`
	Ghost        float64 `json:"ghost"`
	WrongPhase   float64 `json:"wrong_phase"`
	CounterReset float64 `json:"counter_reset"`
	Latency      float64 `json:"latency"`
}

// shares names every share, for parsing and for checking it.
func (f *Faults) shares() map[FaultKind]*float64 {
	return map[FaultKind]*float64{
		FaultWrongHalf:    &f.WrongHalf,
		FaultResend:       &f.Resend,
		FaultBounce:       &f.Bounce,
		FaultLost:         &f.Lost,
		FaultGhost:        &f.Ghost,
		FaultWrongPhase:   &f.WrongPhase,
		FaultCounterReset: &f.CounterReset,
		FaultLatency:      &f.Latency,
	}
}

// Validate refuses a share outside 0..1. A typo is a run that tests nothing,
// so it is an error, not a value clamped into range.
func (f Faults) Validate() error {
	for kind, share := range f.shares() {
		if math.IsNaN(*share) || *share < 0 || *share > 1 {
			return fmt.Errorf("fault %s=%v: want a share from 0 to 1", kind, *share)
		}
	}
	return nil
}

// ParseFaults reads PONG_FAULTS: "wrong_half=0.05,resend=0.1,correct=true".
// Empty is no faults. A name it does not know is refused rather than ignored,
// so that "wrong-half=0.1" does not quietly run without faults.
func ParseFaults(raw string) (Faults, error) {
	var f Faults
	if strings.TrimSpace(raw) == "" {
		return f, nil
	}
	shares := f.shares()
	seen := map[string]bool{}
	for _, pair := range strings.Split(raw, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			return f, fmt.Errorf("faults %q: want name=value pairs separated by commas", raw)
		}
		if seen[name] {
			return f, fmt.Errorf("fault %s: named twice", name)
		}
		seen[name] = true

		if name == "correct" {
			b, err := strconv.ParseBool(value)
			if err != nil {
				return f, fmt.Errorf("correct=%s: want true or false", value)
			}
			f.Correct = b
			continue
		}
		share, known := shares[FaultKind(name)]
		if !known {
			return f, fmt.Errorf("fault %q: want one of %s, or correct", name, faultNames())
		}
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return f, fmt.Errorf("fault %s=%s: want a share from 0 to 1", name, value)
		}
		*share = v
	}
	return f, f.Validate()
}

// faultKinds is every fault, in the order the README's table lists them.
var faultKinds = []FaultKind{
	FaultWrongHalf, FaultResend, FaultBounce, FaultLost,
	FaultGhost, FaultWrongPhase, FaultCounterReset, FaultLatency,
}

func faultNames() string {
	names := make([]string, len(faultKinds))
	for i, k := range faultKinds {
		names[i] = string(k)
	}
	return strings.Join(names, ", ")
}

// Fault is one fault injected, as GET /games/{id} lists it.
type Fault struct {
	// Rally is the press's number in the game: every press takes the next
	// one, a lost press included, and it never resets.
	Rally uint64 `json:"rally"`
	// EventID is what was sent, if anything. It equals Rally until a counter
	// reset, and a lost press sent nothing.
	EventID uint64    `json:"event_id,omitempty"`
	Kind    FaultKind `json:"kind"`
	// Phase is the phase the press was sent in.
	Phase scorer.Phase `json:"phase"`
	Half  scorer.Half  `json:"half,omitempty"`
	// Effect is what the fault should do to the points zaehlwerk awards,
	// player a and player b. A divergence it explains is expected.
	Effect [2]int `json:"effect"`
	// UndoneIn is the phase a corrected wrong half was taken back in.
	UndoneIn scorer.Phase `json:"undone_in,omitempty"`
}

// The quiet gap before a counter reset. zaehlwerk reads a counter that falls
// back as a restarted source only if the source has sent nothing for
// scorer.DefaultRestartQuietPeriod — one minute, and lastSeen moves with every
// press, a discarded one included — and the new id is at most
// scorer.DefaultRestartMaxEventID, which is 8. zaehlwerk-api sets neither, so
// the game waits the minute and a margin for clocks and request time, and
// starts again at 1. Anything shorter is a reset zaehlwerk rightly discards
// as a duplicate, and that shows up as a finding.
const (
	resetQuiet   = time.Minute + 2*time.Second
	resetEventID = 1
)
