package branding

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/danielingemar/lumen/internal/docstore"
)

var png = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")

func url(mime string, raw []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw)
}

func TestParseLogo(t *testing.T) {
	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><rect width="10" height="10"/></svg>`)
	for _, c := range []struct {
		name string
		in   string
		ok   bool
	}{
		{"png", url("image/png", png), true},
		{"jpeg", url("image/jpeg", []byte("\xff\xd8\xff\xe0JFIF")), true},
		{"gif", url("image/gif", []byte("GIF89a....")), true},
		{"webp", url("image/webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 ")), true},
		{"svg", url("image/svg+xml", svg), true},
		{"declared type lies", url("image/jpeg", png), false},
		{"html pretending to be png", url("image/png", []byte("<html><script>alert(1)</script>")), false},
		{"svg with script", url("image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)), false},
		{"svg with onload", url("image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`)), false},
		{"svg with javascript url", url("image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><a href="javascript:alert(1)"><rect/></a></svg>`)), false},
		{"svg with foreignObject", url("image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><foreignObject><body/></foreignObject></svg>`)), false},
		{"svg with entity", url("image/svg+xml", []byte(`<?xml version="1.0"?><!DOCTYPE svg [<!ENTITY x "y">]><svg xmlns="http://www.w3.org/2000/svg"/>`)), false},
		{"not a data url", "http://evil.example/x.png", false},
		{"not base64", "data:image/png,abc", false},
		{"broken base64", "data:image/png;base64,@@@@", false},
		{"empty", "data:image/png;base64,", false},
		{"too large", url("image/png", append(append([]byte{}, png...), make([]byte, MaxLogo)...)), false},
	} {
		_, _, err := ParseLogo(c.in)
		if c.ok && err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s must be refused with ErrInvalid, got %v", c.name, err)
		}
	}
}

func TestUpdateAndPublic(t *testing.T) {
	f, _ := docstore.OpenFile(t.TempDir())
	s := New(f)
	if p := s.Public(); p.Name != "" || p.HasLogo {
		t.Fatalf("defaults: %+v", p)
	}
	p, err := s.Update("  Tracexit  ", url("image/png", png), false)
	if err != nil || p.Name != "Tracexit" || !p.HasLogo || p.Version == "" {
		t.Fatalf("%+v %v", p, err)
	}
	raw, mime, ok := s.Logo()
	if !ok || mime != "image/png" || string(raw) != string(png) {
		t.Fatal("logo must round-trip byte for byte")
	}
	// changing only the name keeps the logo
	v := p.Version
	p, err = s.Update("Renamed", "", false)
	if err != nil || !p.HasLogo || p.Name != "Renamed" {
		t.Fatalf("%+v %v", p, err)
	}
	if p.Version == v {
		// the version follows the update time; it only has to change when the logo is replaced
		p2, _ := s.Update("Renamed", url("image/gif", []byte("GIF89a....")), false)
		if p2.Version == v {
			t.Fatal("a new logo must get a new version so browsers refetch it")
		}
	}
	// a rejected upload changes nothing
	if _, err := s.Update("Other", url("image/png", []byte("nope")), false); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if s.Public().Name == "Other" {
		t.Fatal("a failed update must not be partly applied")
	}
	for _, bad := range []string{strings.Repeat("x", MaxName+1), "a\nb", "<b>x</b>"} {
		if _, err := s.Update(bad, "", false); !errors.Is(err, ErrInvalid) {
			t.Errorf("name %q must be refused", bad)
		}
	}
	p, _ = s.Update("Renamed", "", true)
	if p.HasLogo {
		t.Fatal("remove_logo")
	}
	if _, _, ok := s.Logo(); ok {
		t.Fatal("logo must be gone")
	}
}
