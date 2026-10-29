package pong

import (
	"bytes"
	"context"
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
	"unicode/utf8"

	"github.com/stuttgart-things/zaehlwerk/internal/face"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
	"github.com/stuttgart-things/zaehlwerk/internal/ui/static"
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
	s.mux.HandleFunc("POST /ui/demo", s.demoButton)
	s.mux.HandleFunc("POST /ui/league", s.leagueButton)
	s.mux.HandleFunc("GET /join", s.joinPage)
	s.mux.HandleFunc("POST /ui/join", s.joinFromForm)
	s.mux.HandleFunc("GET /play/{id}", s.playPage)
	s.mux.HandleFunc("GET /live", s.livePage)
	s.mux.HandleFunc("GET /ui/games/{id}/stream", s.boardStream)
	if s.figures != nil {
		s.mux.HandleFunc("GET /ui/games/{id}/figures/{side}", s.figure)
	}
	s.mux.HandleFunc("GET /static/{file}", s.script)
	s.mux.HandleFunc("GET /static/mark-32.png", s.mark)
	s.mux.HandleFunc("GET /static/mark-180.png", s.mark)
}

// lobbyView is the start page.
type lobbyView struct {
	// Running is the game playing now, which the lobby offers to watch.
	Running *View
	// Attract is attract mode as the lobby tells it; nil when it is off.
	Attract *attractView
	// League is league mode as the lobby tells it; nil when it is off.
	League  *leagueView
	Bots    [2]botView
	Faults  []faultField
	Correct bool
	// Lengths is the choice of how long a game lasts.
	Lengths []lengthOption
	// Players is the names Schmetterpause knows, offered in the name
	// fields; empty when the coupling is off or the list could not be read.
	Players []string
	// PlayersErr says the list is missing although the coupling is on.
	PlayersErr bool
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

func (s *Server) lobby(w http.ResponseWriter, r *http.Request) {
	v := lobbyView{
		Bots:    [2]botView{newBotView("a", s.cfg.Skills[0].Name), newBotView("b", s.cfg.Skills[1].Name)},
		Correct: s.cfg.Faults.Correct,
		Lengths: lengthOptions(s.cfg.BestOf),
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
	v.League = s.leagueView()
	v.Players, v.PlayersErr = s.playerNames(r.Context())
	render(w, "lobby", v)
}

// leagueView is league mode in the lobby: playing or stopped.
type leagueView struct {
	Held bool
}

func (s *Server) leagueView() *leagueView {
	if s.cfg.League == nil {
		return nil
	}
	return &leagueView{Held: s.leagueHeldNow()}
}

// leagueButton is the lobby's stop and play for league mode: action=stop ends
// the match playing and starts none after it, action=play lets the league go
// on. htmx swaps the card; a page without it is sent back to the lobby.
func (s *Server) leagueButton(w http.ResponseWriter, r *http.Request) {
	if s.cfg.League == nil {
		http.Error(w, "league mode is off", http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLobbyForm)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch r.PostForm.Get("action") {
	case "stop":
		s.holdLeague()
	case "play":
		s.resumeLeague()
	default:
		http.Error(w, "action: want stop or play", http.StatusBadRequest)
		return
	}
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	render(w, "league", s.leagueView())
}

// playerNames is who the lobby offers, read from Schmetterpause on every load
// and never cached, as zaehlwerk's scoring page reads it (ADR-0004). Names
// only, for what a game is called: a pong game is a demo, and nothing it plays
// is reported (ADR-0008). A name longer than a game takes is left out rather
// than cut, because a cut name would be nobody's. A list that cannot be read
// leaves the fields free, as they are without the coupling.
func (s *Server) playerNames(ctx context.Context) ([]string, bool) {
	if s.cfg.Roster == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, rosterTimeout)
	defer cancel()
	players, err := s.cfg.Roster.Players(ctx)
	if err != nil {
		s.log.Warn("the lobby offers no players, Schmetterpause did not answer", "error", err)
		return nil, true
	}
	names := make([]string, 0, len(players))
	for _, p := range players {
		if p.DisplayName != "" && utf8.RuneCountInString(p.DisplayName) <= maxNameRunes {
			names = append(names, p.DisplayName)
		}
	}
	slices.SortFunc(names, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	return names, false
}

// rosterTimeout bounds the lobby's wait for the list. The page is the thing
// somebody is waiting for; without names it still starts a game.
const rosterTimeout = 2 * time.Second

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
	cfg, faults, err := lobbyForm(r, s.cfg.BestOf)
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

// demoButton is zaehlwerk's "Demo" button: a served bot game (ADR-0014) with
// the lobby's defaults, sent to its page in the tab the button opened. Served,
// because that tab is the one most likely to be left in the background, and a
// game the page plays would stand still there. A game already playing is
// shown instead of being refused: whoever pressed the button wanted something
// to watch, and that is it. The form comes from another origin; it starts
// nothing the lobby would not start for anybody, so it needs nothing to prove.
func (s *Server) demoButton(w http.ResponseWriter, r *http.Request) {
	cfg, err := servedGame(startBody{})
	if err != nil {
		// Only reachable if the bots' names stop passing browserGame's checks.
		s.demoRefused(w, http.StatusInternalServerError, "Die Demo ließ sich nicht anlegen.", err)
		return
	}
	cfg.Faults = s.cfg.Faults

	g, err := s.start(cfg)
	if errors.Is(err, errBusy) {
		s.mu.Lock()
		running := s.running
		s.mu.Unlock()
		if running != nil {
			http.Redirect(w, r, "/play/"+running.ID, http.StatusSeeOther)
			return
		}
		// Busy with nothing of ours playing: a real match holds the table,
		// and a demo never takes it from one (ADR-0008).
		s.demoRefused(w, http.StatusConflict, "Am Tisch läuft ein echtes Match. Die Demo wartet, bis es vorbei ist.", err)
		return
	}
	if err != nil {
		s.demoRefused(w, http.StatusBadGateway, "Zählwerk ist nicht erreichbar.", err)
		return
	}
	http.Redirect(w, r, "/play/"+g.ID+"#"+g.token, http.StatusSeeOther)
}

// demoRefused says why the button started nothing, as a whole page: it is the
// first and only thing the new tab shows.
func (s *Server) demoRefused(w http.ResponseWriter, status int, msg string, err error) {
	s.log.Info("the demo button started nothing", "error", err)
	html, perr := partial("demo-refused", struct{ Message, Detail string }{msg, err.Error()})
	if perr != nil {
		http.Error(w, perr.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(html))
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

// lobbyLengths are the lengths the lobby offers: a single set to eleven, the
// quick game at a screen, and the two lengths a match at the table has. The
// service's PONG_BEST_OF is offered too when it is none of them, and is what
// the lobby starts with.
var lobbyLengths = []int{1, 3, 5}

// lengthOption is one length in the lobby.
type lengthOption struct {
	BestOf  int
	Label   string
	Checked bool
}

func lengthOptions(def int) []lengthOption {
	lengths := slices.Clone(lobbyLengths)
	if !slices.Contains(lengths, def) {
		lengths = append(lengths, def)
		slices.Sort(lengths)
	}
	opts := make([]lengthOption, 0, len(lengths))
	for _, n := range lengths {
		label := fmt.Sprintf("Best of %d", n)
		if n == 1 {
			label = "1 Satz"
		}
		opts = append(opts, lengthOption{BestOf: n, Label: label, Checked: n == def})
	}
	return opts
}

// lobbyForm reads the lobby's form into a browser game. def is the service's
// PONG_BEST_OF, which the lobby offers too.
func lobbyForm(r *http.Request, def int) (GameConfig, Faults, error) {
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
	if raw := r.FormValue("best_of"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || (!slices.Contains(lobbyLengths, n) && n != def) {
			return cfg, Faults{}, fmt.Errorf("best of %q: want one the lobby offers", raw)
		}
		cfg.BestOf = n
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
	cfg.Diagonal = r.FormValue("diagonal") != ""
	if r.FormValue("served") != "" {
		// Asked for under "Computer gegen Computer" only, and checked by the
		// same rules as POST /games.
		served, err := servedGame(startBody{Mode: mode, Diagonal: cfg.Diagonal})
		if err != nil {
			return cfg, f, err
		}
		cfg.Driver = served.Driver
	}
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
	// Live is the page at /live: it watches whatever plays, and moves on to
	// the next game by itself once this one is over.
	Live bool
}

func (s *Server) playPage(w http.ResponseWriter, r *http.Request) {
	g, ok := s.game(r.PathValue("id"))
	if !ok {
		http.Error(w, "Dieses Spiel gibt es nicht (mehr).", http.StatusNotFound)
		return
	}
	s.renderPlay(w, g, false)
}

// livePage is GET /live: one address for a screen that stays on, the TV next
// to the table say. It draws the game playing now, on the same page as
// /play/{id}, and live.js reloads it once that game is over, so the next one
// shows without anybody touching the screen. Between games -- the league's
// pause, or no game at all -- it waits and looks again.
func (s *Server) livePage(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	g := s.running
	s.mu.Unlock()
	if g == nil {
		render(w, "live-wait", s.leagueView())
		return
	}
	s.renderPlay(w, g, true)
}

func (s *Server) renderPlay(w http.ResponseWriter, g *Game, live bool) {
	v := g.View()
	if v.Driver != DriverBrowser && v.Driver != DriverServer {
		// A headless game and a button check have no canvas to draw; their
		// page is the board alone.
		v.Mode = ""
	}
	bots, err := json.Marshal(v.Bots)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	b := newBoardView(v)
	rest, _ := faces(g, nil, v.State, face.Faces{}, face.Streak{})
	s.dress(&b, g, rest)
	render(w, "play", playView{Board: b, Bots: string(bots), Live: live})
}

// boardStream is the board as rendered HTML over SSE, for htmx to swap.
func (s *Server) boardStream(w http.ResponseWriter, r *http.Request) {
	g, ok := s.game(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such game", http.StatusNotFound)
		return
	}
	// The faces' memory, this connection's: the state it saw last, the faces
	// it showed, and the points in a row (zaehlwerk ADR-0018).
	var (
		prev   *scorer.State
		shown  face.Faces
		streak face.Streak
	)
	s.follow(w, r, g, func(v View) ([]sseEvent, error) {
		b := newBoardView(v)
		shown, streak = faces(g, prev, v.State, shown, streak)
		prev = &v.State
		s.dress(&b, g, shown)
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
	if name != "game.js" && name != "serve.js" && name != "check.js" && name != "tab.js" && name != "saver.js" && name != "live.js" {
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

// mark serves Zählwerk's mark, the favicon and the home-screen icon, from the
// same files the scoring page serves: the game is Zählwerk's too, and a tab
// with the browser's blank page icon is the one tab nobody finds again.
func (s *Server) mark(w http.ResponseWriter, r *http.Request) {
	png, err := static.FS.ReadFile(strings.TrimPrefix(r.URL.Path, "/static/"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	// A day, as on the scoring page: the names carry no hash.
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(png)
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
	// Blades is each player's blade colour, a then b, from their figure:
	// the canvas fills their paddle with it. Empty without figures or before
	// they have arrived, and the canvas then gives both its own colour.
	Blades [2]string
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
	// player is who plays at this end, for the figure.
	player  scorer.Player
	Name    string
	Points  int
	Sets    int
	Serving bool
	// Waiting is zaehlwerk waiting for this half's key.
	Waiting bool
	// Figure is the address of this side's figure with its face, empty
	// without figures.
	Figure string
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
		return sideView{player: p, Name: name(p), Points: st.Points[index(p)], Sets: st.Sets[index(p)],
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
