package ui

import (
	"embed"
	"html/template"
	"io/fs"
	"math/rand/v2"
	"net/http"
)

// The mark and the mascot are schmetterpause's (web/static/img there), for the
// same reason the tokens are: this is the same product family, and a page that
// scores a match there should be recognisably the same animal here. Copied
// rather than linked to that instance, because the coupling is optional
// (invariant 4) and a logo that disappears when SCHMETTERPAUSE_URL is unset, or
// on a network that cannot reach it, is a broken image on every page load. A
// new drawing over there is worth carrying over here.
//
//go:embed static
var staticFiles embed.FS

// staticHandler serves the embedded images under /ui/static/.
//
// Cached for a day rather than forever: the names carry no hash, so a redrawn
// mascot has to reach a browser eventually without anybody clearing a cache.
func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		// Only reachable if the embed directive above names another directory.
		panic(err)
	}
	files := http.StripPrefix("/ui/static/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	})
}

// mascotSVG is the drawing again, for inlining into a page.
//
// Inlined rather than an <img src>, because an SVG loaded through <img> is a
// document of its own that the page's CSS cannot reach, and the blade takes
// its colour from --paddle outside it. Served as a file as well, for anything
// that only wants to show it.
//
//go:embed static/mascot.svg
var mascotSVG string

// paddleColours are the blades schmetterpause gives its players
// (web/static/css/app.css there), picked by measurement: far enough from each
// other, from the fur, and from every colour that already means something on
// the page — violet is "press me", green and red are good and bad. That last
// one is why the drawing's own red is not among them.
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
	// A fresh blade on every render. There is no player behind the mascot
	// here whose colour it could keep, so it simply changes now and then.
	"paddle": func() template.CSS {
		return template.CSS("--paddle: " + paddleColours[rand.IntN(len(paddleColours))]) //nolint:gosec // decoration, not a secret
	},
}
