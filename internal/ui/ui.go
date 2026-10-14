// Package ui embeds the single-file web UI into the binary.
package ui

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
)

//go:embed index.html
var index []byte

// etag identifies this build of the page. With "no-cache" the browser asks the server every time, and gets a cheap
// 304 when nothing changed, so an upgrade shows up at once (no hard reload needed) without sending the page again.
var etag = func() string { s := sha256.Sum256(index); return `"` + hex.EncodeToString(s[:8]) + `"` }()

// Handler serves the UI shell. It carries no data and needs no auth: the page asks
// for an API key and calls the authenticated /api/v1 endpoints itself.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-cache")
		h.Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		h.Set("Content-Type", "text/html; charset=utf-8")
		w.Write(index)
	})
}
