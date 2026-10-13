package ui

import (
	"embed"
	"io/fs"
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
