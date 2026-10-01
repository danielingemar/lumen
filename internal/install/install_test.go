package install

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidatePublicURL(t *testing.T) {
	for _, ok := range []string{"", "https://lumen.example.com", "http://10.0.0.5:4318", "https://lumen.example.com/"} {
		if err := ValidatePublicURL(ok); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"lumen.example.com", "ftp://x", "https://x.com/path", "https://x.com;rm -rf /", "https://x.com\"; id; \"", "https://a b"} {
		if err := ValidatePublicURL(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestBaseURLRejectsHostileHost(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "evil.com;id"
	if _, err := BaseURL("", r); err == nil {
		t.Fatal("hostile Host header must be rejected")
	}
	r.Host = "lumen.local:4318"
	r.Header.Set("X-Forwarded-Proto", "https")
	if u, err := BaseURL("", r); err != nil || u != "https://lumen.local:4318" {
		t.Fatalf("got %q %v", u, err)
	}
	if u, _ := BaseURL("https://configured.example", r); u != "https://configured.example" {
		t.Fatalf("configured URL must win, got %q", u)
	}
}

func TestScriptsRenderURL(t *testing.T) {
	for _, name := range []string{"agent.sh", "agent.ps1"} {
		w := httptest.NewRecorder()
		Script(name, "text/plain", "https://lumen.example.com")(w, httptest.NewRequest("GET", "/", nil))
		body := w.Body.String()
		if w.Code != 200 || strings.Contains(body, "__LUMEN_URL__") || !strings.Contains(body, `"https://lumen.example.com"`) {
			t.Fatalf("%s not rendered correctly (code %d)", name, w.Code)
		}
	}
}

func TestDownloadWhitelist(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "lumen-agent-linux-amd64"), []byte("BIN"), 0o644)
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("SECRET"), 0o644)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /download/{name}", Download(dir))
	get := func(p string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		return w
	}
	if w := get("/download/lumen-agent-linux-amd64"); w.Code != 200 || w.Body.String() != "BIN" {
		t.Fatalf("expected the binary, got %d %q", w.Code, w.Body.String())
	}
	for _, p := range []string{"/download/secret.txt", "/download/..%2Fsecret.txt", "/download/lumen-agent-linux-arm64", "/download/lumen-agent-linux-amd64%00.txt"} {
		if w := get(p); w.Code == 200 || strings.Contains(w.Body.String(), "SECRET") {
			t.Fatalf("%s must not be served (got %d)", p, w.Code)
		}
	}
}
