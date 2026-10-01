// Package install serves the agent installer scripts and agent binaries, so a new machine
// can be enrolled with one command copied from the Lumen UI.
package install

import (
	"embed"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

//go:embed agent.sh agent.ps1 agent-docker.sh
var scripts embed.FS

var (
	hostRe = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)
	fileRe = regexp.MustCompile(`^(lumen-agent-(linux|windows)-(amd64|arm64)(\.exe)?|SHA256SUMS)$`)
)

// ValidatePublicURL checks a configured LUMEN_PUBLIC_URL. The value is written into shell and
// PowerShell scripts, so only plain http(s)://host[:port] is accepted.
func ValidatePublicURL(u string) error {
	if u == "" {
		return nil
	}
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || !hostRe.MatchString(p.Host) || (p.Path != "" && p.Path != "/") {
		return fmt.Errorf("LUMEN_PUBLIC_URL must look like https://lumen.example.com[:port], got %q", u)
	}
	return nil
}

// BaseURL returns the URL agents should use. Prefer the configured public URL: the Host
// header is client-controlled, so it is strictly validated before it reaches a script.
func BaseURL(publicURL string, r *http.Request) (string, error) {
	if publicURL != "" {
		return publicURL, nil
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if x := r.Header.Get("X-Forwarded-Proto"); x == "https" || x == "http" {
		scheme = x
	}
	if !hostRe.MatchString(r.Host) {
		return "", errors.New("invalid Host header; set LUMEN_PUBLIC_URL")
	}
	return scheme + "://" + r.Host, nil
}

// Script serves an embedded installer with the server URL filled in.
func Script(name, contentType, publicURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		base, err := BaseURL(publicURL, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b, err := scripts.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte(strings.ReplaceAll(string(b), "__LUMEN_URL__", base)))
	}
}

// Download serves a whitelisted file from distDir. Names are matched against a strict
// pattern, so no path traversal is possible.
func Download(distDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		path := filepath.Join(distDir, name)
		if _, err := os.Stat(path); !fileRe.MatchString(name) || err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		http.ServeFile(w, r, path)
	}
}
