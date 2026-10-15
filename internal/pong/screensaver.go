package pong

import (
	"errors"
	"net/http"
)

// The screensaver (ADR-0013, #120): a page for a screen at the table that
// starts a browser game between two bots as soon as it is opened, and the next
// one when that is over. Deciding when the table has been free long enough is
// not its business: zaehlwerk's watch screen does that, and shows this page
// while it holds (zaehlwerk #128). Opened on its own, it plays one demo after
// another for as long as the table lets it.
//
// A screensaver game is a demo like attract mode's: never reported, given up
// to any real match, any game somebody starts in the lobby, and anybody
// counting into it. It differs in one thing, which is the point of it: the
// rallies are played on a canvas, so the screen shows the rally the panel
// scores.

func (s *Server) routeScreensaver() {
	s.mux.HandleFunc("GET /screensaver", s.screensaverPage)
	s.mux.HandleFunc("POST /ui/screensaver", s.startScreensaver)
}

func (s *Server) screensaverPage(w http.ResponseWriter, _ *http.Request) {
	render(w, "screensaver", nil)
}

// startScreensaver starts the screensaver's game and sends the page to it,
// with the token in the fragment as the lobby does. A table that is taken is
// said on the page, which asks again later.
func (s *Server) startScreensaver(w http.ResponseWriter, r *http.Request) {
	cfg, err := browserGame(ModeCvC, [2]string{}, [2]*BotParams{})
	if err != nil {
		// Only reachable if the bots' names stop passing browserGame's checks.
		s.screensaverWait(w, err)
		return
	}
	cfg.Screensaver = true

	g, err := s.start(cfg)
	if err != nil {
		s.screensaverWait(w, err)
		return
	}
	s.log.Info("screensaver started a demo", "game", g.ID, "match_id", g.matchIDNow())
	w.Header().Set("HX-Redirect", "/play/"+g.ID+"#"+g.token)
	w.WriteHeader(http.StatusOK)
}

// screensaverWait says why no demo started, and has the page ask again. A
// busy table is the ordinary case: somebody is playing, which is what the
// table is for.
func (s *Server) screensaverWait(w http.ResponseWriter, err error) {
	msg := "Zählwerk ist nicht erreichbar."
	if errors.Is(err, errBusy) {
		msg = "Der Tisch ist belegt."
		s.log.Debug("screensaver waits, the table is taken", "reason", err.Error())
	} else {
		s.log.Warn("screensaver could not start a demo", "error", err)
	}
	render(w, "screensaver-wait", msg)
}
