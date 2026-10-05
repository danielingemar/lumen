package ui

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'", "img-src 'self' data:"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
	// the logo preview and the generated tab icon are data: images; connections and scripts must stay same-origin
	if strings.Contains(csp, "connect-src") || strings.Contains(csp, "http:") || strings.Contains(csp, "https:") || strings.Contains(csp, "*") {
		t.Errorf("CSP must not open other origins: %s", csp)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(rec.Body.String(), "<title>") {
		t.Error("headers or body")
	}
}
