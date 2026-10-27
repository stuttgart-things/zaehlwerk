package ui

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/face"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// Figures is where the players' figures come from: Schmetterpause, which lets
// every player assemble one (Schmetterpause ADR-0029). An empty id asks for a
// random figure, for a side with nobody behind it.
type Figures interface {
	Figure(ctx context.Context, id string) ([]byte, error)
}

// WithFigures shows each side's figure on the spectator view, with the face
// the match gives it (ADR-0018), and takes the referee's cord from the blade
// the figure carries. Without it the view is what it was: no figures, and the
// blade recomputed from the player id.
func WithFigures(f Figures) Option {
	return func(s *Server) { s.figures = newFigureCache(f) }
}

const (
	// figureMatches is how many matches' figures are kept. One table has one
	// running match and the one before it on a screen still holding its
	// result; more is room for a page left open on an old ?match=.
	figureMatches = 8

	// figureWait bounds how long the figure route waits for the fetch it
	// started, a little over the client's own timeout. Past it the side gets
	// the mascot.
	figureWait = 6 * time.Second
)

// figureCache holds each match's two figures, fetched once per match.
//
// Once rather than per request because a random figure is a new one every
// time it is asked for, and because a figure that changed mid-match because
// its player saved a new one would be a figure that is not the one that
// started. Only in memory and bounded (invariant 2): a match that falls out
// fetches again if anybody asks.
type figureCache struct {
	src Figures

	mu      sync.Mutex
	byMatch map[string]*matchFigures
	order   []string
}

// matchFigures is one match's two figures. side is written once, by the fetch,
// before done is closed; nothing reads it before.
type matchFigures struct {
	done chan struct{}
	side [2]figure
}

// figure is one side's drawing and the blade it carries. svg is nil when the
// fetch failed, and that side shows the mascot.
type figure struct {
	svg   []byte
	blade string
}

func newFigureCache(src Figures) *figureCache {
	return &figureCache{src: src, byMatch: make(map[string]*matchFigures)}
}

// ensure starts fetching m's figures unless that has already happened, and
// returns them, ready or not. It never waits: it is called while a point is
// being rendered.
func (c *figureCache) ensure(m *match.Match, log *slog.Logger) *matchFigures {
	c.mu.Lock()
	defer c.mu.Unlock()

	if f, ok := c.byMatch[m.ID]; ok {
		return f
	}
	f := &matchFigures{done: make(chan struct{})}
	c.byMatch[m.ID] = f
	c.order = append(c.order, m.ID)
	if len(c.order) > figureMatches {
		delete(c.byMatch, c.order[0])
		c.order = c.order[1:]
	}

	ids := [2]string{m.Handover.HomeID, m.Handover.AwayID}
	go func() {
		defer close(f.done)
		for i, id := range ids {
			// Its own deadline, not a request's: the fetch outlives whichever
			// render or request started it, and serves every later one.
			ctx, cancel := context.WithTimeout(context.Background(), figureWait)
			svg, err := c.src.Figure(ctx, id)
			cancel()
			if err != nil {
				log.Warn("figure unavailable, the side shows the mascot", "match", m.ID, "side", i, "error", err)
				continue
			}
			f.side[i] = figure{svg: svg, blade: bladeOf(svg)}
		}
	}()
	return f
}

// ready is m's figures if they have arrived, else nil.
func (c *figureCache) ready(id string) *matchFigures {
	c.mu.Lock()
	f := c.byMatch[id]
	c.mu.Unlock()
	if f == nil {
		return nil
	}
	select {
	case <-f.done:
		return f
	default:
		return nil
	}
}

// paddleProp is the blade colour on a figure's root: Schmetterpause puts the
// colours there as custom properties, the blade as --paddle.
var paddleProp = regexp.MustCompile(`--paddle:\s*(#[0-9a-fA-F]{6})\b`)

// bladeOf is the blade a figure carries, or empty when it says none.
func bladeOf(svg []byte) string {
	m := paddleProp.FindSubmatch(svg)
	if m == nil {
		return ""
	}
	return string(bytes.ToLower(m[1]))
}

// dataFace is the face selector on a figure's root. The figure carries every
// face, and a scoped <style> inside it shows the one named here.
var dataFace = regexp.MustCompile(`data-face="[a-z-]*"`)

// withFace is svg showing f.
func withFace(svg []byte, f face.Face) []byte {
	attr := []byte(`data-face="` + string(f) + `"`)
	if loc := dataFace.FindIndex(svg); loc != nil {
		return append(append(append([]byte{}, svg[:loc[0]]...), attr...), svg[loc[1]:]...)
	}
	// A figure without the attribute shows its default face, which is fine;
	// adding it costs nothing and keeps the face this side was given.
	return bytes.Replace(svg, []byte("<svg"), append([]byte("<svg "), attr...), 1)
}

// knownFaces is what ?face= may ask for. Anything else is neutral, so the
// query string never reaches the document unchecked.
var knownFaces = map[face.Face]bool{
	face.Neutral: true, face.Happy: true, face.Surprised: true,
	face.Skeptical: true, face.Angry: true, face.Sad: true,
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
	f := face.Face(r.URL.Query().Get("face"))
	if !knownFaces[f] {
		f = face.Neutral
	}

	figs := s.figures.ensure(m, s.log)
	select {
	case <-figs.done:
	case <-r.Context().Done():
		return
	case <-time.After(figureWait):
	}

	h := w.Header()
	h.Set("Content-Type", "image/svg+xml")
	h.Set("X-Content-Type-Options", "nosniff")
	// What Schmetterpause sends with the same document: nothing loads,
	// nothing runs, the inline styles that pick the face and colour it apply.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")

	select {
	case <-figs.done:
		if svg := figs.side[i].svg; svg != nil {
			// Fixed for the match, face by face, so the browser may keep it
			// for as long as a match lasts.
			h.Set("Cache-Control", "private, max-age=3600")
			_, _ = w.Write(withFace(svg, f))
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
	s.figures.ensure(m, s.log)
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
