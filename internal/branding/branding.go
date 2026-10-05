// Package branding stores the site name and logo that an administrator sets in the UI. They are instance-wide
// (the login page needs them before anyone is known), so they are shown to everyone who can reach the server.
package branding

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
)

const (
	coll    = "meta"
	docID   = "branding"
	MaxLogo = 512 << 10 // bytes after decoding
	MaxName = 40
)

var ErrInvalid = errors.New("invalid")

func invalid(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(f, a...)) }

// Settings is what is stored.
type Settings struct {
	Name     string    `json:"name"`
	LogoMime string    `json:"logo_mime"`
	Logo     string    `json:"logo"` // base64
	Updated  time.Time `json:"updated"`
}

// Public is what anyone may see.
type Public struct {
	Name    string `json:"name"`
	HasLogo bool   `json:"has_logo"`
	Version string `json:"version"` // changes when the logo changes, so browsers refetch it
}

type Service struct{ b docstore.Backend }

func New(b docstore.Backend) *Service { return &Service{b} }

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func (s *Service) get() Settings {
	c, cancel := ctx()
	defer cancel()
	d, err := s.b.Get(c, coll, docID)
	var st Settings
	if err != nil || json.Unmarshal(d.Data, &st) != nil {
		return Settings{}
	}
	return st
}

func (s *Service) Public() Public {
	st := s.get()
	p := Public{Name: st.Name, HasLogo: st.Logo != ""}
	if p.HasLogo {
		p.Version = strconv.FormatInt(st.Updated.UnixNano(), 36)
	}
	return p
}

// Logo returns the image bytes and its content type.
func (s *Service) Logo() ([]byte, string, bool) {
	st := s.get()
	if st.Logo == "" {
		return nil, "", false
	}
	raw, err := base64.StdEncoding.DecodeString(st.Logo)
	if err != nil {
		return nil, "", false
	}
	return raw, st.LogoMime, true
}

// Update changes the name and, when logoDataURL is not empty, the logo; removeLogo deletes it.
func (s *Service) Update(name, logoDataURL string, removeLogo bool) (Public, error) {
	name = strings.TrimSpace(name)
	if len(name) > MaxName || strings.ContainsAny(name, "\r\n\t\x00<>") {
		return Public{}, invalid("the name must be at most %d characters, on one line, without < or >", MaxName)
	}
	st := s.get()
	st.Name = name
	switch {
	case removeLogo:
		st.Logo, st.LogoMime = "", ""
	case logoDataURL != "":
		mime, raw, err := ParseLogo(logoDataURL)
		if err != nil {
			return Public{}, err
		}
		st.Logo, st.LogoMime = base64.StdEncoding.EncodeToString(raw), mime
	}
	st.Updated = time.Now().UTC()
	c, cancel := ctx()
	defer cancel()
	if err := s.b.Put(c, coll, docID, st, ""); err != nil {
		return Public{}, err
	}
	return s.Public(), nil
}

var (
	svgBad = regexp.MustCompile(`(?is)<\s*script|javascript:|<\s*foreignobject|<\s*iframe|<\s*embed|<\s*object|<!entity|<!doctype[^>]*\[|\son[a-z]+\s*=`)
)

// Detect returns the image type of the bytes by looking at them (never trusting a declared type), or "".
func Detect(raw []byte) string {
	switch {
	case bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(raw, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(raw, []byte("GIF87a")) || bytes.HasPrefix(raw, []byte("GIF89a")):
		return "image/gif"
	case len(raw) > 12 && bytes.Equal(raw[:4], []byte("RIFF")) && bytes.Equal(raw[8:12], []byte("WEBP")):
		return "image/webp"
	}
	head := strings.ToLower(strings.TrimLeft(string(raw[:min(len(raw), 512)]), "\ufeff \t\r\n"))
	if (strings.HasPrefix(head, "<svg") || strings.HasPrefix(head, "<?xml")) && strings.Contains(strings.ToLower(string(raw)), "<svg") {
		return "image/svg+xml"
	}
	return ""
}

// ParseLogo checks a data URL (data:image/png;base64,...) and returns the content type and the image bytes.
// The declared type must match what the bytes really are; SVG files with scripts or event handlers are refused.
func ParseLogo(dataURL string) (string, []byte, error) {
	rest, ok := strings.CutPrefix(dataURL, "data:")
	if !ok {
		return "", nil, invalid("the logo must be an image file")
	}
	meta, b64, ok := strings.Cut(rest, ",")
	declared, enc, _ := strings.Cut(meta, ";")
	if !ok || enc != "base64" {
		return "", nil, invalid("the logo must be a base64 data URL")
	}
	if base64.StdEncoding.DecodedLen(len(b64)) > MaxLogo+4 {
		return "", nil, invalid("the logo is larger than %d KB", MaxLogo>>10)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", nil, invalid("the logo is not valid base64")
	}
	if len(raw) == 0 || len(raw) > MaxLogo {
		return "", nil, invalid("the logo must be between 1 byte and %d KB", MaxLogo>>10)
	}
	real := Detect(raw)
	if real == "" {
		return "", nil, invalid("the logo must be PNG, JPEG, GIF, WebP or SVG")
	}
	if declared != real {
		return "", nil, invalid("the file is %s but was sent as %s", real, declared)
	}
	if real == "image/svg+xml" && svgBad.Match(raw) {
		return "", nil, invalid("the SVG contains scripts or event handlers, which are not allowed; export it again as a plain SVG or use PNG")
	}
	return real, raw, nil
}
