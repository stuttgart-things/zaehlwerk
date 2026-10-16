package ui

import (
	"html/template"
	"math/rand/v2"
	"net/http"

	"github.com/stuttgart-things/zaehlwerk/internal/ui/static"
)

// The mark (mark-32.png, mark-180.png) and the mascot are Zählwerk's own
// drawings of schmetterpause's cat: the same animal, because this is the same
// product family and a page that scores a match there should be recognisably
// the same here, but in Zählwerk's role — the referee with stopwatch and
// whistle, because here it keeps the score rather than plays. The mascot's
// layers carry the same ids as schmetterpause's drawing (fur, fur-dark, ink,
// eyes).
//
// Embedded rather than linked to that instance, because the coupling is
// optional (invariant 4) and a logo that disappears when SCHMETTERPAUSE_URL is
// unset, or on a network that cannot reach it, is a broken image on every page
// load.
//
// The files and their embed live in package static, so that zaehlwerk-pong can
// serve the mark too.

// staticHandler serves the embedded images under /ui/static/.
//
// Cached for a day rather than forever: the names carry no hash, so a redrawn
// mascot has to reach a browser eventually without anybody clearing a cache.
func staticHandler() http.Handler {
	files := http.StripPrefix("/ui/static/", http.FileServerFS(static.FS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	})
}

// mascotSVG is the drawing again, for inlining into a page.
//
// Inlined rather than an <img src>, because an SVG loaded through <img> is a
// document of its own that the page's CSS cannot reach, and the whistle's
// cord takes its colour from --paddle outside it. Served as a file as well,
// for anything that only wants to show it.
var mascotSVG = static.Mascot

// paddleColours are the blades schmetterpause gives its players
// (web/static/css/app.css there), picked by measurement: far enough from each
// other, from the fur, and from every colour that already means something on
// the page — violet is "press me", green and red are good and bad. The
// drawing's own gold, the cord's fallback, sits too close to the fur to be
// one of them.
var paddleColours = []string{
	"#3aa6ff", // light blue
	"#2f6fd0", // blue
	"#159c8c", // petrol
	"#4d7c4a", // moss green
	"#d64f9c", // pink
	"#c05fd0", // orchid
	"#8c8f9c", // steel
}

// templateFuncs is what the templates may call beyond the built-ins.
var templateFuncs = template.FuncMap{
	"mascot": func() template.HTML { return template.HTML(mascotSVG) }, //nolint:gosec // our own embedded file, not input
	// A fresh cord on every render. There is no player behind the mascot
	// here whose colour it could keep, so it simply changes now and then.
	"paddle": func() template.CSS {
		return template.CSS("--paddle: " + paddleColours[rand.IntN(len(paddleColours))]) //nolint:gosec // decoration, not a secret
	},
}
