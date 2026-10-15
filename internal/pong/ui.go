package pong

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// The page around the game is htmx and SSE, the way zaehlwerk's scoring page
// is: the lobby is a form, the banner, the score and truth against zaehlwerk
// are partials the service renders and swaps in. Only the game itself, the
// canvas, is JavaScript (web/game.js, ADR-0006), and on a button check's page
// the few lines that hold its token (web/check.js).
//
//go:embed web/*.html web/*.js
var web embed.FS

// Parsed once at startup: a broken template is a build mistake, and
// template.Must says so on the first run rather than on the first page load.
var pages = template.Must(template.New("pong").ParseFS(web, "web/*.html"))

// maxLobbyForm caps the lobby's form: names, sliders and fault shares.
const maxLobbyForm = 8 << 10

func (s *Server) routeUI() {
	s.mux.HandleFunc("GET /{$}", s.lobby)
	s.mux.HandleFunc("GET /ui/bot", s.botSliders)
	s.mux.HandleFunc("GET /ui/attract", s.attractCard)
	s.mux.HandleFunc("POST /ui/games", s.startFromLobby)
	s.mux.HandleFunc("GET /join", s.joinPage)
	s.mux.HandleFunc("POST /ui/join", s.joinFromForm)
	s.mux.HandleFunc("GET /play/{id}", s.playPage)
	s.mux.HandleFunc("GET /ui/games/{id}/stream", s.boardStream)
	s.mux.HandleFunc("GET /static/{file}", s.script)
}

// lobbyView is the start page.
type lobbyView struct {
	// Running is the game playing now, which the lobby offers to watch.
	Running *View
	// Attract is attract mode as the lobby tells it; nil when it is off.
	Attract *attractView
	BestOf  int
	Bots    [2]botView
	Faults  []faultField
	Correct bool
}

// botView is one bot's sliders in the lobby.
type botView struct {
	Side    string
	Preset  string
	Presets []string
	BotParams
	ReactionMax int
	SpeedMin    float64
	SpeedMax    float64
	AimMax      float64
}

func newBotView(side, preset string) botView {
	sk, ok := Preset(preset)
	if !ok {
		sk, _ = Preset(DefaultSkill)
		preset = DefaultSkill
	}
	return botView{Side: side, Preset: preset, Presets: PresetNames(), BotParams: sk.Bot(),
		ReactionMax: botReactionMS[1], SpeedMin: botSpeed[0], SpeedMax: botSpeed[1], AimMax: botAim[1]}
}

// faultField is one fault's share in the lobby, with its German name.
type faultField struct {
	Kind  FaultKind
	Label string
	Share float64
}

// faultLabels are the faults as a player reads them.
var faultLabels = map[FaultKind]string{
	FaultWrongHalf:    "falsche Hälfte",
	FaultResend:       "doppelt gesendet",
	FaultBounce:       "Prellen",
	FaultLost:         "verloren",
	FaultGhost:        "Geisterpunkt",
	FaultWrongPhase:   "falsche Phase",
	FaultCounterReset: "Zählerreset",
}

var faultOrder = []FaultKind{FaultWrongHalf, FaultResend, FaultBounce, FaultLost,
	FaultGhost, FaultWrongPhase, FaultCounterReset}

func (s *Server) lobby(w http.ResponseWriter, _ *http.Request) {
	v := lobbyView{
		BestOf:  s.cfg.BestOf,
		Bots:    [2]botView{newBotView("a", s.cfg.Skills[0].Name), newBotView("b", s.cfg.Skills[1].Name)},
		Correct: s.cfg.Faults.Correct,
	}
	f := s.cfg.Faults
	shares := f.shares()
	for _, kind := range faultOrder {
		v.Faults = append(v.Faults, faultField{Kind: kind, Label: faultLabels[kind], Share: *shares[kind]})
	}
	s.mu.Lock()
	if s.running != nil {
		rv := s.running.View()
		v.Running = &rv
	}
	s.mu.Unlock()
	v.Attract = s.attractView(time.Now())
	render(w, "lobby", v)
}

