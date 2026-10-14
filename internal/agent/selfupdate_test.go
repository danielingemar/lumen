package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func artifactName() string { return "lumen-agent-" + runtime.GOOS + "-" + runtime.GOARCH }

// a pretend agent: a shell script that prints the version it claims to be
func fakeAgent(version string) []byte { return []byte("#!/bin/sh\necho " + version + "\n") }

func downloadServer(t *testing.T, bin []byte, sumOf []byte) *httptest.Server {
	sum := sha256.Sum256(sumOf)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download/SHA256SUMS":
			w.Write([]byte(hex.EncodeToString(sum[:]) + "  " + artifactName() + "\n"))
		case "/download/" + artifactName():
			w.Write(bin)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestSelfUpdateSettingIsParsedStrictly(t *testing.T) {
	for _, bad := range []string{"", "systemd", "systemd:", "systemd:relative/dir", "root:/tmp", "exec", ":/tmp", "systemd /tmp"} {
		if NewSelfUpdate(bad, "http://x") != nil {
			t.Errorf("%q must not enable self-update", bad)
		}
	}
	u := NewSelfUpdate("systemd:/var/lib/lumen-agent/", "http://lumen.example.com/")
	if u == nil || u.Mode != "systemd" || u.Dir != "/var/lib/lumen-agent" || u.URL != "http://lumen.example.com" || u.Capability() != "systemd" {
		t.Fatalf("%+v", u)
	}
	var none *SelfUpdate
	if none.Capability() != "" {
		t.Fatal("an agent without the setting cannot update itself, and says so")
	}
	if o, _, err := none.Request(context.Background(), "src-new"); o != OutcomeNone || err != nil {
		t.Fatal("nothing happens without the setting")
	}
}

func TestSystemdModeOnlyWritesARequest(t *testing.T) {
	dir := t.TempDir()
	u := NewSelfUpdate("systemd:"+dir, "http://x")
	old := Version
	Version = "src-old"
	defer func() { Version = old }()
	if o, _, _ := u.Request(context.Background(), ""); o != OutcomeNone {
		t.Fatal("no version asked for")
	}
	if o, _, _ := u.Request(context.Background(), "src-old"); o != OutcomeNone {
		t.Fatal("already that version")
	}
	if _, _, err := u.Request(context.Background(), "../../etc/passwd"); err == nil || !strings.Contains(err.Error(), "not a plain name") {
		t.Fatalf("a version that is not a plain name is refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "update-requested")); err == nil {
		t.Fatal("nothing was written for a refused request")
	}
	o, p, err := u.Request(context.Background(), "src-new")
	if err != nil || o != OutcomeRequested || p != "" {
		t.Fatalf("%v %v %q", o, err, p)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "update-requested"))
	if strings.TrimSpace(string(b)) != "src-new" {
		t.Fatalf("the request names the version: %q", b)
	}
	// asked again within the hour for the same version, which did not take effect: not again (no restart loop)
	os.Remove(filepath.Join(dir, "update-requested"))
	if o, _, _ := u.Request(context.Background(), "src-new"); o != OutcomeSkipped {
		t.Fatalf("the same update is not tried again within an hour: %v", o)
	}
	if _, err := os.Stat(filepath.Join(dir, "update-requested")); err == nil {
		t.Fatal("and nothing is written")
	}
	// a different version is a new request, and an hour later the same one is tried again
	if o, _, _ := u.Request(context.Background(), "src-newer"); o != OutcomeRequested {
		t.Fatal("another version")
	}
	u.Now = func() time.Time { return time.Now().Add(61 * time.Minute) }
	if o, _, _ := u.Request(context.Background(), "src-newer"); o != OutcomeRequested {
		t.Fatal("an hour later")
	}
	if _, err := os.Stat(filepath.Join(dir, ".update-requested.tmp")); err == nil {
		t.Fatal("no temporary file is left")
	}
}

func TestExecModeDownloadsVerifiesAndChecksTheVersion(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("in-place update is for linux and darwin")
	}
	old := Version
	Version = "src-old"
	defer func() { Version = old }()
	good := fakeAgent("src-new")
	ts := downloadServer(t, good, good)
	dir := t.TempDir()
	u := NewSelfUpdate("exec:"+dir, ts.URL)
	o, p, err := u.Request(context.Background(), "src-new")
	if err != nil || o != OutcomeExec || p != filepath.Join(dir, "lumen-agent.next") {
		t.Fatalf("%v %q %v", o, p, err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm()&0o100 == 0 {
		t.Fatal("the new agent is executable")
	}
	// the file does not match the server's checksum: refused, and nothing is left to run
	dir2 := t.TempDir()
	ts2 := downloadServer(t, good, []byte("something else"))
	if _, _, err := NewSelfUpdate("exec:"+dir2, ts2.URL).Request(context.Background(), "src-new"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir2, "lumen-agent.next")); err == nil {
		t.Fatal("a file that failed its checksum is never written")
	}
	// the file is genuine but says it is another version than the one asked for (a stale download folder): refused, deleted
	dir3 := t.TempDir()
	stale := fakeAgent("src-stale")
	ts3 := downloadServer(t, stale, stale)
	if _, _, err := NewSelfUpdate("exec:"+dir3, ts3.URL).Request(context.Background(), "src-new"); err == nil || !strings.Contains(err.Error(), `says it is version "src-stale", not "src-new"`) {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir3, "lumen-agent.next")); err == nil {
		t.Fatal("a file that is the wrong version is removed")
	}
	// ... and the attempt is remembered, so that it is not tried every minute
	if o, _, _ := NewSelfUpdate("exec:"+dir3, ts3.URL).Request(context.Background(), "src-new"); o != OutcomeSkipped {
		t.Fatalf("%v", o)
	}
	// a server that has no such file, or no checksum for it
	dir4 := t.TempDir()
	empty := httptest.NewServer(http.NotFoundHandler())
	defer empty.Close()
	if _, _, err := NewSelfUpdate("exec:"+dir4, empty.URL).Request(context.Background(), "src-new"); err == nil {
		t.Fatal("no download available")
	}
}

func TestFetchRemoteSendsTheVersionAndReadsTheRequest(t *testing.T) {
	var gotVersion string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.URL.Query().Get("version")
		w.Write([]byte(`{"revision":"abc","update_to":"src-new"}`))
	}))
	defer ts.Close()
	old := Version
	Version = "src-old"
	defer func() { Version = old }()
	rc, err := FetchRemote(context.Background(), ts.URL, "k", "web1")
	if err != nil || gotVersion != "src-old" || rc.UpdateTo != "src-new" || rc.Revision != "abc" {
		t.Fatalf("%+v %v %q", rc, err, gotVersion)
	}
}

func TestAgentReportsHowItCanUpdate(t *testing.T) {
	s := NewSelf("web1")
	has := func() (string, bool) {
		for _, p := range s.Collect("host", time.Now().UnixNano()) {
			if p.Name == "lumen_agent_info" {
				v, ok := p.Attrs["self_update"]
				return v, ok
			}
		}
		return "", false
	}
	if _, ok := has(); ok {
		t.Fatal("an agent that cannot update itself does not claim to")
	}
	s.SetSelfUpdate("systemd")
	if v, ok := has(); !ok || v != "systemd" {
		t.Fatal("it says how")
	}
}
