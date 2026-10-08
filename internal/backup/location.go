package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
)

// Where is the folder backups are written to. It can be changed from Settings while Lumen runs, so everything that reads it
// asks here.
type Where struct {
	mu     sync.RWMutex
	dir    string
	strict bool
}

func NewWhere(dir string, strict bool) *Where { return &Where{dir: dir, strict: strict} }

func (w *Where) Get() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.dir
}

// Strict tells whether the folder was chosen by somebody (then it must exist already) or is the default (then it is made).
func (w *Where) Strict() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.strict
}

func (w *Where) Set(dir string, strict bool) {
	w.mu.Lock()
	w.dir, w.strict = dir, strict
	w.mu.Unlock()
}

const (
	settingsColl = "meta"
	settingsID   = "backup_location"
)

type saved struct {
	Dir string    `json:"dir"`
	By  string    `json:"by"`
	At  time.Time `json:"at"`
}

// LoadSaved reads the folder somebody chose in Settings ("" if nobody did).
func LoadSaved(ctx context.Context, db docstore.Backend) string {
	d, err := db.Get(ctx, settingsColl, settingsID)
	if err != nil {
		return ""
	}
	var s saved
	if d.Decode(&s) != nil {
		return ""
	}
	return s.Dir
}

// Save remembers the chosen folder. An empty one goes back to the default.
func Save(ctx context.Context, db docstore.Backend, dir, by string) error {
	if dir == "" {
		_ = db.Delete(ctx, settingsColl, settingsID)
		return nil
	}
	return db.Put(ctx, settingsColl, settingsID, saved{Dir: dir, By: by, At: time.Now().UTC()}, "")
}

// DefaultRoots are where a backup folder may be: the default one, and the places a disk is usually mounted. A folder somewhere
// else, such as /etc, is refused, so that this setting cannot be used to write into the system.
var DefaultRoots = []string{"/backup", "/mnt", "/media", "/srv", "/var/backups"}

// Info is what is known about a folder.
type Info struct {
	Dir            string `json:"dir"`
	Exists         bool   `json:"exists"`
	Writable       bool   `json:"writable"`
	Free           uint64 `json:"free"`
	Total          uint64 `json:"total"`
	SameDiskAsData bool   `json:"same_disk_as_data"` // a disk that fails takes both the data and the backups
	OnRootDisk     bool   `json:"on_root_disk"`      // on the same disk as "/": in a container, the container's own storage
	Warning        string `json:"warning,omitempty"`
	Problem        string `json:"problem,omitempty"` // why it cannot be used
}

var badChars = regexp.MustCompile(`[\x00-\x1f\x7f]`)

// Check says whether a folder can be used for backups, and what is worth knowing about it. With write true it also tries to
// write a file there (and removes it again); that is what proves that the disk is mounted and not read-only.
func Check(dir string, roots []string, dataDir string, write bool, statfs func(string) (uint64, uint64, error)) Info {
	in := Info{Dir: dir}
	fail := func(f string, a ...any) Info { in.Problem = fmt.Sprintf(f, a...); return in }
	if dir == "" || badChars.MatchString(dir) {
		return fail("choose a folder")
	}
	if !filepath.IsAbs(dir) {
		return fail("the folder must be a full path, starting with /")
	}
	if filepath.Clean(dir) != dir {
		return fail("write the path without .. or a trailing slash: %s", filepath.Clean(dir))
	}
	if len(roots) == 0 {
		roots = DefaultRoots
	}
	inRoots := func(p string) (string, bool) {
		for _, r := range roots {
			r = filepath.Clean(r)
			if p == r && r != "/backup" && r != "/var/backups" { // a place where disks are mounted is not itself a folder to fill
				continue
			}
			if rel, err := filepath.Rel(r, p); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
				return r, true
			}
		}
		return "", false
	}
	if _, ok := inRoots(dir); !ok {
		return fail("the folder must be inside one of: %s (for example /mnt/backup). This limit is set with LUMEN_BACKUP_ROOTS", strings.Join(roots, ", "))
	}
	st, err := os.Stat(dir)
	if err != nil {
		return fail("%s does not exist here. If it is a disk, it has to be mounted into the container first (see below), and it is not made for you: a backup into a folder that is not on the disk would only look saved", dir)
	}
	if !st.IsDir() {
		return fail("%s is a file, not a folder", dir)
	}
	in.Exists = true
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != dir { // a link must not lead out of the allowed places
		if _, ok := inRoots(real); !ok {
			return fail("%s leads to %s, which is outside the allowed places", dir, real)
		}
	}
	if statfs != nil {
		in.Total, in.Free = func() (uint64, uint64) { t, f, err := statfs(dir); _ = err; return t, f }()
	}
	if write {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		probe := filepath.Join(dir, ".lumen-write-test-"+hex.EncodeToString(b))
		if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
			return fail("Lumen cannot write to %s (%v). The folder or the mount may be read-only, or owned by another user", dir, strings.TrimPrefix(err.Error(), "open "+probe+": "))
		}
		_ = os.Remove(probe)
		in.Writable = true
	}
	dd, ok1 := deviceOf(dir)
	if dataDir != "" {
		if dv, ok2 := deviceOf(dataDir); ok1 && ok2 && dd == dv {
			in.SameDiskAsData = true
		}
	}
	if rd, ok := deviceOf("/"); ok && ok1 && rd == dd {
		in.OnRootDisk = true
	}
	switch {
	case in.OnRootDisk && in.SameDiskAsData:
		in.Warning = "This folder is on the same disk as Lumen's data and the system. A disk that fails takes the backups with it, and in a container this may be the container's own storage, which is lost when the container is recreated. Mount a separate disk for backups."
	case in.SameDiskAsData:
		in.Warning = "This folder is on the same disk as Lumen's data: a disk that fails takes the backups with it."
	}
	return in
}
