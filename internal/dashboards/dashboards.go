// Package dashboards stores user-built dashboards (panel layout + queries) per tenant in the document store.
package dashboards

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
)

const (
	coll      = "dashboards"
	MaxBody   = 256 << 10 // bytes
	MaxPanels = 60
)

var (
	ErrNotFound = errors.New("dashboard not found")
	ErrInvalid  = errors.New("invalid dashboard")
	ErrConflict = docstore.ErrConflict
)

// Dashboard is the stored document. Body is opaque to the server except for basic validation:
// the UI owns its structure ({"panels":[...]}).
type Dashboard struct {
	ID          string          `json:"id"`
	Tenant      string          `json:"tenant"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Body        json.RawMessage `json:"body,omitempty"`
	CreatedBy   string          `json:"created_by,omitempty"`
	Created     time.Time       `json:"created"`
	Updated     time.Time       `json:"updated"`
	Version     string          `json:"version,omitempty"` // filled on read; send it back on update to detect concurrent edits
}

type Service struct{ b docstore.Backend }

func New(b docstore.Backend) *Service { return &Service{b} }

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func validate(name string, body json.RawMessage) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return fmt.Errorf("%w: the name must be 1-100 characters", ErrInvalid)
	}
	if len(body) > MaxBody {
		return fmt.Errorf("%w: the dashboard is larger than %d KB", ErrInvalid, MaxBody>>10)
	}
	var b struct {
		Panels []json.RawMessage `json:"panels"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return fmt.Errorf("%w: body must be a JSON object with a panels array", ErrInvalid)
	}
	if len(b.Panels) > MaxPanels {
		return fmt.Errorf("%w: at most %d panels", ErrInvalid, MaxPanels)
	}
	return nil
}

func (s *Service) List(tenant string) ([]Dashboard, error) {
	c, cancel := ctx()
	defer cancel()
	docs, err := s.b.List(c, coll, map[string]string{"tenant": tenant}, 500)
	if err != nil {
		return nil, err
	}
	out := []Dashboard{}
	for _, d := range docs {
		var x Dashboard
		if d.Decode(&x) == nil {
			x.Body = nil // listings do not carry the panels
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

func (s *Service) Get(tenant, id string) (Dashboard, error) {
	c, cancel := ctx()
	defer cancel()
	d, err := s.b.Get(c, coll, id)
	if errors.Is(err, docstore.ErrNotFound) {
		return Dashboard{}, ErrNotFound
	}
	if err != nil {
		return Dashboard{}, err
	}
	var x Dashboard
	if err := d.Decode(&x); err != nil || x.Tenant != tenant { // another tenant's dashboard looks like it does not exist
		return Dashboard{}, ErrNotFound
	}
	x.Version = d.Version
	return x, nil
}

func (s *Service) Create(tenant, user, name, desc string, body json.RawMessage) (Dashboard, error) {
	if len(body) == 0 {
		body = json.RawMessage(`{"panels":[]}`)
	}
	if err := validate(name, body); err != nil {
		return Dashboard{}, err
	}
	now := time.Now().UTC()
	c, cancel := ctx()
	defer cancel()
	for i := 0; i < 5; i++ {
		x := Dashboard{ID: randomID(), Tenant: tenant, Name: strings.TrimSpace(name), Description: desc, Body: body, CreatedBy: user, Created: now, Updated: now}
		err := s.b.Create(c, coll, x.ID, x)
		if errors.Is(err, docstore.ErrExists) {
			continue
		}
		if err != nil {
			return Dashboard{}, err
		}
		return s.Get(tenant, x.ID)
	}
	return Dashboard{}, errors.New("could not allocate a dashboard id")
}

// Update replaces name, description and body. If version is non-empty and stale, it returns ErrConflict.
func (s *Service) Update(tenant, id, name, desc string, body json.RawMessage, version string) (Dashboard, error) {
	if err := validate(name, body); err != nil {
		return Dashboard{}, err
	}
	cur, err := s.Get(tenant, id)
	if err != nil {
		return Dashboard{}, err
	}
	if version == "" {
		version = cur.Version
	}
	cur.Name, cur.Description, cur.Body, cur.Updated = strings.TrimSpace(name), desc, body, time.Now().UTC()
	cur.Version = ""
	c, cancel := ctx()
	defer cancel()
	if err := s.b.Put(c, coll, id, cur, version); err != nil {
		return Dashboard{}, err
	}
	return s.Get(tenant, id)
}

func (s *Service) Delete(tenant, id string) error {
	if _, err := s.Get(tenant, id); err != nil {
		return err
	}
	c, cancel := ctx()
	defer cancel()
	return s.b.Delete(c, coll, id)
}
