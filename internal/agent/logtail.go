package agent

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Tailer follows log files matching glob patterns. Files present at the first poll are read
// from their end (no historical backfill); files that appear later are read from the start.
// Only complete lines are emitted; a partial last line waits for its newline.
type Tailer struct {
	Patterns []string
	Service  string
	Format   string // "text" (default) or "docker" (json-file driver)
	offsets  map[string]int64
	started  bool
}

const maxRead = 1 << 20

func (t *Tailer) Poll() []Log {
	if t.offsets == nil {
		t.offsets = map[string]int64{}
	}
	var out []Log
	seen := map[string]bool{}
	for _, pat := range t.Patterns {
		files, _ := filepath.Glob(pat)
		for _, f := range files {
			if seen[f] {
				continue
			}
			seen[f] = true
			out = append(out, t.readFile(f)...)
		}
	}
	for f := range t.offsets { // forget deleted files
		if !seen[f] {
			delete(t.offsets, f)
		}
	}
	t.started = true
	return out
}

func (t *Tailer) readFile(path string) []Log {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return nil
	}
	off, known := t.offsets[path]
	if !known {
		if t.started {
			off = 0
		} else {
			off = st.Size()
		}
	}
	if st.Size() < off { // truncated or rotated
		off = 0
	}
	if st.Size() == off {
		t.offsets[path] = off
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, min(st.Size()-off, maxRead))
	n, _ := f.ReadAt(buf, off)
	if n == 0 && err != io.EOF {
		return nil
	}
	buf = buf[:n]
	last := strings.LastIndexByte(string(buf), '\n')
	if last < 0 {
		if n == maxRead { // a single giant line: skip it rather than stall forever
			t.offsets[path] = off + int64(n)
		} else {
			t.offsets[path] = off
		}
		return nil
	}
	t.offsets[path] = off + int64(last) + 1
	var out []Log
	for _, ln := range strings.Split(string(buf[:last]), "\n") {
		ln = strings.TrimRight(ln, "\r")
		if ln == "" {
			continue
		}
		l := Log{Service: t.Service, TimeNs: time.Now().UnixNano(), Body: ln, Attrs: map[string]string{"log.file.path": path}}
		if t.Format == "nextcloud" {
			applyNextcloudLog(&l, ln)
		}
		if t.Format == "docker" {
			var d struct {
				Log    string `json:"log"`
				Stream string `json:"stream"`
				Time   string `json:"time"`
			}
			if json.Unmarshal([]byte(ln), &d) == nil && d.Log != "" {
				l.Body = strings.TrimRight(d.Log, "\n")
				l.Attrs["log.iostream"] = d.Stream
				if ts, err := time.Parse(time.RFC3339Nano, d.Time); err == nil {
					l.TimeNs = ts.UnixNano()
				}
			}
		}
		out = append(out, l)
	}
	return out
}

// applyNextcloudLog parses one line of nextcloud.log (JSON). Non-JSON lines are kept as plain text.
// The request URL is deliberately not copied: it can contain tokens.
func applyNextcloudLog(l *Log, line string) {
	var d struct {
		Level   int             `json:"level"`
		Time    string          `json:"time"`
		App     string          `json:"app"`
		User    string          `json:"user"`
		ReqID   string          `json:"reqId"`
		Method  string          `json:"method"`
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal([]byte(line), &d) != nil || len(d.Message) == 0 {
		return
	}
	var msg string
	if json.Unmarshal(d.Message, &msg) != nil {
		msg = string(d.Message)
	}
	l.Body = msg
	l.Severity = [...]string{"DEBUG", "INFO", "WARN", "ERROR", "FATAL"}[max(0, min(d.Level, 4))]
	if ts, err := time.Parse(time.RFC3339, d.Time); err == nil {
		l.TimeNs = ts.UnixNano()
	}
	for k, v := range map[string]string{"nextcloud.app": d.App, "nextcloud.user": d.User, "nextcloud.req_id": d.ReqID, "http.method": d.Method} {
		if v != "" {
			l.Attrs[k] = v
		}
	}
}
