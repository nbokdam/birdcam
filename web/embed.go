// Package web holds the static viewer page, embedded into birdcam-server.
package web

import "embed"

// Files contains index.html, app.js and style.css.
//
//go:embed index.html app.js style.css
var Files embed.FS
