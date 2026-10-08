package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
)

func statfsFake(string) (uint64, uint64, error) { return 100 << 30, 40 << 30, nil }

func TestAChosenFolderIsChecked(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "backup")
	os.Mkdir(good, 0o755)
	file := filepath.Join(root, "afile")
	os.WriteFile(file, []byte("x"), 0o644)
	os.Symlink("/etc", filepath.Join(root, "link"))
	roots := []string{root}

	for name, c := range map[string]struct{ dir, want string }{
		"nothing":             {"", "choose a folder"},
		"relative":            {"mnt/backup", "full path"},
		"dots":                {root + "/backup/../backup", "without .."},
		"a trailing slash":    {good + "/", "without .."},
		"outside the places":  {"/etc", "must be inside one of"},
		"a control character": {good + "\x00", "choose a folder"},
		"the root itself":     {root, "must be inside one of"},
		"missing":             {filepath.Join(root, "nodisk"), "does not exist here"},
		"a file":              {file, "is a file"},
		"a link out":          {filepath.Join(root, "link"), "outside the allowed places"},
	} {
		in := Check(c.dir, roots, "", true, statfsFake)
		if in.Problem == "" || !strings.Contains(in.Problem, c.want) {
			t.Errorf("%s: %q, want %q", name, in.Problem, c.want)
		}
	}
	in := Check(good, roots, "", true, statfsFake)
	if in.Problem != "" || !in.Exists || !in.Writable || in.Total != 100<<30 || in.Free != 40<<30 {
		t.Fatalf("a folder that is there and can be written to: %+v", in)
	}
	if left, _ := os.ReadDir(good); len(left) != 0 {
		t.Fatalf("the test file is removed again: %v", left)
	}
	// without the write test nothing is written
	if in := Check(good, roots, "", false, statfsFake); in.Writable || in.Problem != "" {
		t.Fatalf("%+v", in)
	}
	// a disk that is read-only (running as root ignores permissions, so this only means something for other users)
	if os.Geteuid() != 0 {
		ro := filepath.Join(root, "ro")
		os.Mkdir(ro, 0o555)
		if in := Check(ro, roots, "", true, statfsFake); !strings.Contains(in.Problem, "cannot write") {
			t.Fatalf("%+v", in)
		}
	}
	// the default folder and the places disks are mounted are allowed; the mount point itself is not a folder to fill
	if got := Check("/mnt", nil, "", false, nil); !strings.Contains(got.Problem, "must be inside one of") {
		t.Fatalf("%+v", got)
	}
	if got := Check("/etc/ssh", nil, "", false, nil); !strings.Contains(got.Problem, "must be inside one of") {
		t.Fatalf("the system is not a place for backups: %+v", got)
	}
	// on the same disk as the data is worth a warning
	data := t.TempDir()
	in = Check(good, roots, data, true, statfsFake)
	if !in.SameDiskAsData || !strings.Contains(in.Warning, "same disk as Lumen's data") {
		t.Fatalf("both are under /tmp here, so one disk: %+v", in)
	}
}

func TestAChosenFolderThatIsNotThereIsNeverMade(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	src := newSrc(now)
	base := t.TempDir()
	missing := filepath.Join(base, "mnt-backup") // a disk that is not mounted
	where := NewWhere(missing, true)
	m := &Manager{Where: where, Src: src, DataDays: 5, Now: func() time.Time { return now }}
	err := m.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Is the disk mounted?") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("the folder must not be made: the backups would look saved and be on the wrong disk")
	}
	// the default folder is made when it is not there
	where.Set(filepath.Join(base, "default"), false)
	if err := m.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "default", "telemetry")); err != nil {
		t.Fatal("the default is made")
	}
	// and changing the folder while Lumen runs sends the next backup there
	next := filepath.Join(base, "disk2")
	os.Mkdir(next, 0o755)
	where.Set(next, true)
	if err := m.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(next, "telemetry")); len(entries) == 0 {
		t.Fatal("the new folder is used from the next backup")
	}
}

func TestTheChosenFolderIsRemembered(t *testing.T) {
	db, _ := docstore.OpenFile(t.TempDir())
	ctx := context.Background()
	if LoadSaved(ctx, db) != "" {
		t.Fatal("nothing chosen")
	}
	if err := Save(ctx, db, "/mnt/backup", "admin"); err != nil || LoadSaved(ctx, db) != "/mnt/backup" {
		t.Fatalf("%v %q", err, LoadSaved(ctx, db))
	}
	Save(ctx, db, "", "admin")
	if LoadSaved(ctx, db) != "" {
		t.Fatal("an empty one goes back to the default")
	}
}