// attractView is attract mode in the lobby: on, and when the next demo starts,
// or why none is waiting.
type attractView struct {
	After string
	// In is the time left before the next demo, empty when none is waiting.
	In   string
	Demo bool
	// Blind is zaehlwerk out of sight; with In and Demo empty it is a match
	// at the table.
	Blind bool
}

func (s *Server) attractView(now time.Time) *attractView {
	st := s.Attract()
	if !st.On {
		return nil
	}
	v := &attractView{After: minutes(st.After), Blind: st.Blind}
	s.mu.Lock()
	v.Demo = s.running != nil && s.running.cfg.Attract
	s.mu.Unlock()
	if !st.Next.IsZero() && !v.Demo {
		v.In = minutes(max(st.Next.Sub(now), 0))
	}
	return v
}

// minutes says a wait the way the lobby says it: in whole minutes, rounded up
// so it never promises a demo sooner than it comes, and in seconds under one.
// Seconds above a minute would be stale before the card is fetched again.
func minutes(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d s", int(d.Round(time.Second).Seconds()))
	}
	return fmt.Sprintf("%d Min.", int((d+time.Minute-1)/time.Minute))
}

// attractCard is the lobby's attract card on its own, which the lobby fetches
// again every few seconds so its countdown stays roughly true.
func (s *Server) attractCard(w http.ResponseWriter, _ *http.Request) {
	v := s.attractView(time.Now())
	if v == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	render(w, "attract", v)
}

// botSliders is the sliders for a preset, swapped in when the lobby's preset
// changes: the preset fills them, and they can then be moved freely.
func (s *Server) botSliders(w http.ResponseWriter, r *http.Request) {
	side := r.URL.Query().Get("side")
	if side != "a" && side != "b" {
		http.Error(w, "side: want a or b", http.StatusBadRequest)
		return
	}
	render(w, "bot", newBotView(side, r.URL.Query().Get("bot_"+side+"_preset")))
}

// startFromLobby starts a browser game from the lobby's form and sends the page
// to it, with the token in the fragment: it is not sent to any server from
// there, and the game's page keeps it in this tab only.
func (s *Server) startFromLobby(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxLobbyForm)
	if err := r.ParseForm(); err != nil {
		s.lobbyError(w, "Das Formular kam nicht an.", err)
		return
	}
	cfg, faults, err := lobbyForm(r)
	if err != nil {
		s.lobbyError(w, "Das geht so nicht:", err)
		return
	}
	cfg.Faults = faults

	g, err := s.start(cfg)
	switch {
	case errors.Is(err, errBusy):
		s.lobbyError(w, "Der Tisch ist belegt. Erst das laufende Match oder Spiel beenden.", err)
		return
	case err != nil:
		s.lobbyError(w, "Zählwerk ist nicht erreichbar.", err)
		return
	}

	target := "/play/" + g.ID + "#" + g.token
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	w.Header().Set("HX-Redirect", target)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) joinPage(w http.ResponseWriter, _ *http.Request) { render(w, "join", nil) }

// joinMessages are the refusals of a join as the guest reads them.
var joinMessages = []struct {
	err error
	msg string
}{
	{errNoGuestWanted, "Gerade wartet kein Spiel auf einen Gast."},
	{errJoined, "Diesem Spiel ist schon ein Gast beigetreten."},
	{errCodeLocked, "Der Code ist nach fünf falschen Versuchen gesperrt. Auf dem Gerät des Gastgebers gibt es einen neuen."},
	{errCodeExpired, "Der Code ist abgelaufen. Auf dem Gerät des Gastgebers gibt es einen neuen."},
	{errWrongCode, "Der Code passt nicht."},
}

