package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// SelfUpdate lets the Lumen server ask this agent to update itself. How it is done depends on how the agent runs, and the
// installer says so in LUMEN_AGENT_SELF_UPDATE:
//
//	systemd:<dir>  the agent runs as an unprivileged user and cannot replace its own file. It writes <dir>/update-requested; a
//	               systemd path unit starts a small root-owned helper that downloads the new version from the Lumen server
//	               (the address in the root-owned config), checks its checksum, replaces the file and restarts the agent, and
//	               puts the old one back if the new one does not start. The agent decides nothing but "please update".
//	exec:<dir>     for a container: the agent downloads the new version itself, checks the checksum, makes sure the file
//	               says it is the version that was asked for, and replaces its own process (same process id, so the
//	               container keeps running). A restart of the container downloads the latest version anyway.
//
// Without the variable the agent never updates itself.
type SelfUpdate struct {
	Mode string
	Dir  string
	URL  string // the Lumen server, for the exec mode
	Now  func() time.Time
	HTTP *http.Client
}

// Outcome is what Request did.
type Outcome string

const (
	OutcomeNone      Outcome = ""          // nothing to do
	OutcomeRequested Outcome = "requested" // systemd: the request was written; the helper does the rest
	OutcomeExec      Outcome = "exec"      // exec: a verified new binary is ready; the caller replaces the process
	OutcomeSkipped   Outcome = "skipped"   // the same update was already tried within the last hour and the version did not change
)

var targetRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// NewSelfUpdate reads the setting ("systemd:/var/lib/lumen-agent", "exec:/tmp"). An empty or invalid value means no self-update.
func NewSelfUpdate(setting, serverURL string) *SelfUpdate {
	mode, dir, ok := strings.Cut(strings.TrimSpace(setting), ":")
	if !ok || (mode != "systemd" && mode != "exec") || !filepath.IsAbs(dir) {
		return nil
	}
	return &SelfUpdate{Mode: mode, Dir: filepath.Clean(dir), URL: strings.TrimRight(serverURL, "/"), Now: time.Now, HTTP: &http.Client{Timeout: 5 * time.Minute}}
}

// Capability is what the agent tells the server it can do: "systemd", "exec" or "" for nothing.
func (u *SelfUpdate) Capability() string {
	if u == nil {
		return ""
	}
	return u.Mode
}

func (u *SelfUpdate) attemptFile() string { return filepath.Join(u.Dir, "last-update-attempt") }

// recentlyTried tells whether this very update was already attempted within the last hour. It is what stops a restart loop
// when the server offers a version that, once installed, still does not report the version it asked for.
func (u *SelfUpdate) recentlyTried(target string) bool {
	b, err := os.ReadFile(u.attemptFile())
	if err != nil {
		return false
	}
	t, ts, ok := strings.Cut(strings.TrimSpace(string(b)), " ")
	n, err := strconv.ParseInt(ts, 10, 64)
	return ok && err == nil && t == target && u.Now().Sub(time.Unix(n, 0)) < time.Hour
}

func (u *SelfUpdate) markTried(target string) {
	_ = os.WriteFile(u.attemptFile(), []byte(fmt.Sprintf("%s %d\n", target, u.Now().Unix())), 0o644)
}

// Request is called with the version the server wants this agent to have. It returns what happened; for OutcomeExec the
// second value is the path of the new, verified binary.
func (u *SelfUpdate) Request(ctx context.Context, target string) (Outcome, string, error) {
	if u == nil || target == "" || target == Version {
		return OutcomeNone, "", nil
	}
	if !targetRe.MatchString(target) {
		return OutcomeNone, "", fmt.Errorf("the server asked for an update to a version that is not a plain name: refused")
	}
	if u.recentlyTried(target) {
		return OutcomeSkipped, "", nil
	}
	u.markTried(target)
	switch u.Mode {
	case "systemd":
		tmp := filepath.Join(u.Dir, ".update-requested.tmp")
		if err := os.WriteFile(tmp, []byte(target+"\n"), 0o644); err != nil {
			return OutcomeNone, "", fmt.Errorf("cannot write the update request: %w", err)
		}
		if err := os.Rename(tmp, filepath.Join(u.Dir, "update-requested")); err != nil {
			return OutcomeNone, "", fmt.Errorf("cannot write the update request: %w", err)
		}
		return OutcomeRequested, "", nil
	case "exec":
		p, err := u.fetchBinary(ctx, target)
		if err != nil {
			return OutcomeNone, "", err
		}
		return OutcomeExec, p, nil
	}
	return OutcomeNone, "", nil
}

// fetchBinary downloads the agent for this platform, checks it against the server's checksums and against the version it
// says it is, and leaves it in the state directory.
func (u *SelfUpdate) fetchBinary(ctx context.Context, target string) (string, error) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return "", errors.New("updating in place is not supported on " + runtime.GOOS)
	}
	name := "lumen-agent-" + runtime.GOOS + "-" + runtime.GOARCH
	get := func(path string, limit int64) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", u.URL+"/download/"+path, nil)
		if err != nil {
			return nil, err
		}
		resp, err := u.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil || int64(len(b)) > limit {
			return nil, fmt.Errorf("%s: too large or interrupted", path)
		}
		return b, nil
	}
	sums, err := get("SHA256SUMS", 1<<20)
	if err != nil {
		return "", err
	}
	want := ""
	for _, l := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(l); len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			want = f[0]
		}
	}
	if want == "" {
		return "", fmt.Errorf("the server has no checksum for %s", name)
	}
	bin, err := get(name, 256<<20)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(bin)
	if hex.EncodeToString(sum[:]) != want {
		return "", errors.New("checksum mismatch: the download is corrupt, refusing to update")
	}
	next := filepath.Join(u.Dir, "lumen-agent.next")
	if err := os.WriteFile(next, bin, 0o755); err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, next, "-version").Output()
	if got := strings.TrimSpace(string(out)); err != nil || got != target {
		os.Remove(next)
		return "", fmt.Errorf("the downloaded agent says it is version %q, not %q: refusing to update (is the server's download folder stale?)", got, target)
	}
	return next, nil
}
