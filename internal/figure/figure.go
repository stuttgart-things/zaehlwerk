// Package figure keeps the players' figures from Schmetterpause, which lets
// every player assemble one (Schmetterpause ADR-0029), for the screens that
// show them with the face the match gives them (ADR-0018): zaehlwerk's own
// pages and the game's.
package figure

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/face"
)

// Source is where the figures come from. An empty id asks for a random figure,
// for a side with nobody behind it.
type Source interface {
	Figure(ctx context.Context, id string) ([]byte, error)
}

const (
	// keep is how many matches' figures are kept. One table has one running
	// match and the one before it on a screen still holding its result; more
	// is room for a page left open on an old one.
	keep = 8

	// Wait bounds how long a fetch may take, and how long a route serving a
	// figure waits for the fetch it started: a little over the client's own
	// timeout.
	Wait = 6 * time.Second
)

// Cache holds each match's two figures, fetched once per match.
//
// Once rather than per request because a random figure is a new one every
// time it is asked for, and because a figure that changed mid-match because
// its player saved a new one would be a figure that is not the one that
// started. Only in memory and bounded (invariant 2): a match that falls out
// fetches again if anybody asks.
type Cache struct {
	src Source
	log *slog.Logger

	mu    sync.Mutex
	byKey map[string]*Pair
	order []string
}

// Pair is one match's two figures. Side is written once, by the fetch, before
// Done is closed; nothing reads it before.
type Pair struct {
	Done chan struct{}
	Side [2]Figure
}

// Figure is one side's drawing and the blade it carries. SVG is nil when the
// fetch failed.
type Figure struct {
	SVG   []byte
	Blade string
}

// NewCache is a cache fetching from src.
func NewCache(src Source, log *slog.Logger) *Cache {
	return &Cache{src: src, log: log, byKey: make(map[string]*Pair)}
}

// Ensure starts fetching the figures of the match under key unless that has
// already happened, and returns them, ready or not. It never waits: it is
// called while a point is being rendered.
//
// ids says whose figures they are, home first. It is asked inside the fetch,
// so a caller that has to look the players up does that off the render too;
// an empty id is a random figure.
func (c *Cache) Ensure(key string, ids func(context.Context) [2]string) *Pair {
	c.mu.Lock()
	defer c.mu.Unlock()

	if p, ok := c.byKey[key]; ok {
		return p
	}
	p := &Pair{Done: make(chan struct{})}
	c.byKey[key] = p
	c.order = append(c.order, key)
	if len(c.order) > keep {
		delete(c.byKey, c.order[0])
		c.order = c.order[1:]
	}

	go func() {
		defer close(p.Done)
		// Its own deadline, not a request's: the fetch outlives whichever
		// render or request started it, and serves every later one.
		lookup, cancel := context.WithTimeout(context.Background(), Wait)
		who := ids(lookup)
		cancel()
		for i, id := range who {
			ctx, cancel := context.WithTimeout(context.Background(), Wait)
			svg, err := c.src.Figure(ctx, id)
			cancel()
			if err != nil {
				c.log.Warn("figure unavailable", "match", key, "side", i, "error", err)
				continue
			}
			p.Side[i] = Figure{SVG: svg, Blade: BladeOf(svg)}
		}
	}()
	return p
}

// Ready is the figures under key if they have arrived, else nil.
func (c *Cache) Ready(key string) *Pair {
	c.mu.Lock()
	p := c.byKey[key]
	c.mu.Unlock()
	if p == nil {
		return nil
	}
	select {
	case <-p.Done:
		return p
	default:
		return nil
	}
}

// paddleProp is the blade colour on a figure's root: Schmetterpause puts the
// colours there as custom properties, the blade as --paddle.
var paddleProp = regexp.MustCompile(`--paddle:\s*(#[0-9a-fA-F]{6})\b`)

// BladeOf is the blade a figure carries, or empty when it says none.
func BladeOf(svg []byte) string {
	m := paddleProp.FindSubmatch(svg)
	if m == nil {
		return ""
	}
	return string(bytes.ToLower(m[1]))
}

// dataFace is the face selector on a figure's root. The figure carries every
// face, and a scoped <style> inside it shows the one named here.
var dataFace = regexp.MustCompile(`data-face="[a-z-]*"`)

// WithFace is svg showing f.
func WithFace(svg []byte, f face.Face) []byte {
	attr := []byte(`data-face="` + string(f) + `"`)
	if loc := dataFace.FindIndex(svg); loc != nil {
		return append(append(append([]byte{}, svg[:loc[0]]...), attr...), svg[loc[1]:]...)
	}
	// A figure without the attribute shows its default face, which is fine;
	// adding it costs nothing and keeps the face this side was given.
	return bytes.Replace(svg, []byte("<svg"), append([]byte("<svg "), attr...), 1)
}

// known is what a route's ?face= may ask for.
var known = map[face.Face]bool{
	face.Neutral: true, face.Happy: true, face.Surprised: true,
	face.Skeptical: true, face.Angry: true, face.Sad: true,
}

// Asked is the face a query names, neutral for anything else, so the query
// string never reaches the document unchecked.
func Asked(q string) face.Face {
	if f := face.Face(q); known[f] {
		return f
	}
	return face.Neutral
}

// Headers are what a figure goes out with: what Schmetterpause sends with the
// same document. Nothing loads, nothing runs, the inline styles that pick the
// face and colour it apply.
var Headers = map[string]string{
	"Content-Type":            "image/svg+xml",
	"X-Content-Type-Options":  "nosniff",
	"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'",
}