// joinFromForm is POST /ui/join: the code from /join, answered with the game's
// page and the guest's token in the fragment, which the guest's page keeps in
// its tab as the host's page keeps the host's.
func (s *Server) joinFromForm(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	if err := r.ParseForm(); err != nil {
		s.joinError(w, r, http.StatusBadRequest, "Das Formular kam nicht an.", err)
		return
	}
	g, token, err := s.join(strings.TrimSpace(r.FormValue("code")))
	if err != nil {
		msg, status := "Beitreten ging nicht.", http.StatusInternalServerError
		for _, m := range joinMessages {
			if errors.Is(err, m.err) {
				msg, status = m.msg, http.StatusConflict
				break
			}
		}
		if errors.Is(err, errWrongCode) || errors.Is(err, errCodeLocked) {
			status = http.StatusForbidden
		}
		s.joinError(w, r, status, msg, err)
		return
	}

	target := "/play/" + g.ID + "#" + GuestTokenPrefix + token
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	w.Header().Set("HX-Redirect", target)
	w.WriteHeader(http.StatusOK)
}

// joinError says why a join was refused. To htmx it is a 200, which it swaps,
// as the lobby's refusals are; to anything else it is the status it is.
func (s *Server) joinError(w http.ResponseWriter, r *http.Request, status int, msg string, err error) {
	s.log.Info("a join was refused", "error", err)
	if r.Header.Get("HX-Request") != "" {
		status = http.StatusOK
	}
	html, perr := partial("lobby-error", struct{ Message, Detail string }{msg, err.Error()})
	if perr != nil {
		http.Error(w, perr.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(html))
}

// lobbyForm reads the lobby's form into a browser game.
func lobbyForm(r *http.Request) (GameConfig, Faults, error) {
	var bots [2]*BotParams
	mode := r.FormValue("mode")
	if mode == ModeCheck {
		// Names, bots and faults are on the form for the other modes; a
		// check has its own names, no bots and the hardware's faults.
		return checkGame(), Faults{}, nil
	}
	for i, side := range []string{"a", "b"} {
		// Only the bots the mode has: the lobby always shows both blocks.
		isBot := mode == ModeCvC || (mode == ModePvC && side == "b")
		if !isBot {
			continue
		}
		var b BotParams
		var err error
		if b.ReactionMS, err = strconv.Atoi(r.FormValue("bot_" + side + "_reaction")); err != nil {
			return GameConfig{}, Faults{}, fmt.Errorf("bot %s: reaction: %w", side, err)
		}
		if b.Speed, err = strconv.ParseFloat(r.FormValue("bot_"+side+"_speed"), 64); err != nil {
			return GameConfig{}, Faults{}, fmt.Errorf("bot %s: speed: %w", side, err)
		}
		if b.Aim, err = strconv.ParseFloat(r.FormValue("bot_"+side+"_aim"), 64); err != nil {
			return GameConfig{}, Faults{}, fmt.Errorf("bot %s: aim: %w", side, err)
		}
		bots[i] = &b
	}
	cfg, err := browserGame(mode, [2]string{r.FormValue("name_a"), r.FormValue("name_b")}, bots)
	if err != nil {
		return cfg, Faults{}, err
	}

	var f Faults
	shares := f.shares()
	for _, kind := range faultOrder {
		raw := strings.TrimSpace(r.FormValue("fault_" + string(kind)))
		if raw == "" {
			continue
		}
		v, err := strconv.ParseFloat(strings.ReplaceAll(raw, ",", "."), 64)
		if err != nil {
			return cfg, f, fmt.Errorf("fault %s=%q: want a share from 0 to 1", kind, raw)
		}
		*shares[kind] = v
	}
	f.Correct = r.FormValue("correct") != ""
	return cfg, f, f.Validate()
}

func (s *Server) lobbyError(w http.ResponseWriter, msg string, err error) {
	s.log.Info("lobby refused a start", "error", err)
	// 200 rather than a 4xx: htmx does not swap an error response, and a
	// start button that silently does nothing is worse than one that says why.
	render(w, "lobby-error", struct{ Message, Detail string }{msg, err.Error()})
}

// playView is the game's page.
type playView struct {
	Board boardView
	// Bots is the bots as JSON for the canvas, player a and b.
	Bots string
}

func (s *Server) playPage(w http.ResponseWriter, r *http.Request) {
	g, ok := s.game(r.PathValue("id"))
	if !ok {
		http.Error(w, "Dieses Spiel gibt es nicht (mehr).", http.StatusNotFound)
		return
	}
	v := g.View()
	if v.Driver != DriverBrowser {
		// A headless game and a button check have no canvas to draw; their
		// page is the board alone.
		v.Mode = ""
	}
	bots, err := json.Marshal(v.Bots)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, "play", playView{Board: newBoardView(v), Bots: string(bots)})
}

