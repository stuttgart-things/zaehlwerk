// Package static holds Zählwerk's images: the mark (mark-32.png,
// mark-180.png) and the mascot (mascot.svg).
//
// A package of its own, next to the files, so that zaehlwerk-pong can show
// the same mark as its favicon without importing package ui and everything
// the scoring page pulls in with it.
package static

import "embed"

// FS is the images, at the root: "mark-32.png", "mark-180.png", "mascot.svg".
//
//go:embed *.png *.svg
var FS embed.FS

// Mascot is the drawing again, as text, for inlining into a page.
//
//go:embed mascot.svg
var Mascot string
