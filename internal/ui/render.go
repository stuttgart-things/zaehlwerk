package ui

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/panel"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

//go:embed templates/*.html
var templates embed.FS

// Parsed once, at startup rather than per request: a broken template is a build
// mistake, and template.Must says so on the first run instead of on the first
// page load.
var tpl = template.Must(template.ParseFS(templates, "templates/*.html"))

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
}

// matchView is one match as the scoreboard shows it.
type matchView struct {
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
	return s.stateView(m.Scorer.State(), m.Running(), kind)
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
	}
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