// boardStream is the board as rendered HTML over SSE, for htmx to swap.
func (s *Server) boardStream(w http.ResponseWriter, r *http.Request) {
	g, ok := s.game(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such game", http.StatusNotFound)
		return
	}
	s.follow(w, r, g, func(v View) ([]sseEvent, error) {
		b := newBoardView(v)
		var events []sseEvent
		names := []string{"score", "truth"}
		if b.Check != nil {
			names = []string{"score", "check"}
		}
		for _, name := range names {
			html, err := partial(name, b)
			if err != nil {
				return nil, err
			}
			events = append(events, sseEvent{Name: name, Data: html})
		}
		return events, nil
	})
}

func (s *Server) script(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if name != "game.js" && name != "check.js" && name != "tab.js" && name != "saver.js" {
		http.NotFound(w, r)
		return
	}
	js, err := web.ReadFile("web/" + name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	// Shipped with the binary, so a new version is a new binary: no-cache
	// keeps a browser from playing yesterday's game against today's service.
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(js)
}

// boardView is the banner, the score and truth against zaehlwerk.
type boardView struct {
	View
	Banner string
	// Over is a game that has stopped, however it stopped.
	Over        bool
	Left, Right sideView
	Compare     []playerRow
	Rows        []divergenceRow
	FaultRows   []faultRow
	Findings    int
	// Check is a button check's prompt and findings, nil for a game.
	Check *checkBoard
}

// checkBoard is a button check on its page.
type checkBoard struct {
	CheckView
	// Prompt is the prompt as the table reads it: "LINKS drücken".
	Prompt string
	For    string
	Ready  bool
	Rows   []checkRow
	Counts []checkCount
}

type checkRow struct {
	CheckFinding
	At, Label string
}

type checkCount struct {
	Label string
	N     int
	Bad   bool
}

// checkPageLabels are the outcomes as the page says them.
var checkPageLabels = map[CheckOutcome]string{
	CheckOK:         "richtig",
	CheckWrongHalf:  "falsche Hälfte",
	CheckDouble:     "doppelt (Prellen)",
	CheckMissing:    "fehlt",
	CheckUnexpected: "ohne Ansage",
}

var halfLabels = map[scorer.Half]string{scorer.HalfLeft: "LINKS", scorer.HalfRight: "RECHTS"}

func newCheckBoard(c *CheckView) *checkBoard {
	b := &checkBoard{CheckView: *c}
	if p := c.Prompt; p != nil {
		b.For, b.Ready = p.For, p.Kind == PressReady
		if b.Ready {
			b.Prompt = "Bereit: " + halfLabels[p.Half]
		} else {
			b.Prompt = halfLabels[p.Half] + " drücken"
		}
	}
	for _, o := range checkOutcomes {
		b.Counts = append(b.Counts, checkCount{Label: checkPageLabels[o], N: c.Counts[o], Bad: o != CheckOK && c.Counts[o] > 0})
	}
	for _, f := range c.Findings {
		b.Rows = append(b.Rows, checkRow{CheckFinding: f, At: f.At.Format("15:04:05.000"), Label: checkPageLabels[f.Outcome]})
	}
	return b
}

type sideView struct {
	Name    string
	Points  int
	Sets    int
	Serving bool
	// Waiting is zaehlwerk waiting for this half's key.
	Waiting bool
}

// playerRow is one player in the comparison: what really happened, what
// zaehlwerk says, and what the faults say zaehlwerk should say.
type playerRow struct {
	Name      string
	Truth     int
	Zaehlwerk int
	Expected  int
}

type divergenceRow struct {
	Divergence
	What string
	// First marks the first finding, the rally to look at.
	First bool
}

type faultRow struct {
	Fault
	Name   string
	Effect string
}

var whatLabels = map[string]string{
	"points":                  "Punkte",
	"serve":                   "Aufschlag",
	"ends":                    "Seiten",
	"undo":                    "Undo",
	string(FaultResend):       "doppelt gesendet",
	string(FaultWrongPhase):   "falsche Phase",
	string(FaultCounterReset): "Zählerreset",
}

// maxFaultRows is how many faults the board lists, newest first.
const maxFaultRows = 12

func newBoardView(v View) boardView {
	st := v.State
	b := boardView{View: v, Over: v.Status != StatusPlaying, Banner: banner(v)}
	if v.Check != nil {
		b.Check = newCheckBoard(v.Check)
	}

	name := func(p scorer.Player) string {
		if p == "" {
			return ""
		}
		return st.Players[index(p)]
	}
	ends := st.Ends
	if ends == ([2]scorer.Player{}) {
		ends = [2]scorer.Player{scorer.PlayerA, scorer.PlayerB}
	}
	side := func(p scorer.Player) sideView {
		return sideView{Name: name(p), Points: st.Points[index(p)], Sets: st.Sets[index(p)],
			Serving: st.Phase == scorer.PhasePlaying && st.Serving == p}
	}
	b.Left, b.Right = side(ends[0]), side(ends[1])
	b.Left.Waiting, b.Right.Waiting = waiting(st, scorer.HalfLeft), waiting(st, scorer.HalfRight)

	for i := range 2 {
		b.Compare = append(b.Compare, playerRow{Name: st.Players[i], Truth: v.Truth[i],
			Zaehlwerk: v.Zaehlwerk[i], Expected: v.Truth[i] + v.Explained[i]})
	}

	first := true
	for _, d := range v.Divergences {
		row := divergenceRow{Divergence: d, What: whatLabels[d.What]}
		if row.What == "" {
			row.What = d.What
		}
		if !d.Expected {
			b.Findings++
			row.First, first = first, false
		}
		b.Rows = append(b.Rows, row)
	}

	for i := len(v.Faults) - 1; i >= 0 && len(b.FaultRows) < maxFaultRows; i-- {
		f := v.Faults[i]
		b.FaultRows = append(b.FaultRows, faultRow{Fault: f, Name: faultLabels[f.Kind], Effect: effect(f.Effect)})
	}
	return b
}

// banner is the panel's German text for where the match stands.
func banner(v View) string {
	st := v.State
	switch v.Status {
	case StatusChecked:
		return "TASTER GEPRÜFT"
	case StatusEnded:
		return "SPIEL BEENDET"
	case StatusFailed:
		return "FEHLER"
	}
	switch st.Phase {
	case scorer.PhaseWarmup:
		return "EINSPIELEN"
	case scorer.PhaseServeRally:
		return "UM DIE ANGABE"
	case scorer.PhaseChangeEnds:
		return "SEITENWECHSEL"
	case scorer.PhasePlaying:
		if st.Serving != "" {
			return "ANGABE " + st.Players[index(st.Serving)]
		}
	case scorer.PhaseComplete:
		if st.Winner != "" {
			return "SIEG " + st.Players[index(st.Winner)]
		}
	}
	return ""
}

func effect(e [2]int) string {
	if e == ([2]int{}) {
		return "–"
	}
	return fmt.Sprintf("%+d : %+d", e[0], e[1])
}

func render(w http.ResponseWriter, name string, v any) {
	html, err := partial(name, v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(html))
}

func partial(name string, v any) (string, error) {
	var buf bytes.Buffer
	if err := pages.ExecuteTemplate(&buf, name, v); err != nil {
		return "", fmt.Errorf("rendering %s: %w", name, err)
	}
	return buf.String(), nil
}

// waiting says whether zaehlwerk waits for a half's press, for the board.
func waiting(st scorer.State, h scorer.Half) bool { return slices.Contains(st.Waiting, h) }
