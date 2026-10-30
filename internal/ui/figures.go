package ui

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/face"
	"github.com/stuttgart-things/zaehlwerk/internal/figure"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// WithFigures shows each side's figure on the spectator view, with the face
// the match gives it (ADR-0018), and takes the referee's cord from the blade
// the figure carries. Without it the view is what it was: no figures, and the
// blade recomputed from the player id.
func WithFigures(f figure.Source) Option {
	return func(s *Server) { s.figureSrc = f }
}

// figuresOf starts fetching m's figures unless that has already happened, and
// returns them, ready or not.
func (s *Server) figuresOf(m *match.Match) *figure.Pair {
	ids := m.Figures
	return s.figures.Ensure(m.ID, func(context.Context) [2]string { return ids })
}

// Colours is the colour of each side of m as this view draws it, for the
// panel (panel.Sink.ColourFrom): the blade each figure carries, see
// [Server.blades], so a player has the same colour on the matrix as on this
// screen and on the game's board. It waits for the figures until ctx is done,
// and then takes what blades has.
func (s *Server) Colours(ctx context.Context, m *match.Match) [2]string {
	if s.figures != nil {
		select {
		case <-s.figuresOf(m).Done:
		case <-ctx.Done():
		}
	}
	a, b := s.blades(m)
	return [2]string{a, b}
}

// figure serves one side's figure with one face.
//
//	GET /ui/matches/{id}/figures/{side}?face=happy    side is a.svg or b.svg
//
// The spectator view points an <img> here rather than inlining the drawing:
// the partial goes out with every point, a figure is 40 KB, and as an image
// each face is fetched once and then comes from the browser's cache. An <img>
// also never runs anything in it.
//
// A side whose figure could not be fetched gets the mascot in its colour, so a
// Schmetterpause that is down shows the screen it had before figures existed.
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
	m, err := s.registry.Get(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	f := figure.Asked(r.URL.Query().Get("face"))

	figs := s.figuresOf(m)
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
			// Fixed for the match, face by face, so the browser may keep it
			// for as long as a match lasts.
			h.Set("Cache-Control", "private, max-age=3600")
			_, _ = w.Write(figure.WithFace(svg, f))
			return
		}
	default:
	}

	// Not stored: the next point asks again, and gets the figure once the
	// fetch is through.
	h.Set("Cache-Control", "no-store")
	a, b := s.blades(m)
	colour := []string{a, b}[i]
	_, _ = fmt.Fprint(w, mascotIn(colour))
}

// mascotIn is the mascot drawing as a document of its own, its cord in colour.
func mascotIn(colour string) string {
	return string(bytes.Replace([]byte(mascotSVG), []byte(`<svg id="mascot"`),
		[]byte(`<svg id="mascot" style="--paddle: `+colour+`"`), 1))
}

// dress gives v's sides their figures with faces, when figures are on. It
// starts the fetch on the first render of a match, so that by the time the
// browser asks for the images they are usually there.
func (s *Server) dress(v *matchView, m *match.Match, faces face.Faces) {
	if s.figures == nil {
		return
	}
	s.figuresOf(m)
	v.A.Figure = figureURL(m.ID, "a", faces[0])
	v.B.Figure = figureURL(m.ID, "b", faces[1])
}

func figureURL(id, side string, f face.Face) string {
	return "/ui/matches/" + id + "/figures/" + side + ".svg?face=" + string(f)
}

// moment is st, an event of m, as the face package reads it. The rules come
// from m's scorer, which is the only thing with an opinion about them.
func moment(m *match.Match, kind string, side scorer.Player, st scorer.State) face.Moment {
	stake, _ := m.Scorer.Stake(st)
	_, perSet := m.Scorer.Rules()
	return face.Moment{Kind: scorer.TransitionKind(kind), Side: side, State: st, Stake: stake, PointsPerSet: perSet}
}
