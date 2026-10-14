package ui

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"slices"
	"strings"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/panel"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

//go:embed templates/*.html
var templates embed.FS

// Parsed once, at startup rather than per request: a broken template is a build
// mistake, and template.Must says so on the first run instead of on the first
// page load.
var tpl = template.Must(template.New("").Funcs(templateFuncs).ParseFS(templates, "templates/*.html"))

// view is what every template here renders. A nil Match is the empty state —
// nothing is running, or the id in ?match is not one we know.
type view struct {
	Match *matchView
	Log   []logRow
	// Error is shown in the board rather than returned as a 4xx, because htmx
	// does not swap an error response by default and a button that silently
	// does nothing is worse than one that says why. Same call the led-catcher
	// makes in its /ui/streams control.
	Error string
	// Stream says whether the page may open an SSE connection for this match.
	// False without a hub, and false with no match to watch.
	Stream bool
	// Roster is the players Schmetterpause knows, fetched while this page was
	// rendered and never cached (ADR-0004). Empty when the coupling is off,
	// and then the form asks for two free-text names as it always has.
	Roster []RosterPlayer
	// RosterErr says why the list is missing when the coupling is on. Shown
	// rather than swallowed: a form silently falling back to free text would
	// start a match that cannot be reported, and nobody would find out until
	// the last point.
	RosterErr string
	// Operators is who may keep score: the players plus the observers, who
	// never play and so are never in Roster. Observers come first.
	Operators []RosterPlayer
	// ReportsTo is the Schmetterpause base URL, empty when the coupling is off.
	ReportsTo string
	// Follow is set on the spectator view opened without ?match: it moves on
	// to the next match by itself, where a pinned one stays put.
	Follow bool
}

// RosterPlayer is one option in the name pickers.
type RosterPlayer struct {
	ID   string
	Name string
	// Observer marks somebody who never plays; only the scorekeeper picker
	// offers them.
	Observer bool
}

// matchView is one match as the scoreboard shows it.
type matchView struct {
	// Reported says what became of the handover to Schmetterpause. Nil when
	// this match is not being reported, which is the ordinary case at a table
	// with nothing else set up.
	Reported      *reportView
	ID            string
	A             sideView
	B             sideView
	SetNumber     int
	Complete      bool
	Running       bool
	CompletedSets [][2]int
	// Panel is the title this score is pitched to the matrix as — the whole of
	// what the panel shows. It is here so that the page and the panel can be
	// disagreed with in the same glance.
	Panel string
	// Phase is what the players have to do when it is not playing a point:
	// empty while they play. A +1 on this page is then a press like any
	// other — ready, the serve rally won, arrived at the new end — so the
	// page says which (ADR-0005).
	Phase string
}

type sideView struct {
	Name    string
	Points  int
	Sets    int
	Serving bool
	Winner  bool
}

// logRow is one transition, in the words the panel gets it in: the title the
// matrix renders and the summary the catcher logs.
type logRow struct {
	Time     string
	Kind     string
	Severity string
	Title    string
	Summary  string
}

func (s *Server) matchView(m *match.Match, kind scorer.TransitionKind) *matchView {
	st := m.Scorer.State()
	v := s.stateView(st, m.Running(), kind)
	v.Reported = s.reportView(m, st)
	return v
}

// reportView is what became of the handover of m, against the score st. Nil
// for a match that was not meant to be reported. stateView cannot do this: it
// renders a state, and the handover belongs to the match around it.
func (s *Server) reportView(m *match.Match, st scorer.State) *reportView {
	if !m.Handover.Wanted() {
		return nil
	}
	r := m.Report()
	v := &reportView{
		Done:     r.Done,
		Failed:   !r.Done && r.Err != nil,
		Attempts: r.Attempts,
	}
	if r.Err != nil {
		v.Reason = r.Err.Error()
	}
	if !r.Due.IsZero() && !r.Done {
		v.Due = r.Due.Format("15:04:05")
		v.In = max(int(math.Ceil(r.Due.Sub(s.now()).Seconds())), 0)
	}
	// An undo after the report is not reported again (ADR-0007), so the
	// result over there is the one sent. Said whenever the score has moved
	// away from it, not only once the match is won again.
	if r.Done && (!st.Complete || !slices.Equal(r.Sent, st.CompletedSets)) {
		v.Stale = true
		v.Sent = sets(r.Sent)
	}
	return v
}

// sets is a result as a line: "11:9 7:11 11:5".
func sets(s [][2]int) string {
	out := make([]string, len(s))
	for i, set := range s {
		out[i] = fmt.Sprintf("%d:%d", set[0], set[1])
	}
	return strings.Join(out, " ")
}

// stateView renders one state rather than the match's current one, so that the
// stream shows the state each transition carried instead of whatever the score
// has since become.
func (s *Server) stateView(st scorer.State, running bool, kind scorer.TransitionKind) *matchView {
	return &matchView{
		ID: st.MatchID,
		A: sideView{
			Name: st.Players[0], Points: st.Points[0], Sets: st.Sets[0],
			Serving: st.Serving == scorer.PlayerA && !st.Complete,
			Winner:  st.Complete && st.Winner == scorer.PlayerA,
		},
		B: sideView{
			Name: st.Players[1], Points: st.Points[1], Sets: st.Sets[1],
			Serving: st.Serving == scorer.PlayerB && !st.Complete,
			Winner:  st.Complete && st.Winner == scorer.PlayerB,
		},
		SetNumber:     st.SetNumber,
		Complete:      st.Complete,
		Running:       running,
		CompletedSets: st.CompletedSets,
		Panel:         panel.Title(scorer.Transition{Kind: kind, State: st}),
		Phase:         phaseText(st),
	}
}

