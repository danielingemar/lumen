package agent

import (
	"path/filepath"
	"runtime"
	"strings"
)

func defaultLogDirs() []string {
	if runtime.GOOS == "windows" {
		return []string{`C:\inetpub`, `C:\ProgramData`, `C:\Logs`, `C:\Windows\Logs`, `C:\xampp`}
	}
	return []string{"/var/log", "/var/lib/docker/containers", "/var/lib/docker/volumes", "/var/www", "/srv", "/mnt", "/opt"}
}

// LogPathAllowed reports whether a server-supplied log path (a file or glob) lies inside one of the allowed
// directories. It must be absolute and contain no ".." element, because a UI user must not be able to make the
// agent read /etc/shadow or an SSH key on every machine.
func LogPathAllowed(p string, dirs []string) bool {
	for _, d := range dirs {
		if d == "*" {
			return true
		}
	}
	if len(dirs) == 0 {
		dirs = defaultLogDirs()
	}
	if !filepath.IsAbs(p) && !(runtime.GOOS == "windows" && len(p) > 2 && p[1] == ':') {
		return false
	}
	for _, seg := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return false
		}
	}
	prefix := p
	if i := strings.IndexAny(p, "*?["); i >= 0 {
		prefix = p[:i]
	}
	prefix = filepath.Clean(prefix)
	fold := func(s string) string {
		if runtime.GOOS == "windows" {
			return strings.ToLower(s)
		}
		return s
	}
	for _, d := range dirs {
		rel, err := filepath.Rel(fold(filepath.Clean(d)), fold(prefix))
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
