// Package ui embeds the single-file web UI into the binary.
package ui

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var index []byte

// Handler serves the UI shell. It carries no data and needs no auth: the page asks
// for an API key and calls the authenticated /api/v1 endpoints itself.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write(index)
	})
}