// phaseText is the page's line for a phase in which a press is not a point.
func phaseText(st scorer.State) string {
	switch st.Phase {
	case scorer.PhaseWarmup:
		return "warm-up: each side presses once when ready" + waitingFor(st)
	case scorer.PhaseServeRally:
		return "rally for the first serve: the winner's side presses"
	case scorer.PhaseChangeEnds:
		return "change ends: each player presses at the new half" + waitingFor(st)
	default:
		return ""
	}
}

func waitingFor(st scorer.State) string {
	if len(st.Waiting) == 0 {
		return ""
	}
	halves := make([]string, len(st.Waiting))
	for i, h := range st.Waiting {
		halves[i] = string(h)
	}
	return " (waiting for " + strings.Join(halves, ", ") + ")"
}

// liveView is the match plus whether it can be watched.
func (s *Server) liveView(m *match.Match, kind scorer.TransitionKind) view {
	return view{Match: s.matchView(m, kind), Stream: s.hub != nil}
}

// errorView is the board as it stands, with a reason under it.
func (s *Server) errorView(m *match.Match, err error) view {
	v := s.liveView(m, kindOf(m.Scorer.State()))
	v.Error = err.Error()
	return v
}

func (s *Server) logRow(kind scorer.TransitionKind, st scorer.State) logRow {
	t := scorer.Transition{Kind: kind, State: st}
	return logRow{
		Time:     s.now().Format("15:04:05"),
		Kind:     string(kind),
		Severity: panel.Severity(kind),
		Title:    panel.Title(t),
		Summary:  panel.Summary(t),
	}
}

// kindBetween names the transition that took a match from before to after.
//
// The scorer reports the outcome of an event, not which kind of transition it
// emitted, and the panel title differs by exactly that: a set win reads
// "SET 1:0" where the point that won it would read "11:9". Comparing the two
// states is exact, where guessing from the state alone is not — see [kindOf].
func kindBetween(before, after scorer.State) scorer.TransitionKind {
	switch {
	case after.Complete && !before.Complete:
		return scorer.TransitionMatchWon
	case len(after.CompletedSets) > len(before.CompletedSets):
		return scorer.TransitionSetWon
	default:
		return scorer.TransitionPoint
	}
}

// kindOf guesses the last transition from the state alone, for a page that is
// being rendered fresh rather than in answer to a point.
//
// A completed match and a set just won are both visible in the state: a set
// leaves the points at 0:0 with a set in the completed list. What it cannot
// tell apart is a set win from a point taken back to the start of the next set,
// and it reads that as a set win. The page then shows "SET 1:0" where the panel
// shows "0:0", until the next point puts both right.
func kindOf(st scorer.State) scorer.TransitionKind {
	switch {
	case st.Complete:
		return scorer.TransitionMatchWon
	case st.Points == [2]int{} && len(st.CompletedSets) > 0:
		return scorer.TransitionSetWon
	default:
		return scorer.TransitionPoint
	}
}

func (s *Server) render(w http.ResponseWriter, name string, v view) {
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, name, v); err != nil {
		// Nothing has been written yet, so this can still be a 500 rather than
		// a half-rendered page.
		s.log.Error("rendering the page failed", "template", name, "error", err)
		http.Error(w, "rendering failed", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(buf.Bytes())
}

// renderLive answers with the whole live panel, which carries the SSE
// connection: the browser swaps it by outerHTML and reconnects to whatever
// match it now holds.
func (s *Server) renderLive(w http.ResponseWriter, v view) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.render(w, "live", v)
}

// renderBoard answers with the scoreboard alone, which is what every button on
// it swaps.
func (s *Server) renderBoard(w http.ResponseWriter, v view) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.render(w, "board", v)
}

// partial renders one template to a string, for the SSE stream: an event has to
// carry its payload, not write it.
func partial(name string, v view) (string, error) {
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, name, v); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// reportView is the handover as the page shows it.
//
// It exists because ADR-0004 accepted that a result can be lost and asked for
// the loss to be visible: without somewhere to say so, a match whose result
// never arrived looks exactly like one that was never meant to be reported.
type reportView struct {
	// Done is true once Schmetterpause has the result. It says "pending
	// there", not "counted": the result waits for one of the two players.
	Done bool
	// Failed is true when the last attempt did not get through and the result
	// can still be retried.
	Failed bool
	// Reason is the sentence Schmetterpause gave, or the transport error.
	Reason string
	// Attempts is how many times it has been tried.
	Attempts int
	// Due is the clock time the result goes out at while it waits out the
	// grace period (ADR-0007), and In the seconds left. Empty and zero
	// otherwise.
	Due string
	In  int
	// Stale is a result sent and then undone: Schmetterpause holds Sent,
	// which is no longer the score, and only somebody over there can reject
	// or correct it.
	Stale bool
	Sent  string
}
