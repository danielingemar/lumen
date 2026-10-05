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

func TestUIIsRevalidatedNotStale(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	tag := rec.Header().Get("ETag")
	if tag == "" || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("the page must be revalidated every time so an upgrade shows at once: etag %q cache-control %q", tag, rec.Header().Get("Cache-Control"))
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("If-None-Match", tag)
	rec2 := httptest.NewRecorder()
	Handler().ServeHTTP(rec2, req)
	if rec2.Code != 304 || rec2.Body.Len() != 0 {
		t.Fatalf("an unchanged page is answered with 304 and no body: %d %d", rec2.Code, rec2.Body.Len())
	}
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("If-None-Match", `"some-older-build"`)
	rec3 := httptest.NewRecorder()
	Handler().ServeHTTP(rec3, req)
	if rec3.Code != 200 || rec3.Body.Len() == 0 {
		t.Fatalf("a browser holding another build gets the new page: %d", rec3.Code)
	}
}
