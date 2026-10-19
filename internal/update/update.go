// Package update finds out whether a newer rimor has been released.
//
// At most once a day it asks GitHub for the latest release, without
// credentials: one HTTPS request to api.github.com, which sees the
// address it comes from and a User-Agent naming rimor's version. The answer
// is kept in the state folder, so starts in between, or offline, still
// know it. Failures are silent: the next start tries again.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Endpoint is GitHub's latest-release API for rimor; it leaves out
// prereleases and drafts.
var Endpoint = "https://api.github.com/repos/alchemy/rimor/releases/latest"

// Interval is how long an answer is trusted before asking again.
const Interval = 24 * time.Hour

// Timeout bounds the request: a slow network must not hold anything up.
const Timeout = 3 * time.Second

// State is what rimor remembers between starts, in update.json.
type State struct {
	Checked time.Time `json:"checked"`
	Latest  string    `json:"latest,omitempty"` // tag, e.g. "v0.7.0"
	URL     string    `json:"url,omitempty"`    // the release's page
	// Notified is the release the user was last told about, so the
	// startup note appears once per release.
	Notified string `json:"notified,omitempty"`
}

// Due reports whether it is time to ask again.
func (s State) Due(now time.Time) bool {
	return s.Checked.IsZero() || now.Sub(s.Checked) >= Interval || now.Before(s.Checked)
}

// Load reads the state; a missing or unreadable file is an empty one.
func Load(path string) State {
	var s State
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &s)
	}
	return s
}

// Save writes the state atomically.
func (s State) Save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Check asks GitHub for the latest release and records it in s.
func Check(ctx context.Context, client *http.Client, current string, s State, now time.Time) (State, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Endpoint, nil)
	if err != nil {
		return s, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "rimor/"+current) // GitHub requires one
	resp, err := client.Do(req)
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return s, fmt.Errorf("github: %s", resp.Status)
	}
	var release struct {
		Tag        string `json:"tag_name"`
		URL        string `json:"html_url"`
		Prerelease bool   `json:"prerelease"`
		Draft      bool   `json:"draft"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return s, err
	}
	if release.Tag == "" || release.Prerelease || release.Draft {
		return s, errors.New("github: no release")
	}
	s.Checked, s.Latest, s.URL = now, release.Tag, release.URL
	return s, nil
}

// Newer reports whether latest is a later release than current. A
// development build, or a version that is not v-major.minor.patch, never
// has a newer one.
func Newer(current, latest string) bool {
	c, ok := parse(current)
	l, ok2 := parse(latest)
	if !ok || !ok2 {
		return false
	}
	for i := range 3 {
		if l.n[i] != c.n[i] {
			return l.n[i] > c.n[i]
		}
	}
	// v1.2.3 is later than v1.2.3-rc.1; prereleases compare as text.
	switch {
	case c.pre == l.pre:
		return false
	case l.pre == "":
		return true
	case c.pre == "":
		return false
	}
	return l.pre > c.pre
}

type semver struct {
	n   [3]int
	pre string
}

func parse(v string) (semver, bool) {
	var s semver
	v, ok := strings.CutPrefix(v, "v")
	if !ok {
		return s, false
	}
	v, s.pre, _ = strings.Cut(v, "-")
	v, _, _ = strings.Cut(v, "+") // build metadata does not count
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return s, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return s, false
		}
		s.n[i] = n
	}
	return s, true
}

// Packaged reports whether the running binary was installed by a system
// package manager (on Linux, under /usr but not /usr/local, as the AUR
// packages do), which keeps it up to date: such installs do not check.
func Packaged() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return strings.HasPrefix(exe, "/usr/") && !strings.HasPrefix(exe, "/usr/local/")
}
