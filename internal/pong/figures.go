package pong

import (
	"context"
	"net/http"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/face"
	"github.com/stuttgart-things/zaehlwerk/internal/figure"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
	"github.com/stuttgart-things/zaehlwerk/internal/ui/static"
)

// The players' figures on the game's board, as on zaehlwerk's spectator view
// (zaehlwerk ADR-0018, Schmetterpause ADR-0029): each side's figure above its
// name, with the face the match gives it.
//
// A league match is between two of Schmetterpause's players, and each side
// shows that player's figure. Every other game has names only; a name
// Schmetterpause knows shows that player's figure, any other a random one,
// drawn once per game. Without Config.Figures the board is what it was.

// figuresOf starts fetching g's figures unless that has already happened, and
// returns them, ready or not.
func (s *Server) figuresOf(g *Game) *figure.Pair {
	return s.figures.Ensure(g.ID, func(ctx context.Context) [2]string { return s.figureIDs(ctx, g) })
}

// figureIDs is whose figures g shows, home first: a league match's players by
// their ids, any other game's by name. The ids only pick a drawing; the game
// still reports nothing (ADR-0008).
func (s *Server) figureIDs(ctx context.Context, g *Game) [2]string {
	if h := g.cfg.Handover; h != nil {
		return [2]string{h.HomeID, h.AwayID}
	}
	var ids [2]string
	if s.cfg.Roster == nil {
		return ids
	}
	players, err := s.cfg.Roster.Players(ctx)
	if err != nil {
		// Info, not a warning: without SCHMETTERPAUSE_TOKEN this is every
		// game, and the lobby already says so.
		s.log.Info("no players to look the names up in, both sides get a random figure", "game", g.ID, "error", err)
		return ids
	}
	names := g.View().Players
	for i, name := range names {
		for _, p := range players {
			if p.DisplayName == name {
				ids[i] = p.ID
				break
			}
		}
	}
	return ids
}

// dress gives b's sides their figures with faces, and its players the blades
// their figures carry, when figures are on. A button check has no players,
// and no figures.
func (s *Server) dress(b *boardView, g *Game, faces face.Faces) {
	if s.figures == nil || b.Check != nil {
		return
	}
	s.figuresOf(g)
	if figs := s.figures.Ready(g.ID); figs != nil {
		b.Blades = [2]string{figs.Side[0].Blade, figs.Side[1].Blade}
	}
	for _, sv := range []*sideView{&b.Left, &b.Right} {
		side := "a"
		if sv.player == scorer.PlayerB {
			side = "b"
		}
		sv.Figure = "/ui/games/" + g.ID + "/figures/" + side + ".svg?face=" + string(faces[index(sv.player)])
	}
}

// faces is the faces at cur, which came after prev on the same stream, where
// last is what that stream showed: the face package reads the change between
// the two states as the moment it was. A stream that has seen nothing yet is a
// resting score. A change that is no point, set or match keeps the faces
// shown: the board goes out with every rally, and a point's faces stay up
// until the next point, as they do on zaehlwerk's stream.
func faces(g *Game, prev *scorer.State, cur scorer.State, last face.Faces, streak face.Streak) (face.Faces, face.Streak) {
	var kind scorer.TransitionKind
	var side scorer.Player
	if prev != nil {
		if kind, side = change(*prev, cur); kind == "" {
			return last, streak
		}
	}
	m := face.Moment{Kind: kind, Side: side, State: cur}
	if sc := rules(g); sc != nil {
		m.Stake, _ = sc.Stake(cur)
		_, m.PointsPerSet = sc.Rules()
	}
	return face.Read(m, streak)
}

// change is what happened between two states of one match, as zaehlwerk's
// stream would have said it. The game's stream sends the state, not the
// transitions, and a quick run of changes can arrive as one: then it is the
// last of them, which is the one the faces are about.
func change(prev, cur scorer.State) (scorer.TransitionKind, scorer.Player) {
	switch {
	case cur.Complete && !prev.Complete:
		return scorer.TransitionMatchWon, cur.Winner
	case cur.Sets[0]+cur.Sets[1] > prev.Sets[0]+prev.Sets[1]:
		if cur.Sets[0] > prev.Sets[0] {
			return scorer.TransitionSetWon, scorer.PlayerA
		}
		return scorer.TransitionSetWon, scorer.PlayerB
	case cur.Sets != prev.Sets || cur.Points[0] < prev.Points[0] || cur.Points[1] < prev.Points[1]:
		return scorer.TransitionUndo, ""
	case cur.Points[0] > prev.Points[0] && cur.Points[1] == prev.Points[1]:
		return scorer.TransitionPoint, scorer.PlayerA
	case cur.Points[1] > prev.Points[1] && cur.Points[0] == prev.Points[0]:
		return scorer.TransitionPoint, scorer.PlayerB
	}
	return "", ""
}

// rules is a scorer with g's rules, asked about the stake only: the scorer is
// the only thing with an opinion about what a point decides (invariant 5).
// Nil for rules zaehlwerk would have refused, where nothing is at stake.
func rules(g *Game) *scorer.Scorer {
	sc, err := scorer.New(scorer.Config{MatchID: g.ID, BestOf: g.cfg.BestOf, PointsPerSet: g.cfg.PointsPerSet})
	if err != nil {
		return nil
	}
	return sc
}

// figure serves one side's figure with one face.
//
//	GET /ui/games/{id}/figures/{side}?face=happy    side is a.svg or b.svg
//
// An <img> rather than the drawing inlined, as on zaehlwerk's spectator view:
// the board goes out with every rally, a figure is 40 KB, and each face is
// fetched once and then comes from the browser's cache. A side whose figure
// could not be fetched gets Zählwerk's mascot.
func (s *Server) figure(w http.ResponseWriter, r *http.Request) {
	var i int
	switch r.PathValue("side") {
	case "a.svg":
		i = 0
	case "b.svg":
		i = 1
	default:
		http.NotFound(w, r)
		return
	}
	g, ok := s.game(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such game", http.StatusNotFound)
		return
	}
	f := figure.Asked(r.URL.Query().Get("face"))

	figs := s.figuresOf(g)
	select {
	case <-figs.Done:
	case <-r.Context().Done():
		return
	case <-time.After(figure.Wait):
	}

	h := w.Header()
	for k, v := range figure.Headers {
		h.Set(k, v)
	}
	select {
	case <-figs.Done:
		if svg := figs.Side[i].SVG; svg != nil {
			// Fixed for the game, face by face.
			h.Set("Cache-Control", "private, max-age=3600")
			_, _ = w.Write(figure.WithFace(svg, f))
			return
		}
	default:
	}
	// Not stored: the next rally asks again, and gets the figure once the
	// fetch is through.
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(static.Mascot))
}
