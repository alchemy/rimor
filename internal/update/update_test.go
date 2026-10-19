package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		current, latest string
		want            bool
	}{
		{"v0.6.0", "v0.7.0", true},
		{"v0.6.0", "v0.6.1", true},
		{"v0.6.0", "v1.0.0", true},
		{"v0.10.0", "v0.9.9", false}, // numbers, not text
		{"v0.6.0", "v0.6.0", false},
		{"v0.7.0", "v0.6.0", false},
		{"v0.7.0-rc.1", "v0.7.0", true},
		{"v0.7.0", "v0.7.0-rc.2", false},
		{"v0.7.0-rc.1", "v0.7.0-rc.2", true},
		{"v0.6.0+build.5", "v0.6.0", false},
		{"dev", "v9.9.9", false},
		{"v0.6.0", "", false},
		{"v0.6.0", "nightly", false},
		{"0.6.0", "v0.7.0", false},
	} {
		if got := Newer(c.current, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.current, c.latest, got)
		}
	}
}

func TestDue(t *testing.T) {
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		checked time.Time
		want    bool
	}{
		{time.Time{}, true},
		{now.Add(-time.Hour), false},
		{now.Add(-25 * time.Hour), true},
		{now.Add(time.Hour), true}, // the clock went back: ask
	} {
		if got := (State{Checked: c.checked}).Due(now); got != c.want {
			t.Errorf("checked %v: due %v", c.checked, got)
		}
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "update.json")
	if s := Load(path); !s.Checked.IsZero() {
		t.Errorf("missing file: %+v", s)
	}
	want := State{Checked: time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC), Latest: "v0.7.0", URL: "https://x", Notified: "v0.7.0"}
	if err := want.Save(path); err != nil {
		t.Fatal(err)
	}
	if got := Load(path); got != want {
		t.Errorf("loaded %+v", got)
	}
}

// github serves a latest-release answer, recording what was asked.
func github(t *testing.T, status int, body string) (*httptest.Server, *http.Request) {
	var seen http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = *r
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	old := Endpoint
	Endpoint = srv.URL + "/repos/alchemy/rimor/releases/latest"
	t.Cleanup(func() { Endpoint = old })
	return srv, &seen
}

func TestCheck(t *testing.T) {
	srv, seen := github(t, http.StatusOK, `{"tag_name":"v0.7.0","html_url":"https://github.com/alchemy/rimor/releases/tag/v0.7.0","prerelease":false,"draft":false}`)
	now := time.Now()
	s, err := Check(context.Background(), srv.Client(), "v0.6.0", State{Notified: "v0.6.5"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if s.Latest != "v0.7.0" || !strings.HasSuffix(s.URL, "/v0.7.0") || !s.Checked.Equal(now) || s.Notified != "v0.6.5" {
		t.Errorf("state %+v", s)
	}
	if ua := seen.Header.Get("User-Agent"); ua != "rimor/v0.6.0" || seen.Header.Get("Authorization") != "" {
		t.Errorf("sent User-Agent %q, Authorization %q", ua, seen.Header.Get("Authorization"))
	}
}

func TestCheckFailsQuietly(t *testing.T) {
	before := State{Latest: "v0.6.0", Checked: time.Unix(1, 0)}
	for name, c := range map[string]struct {
		status int
		body   string
	}{
		"rate limited": {http.StatusForbidden, `{"message":"API rate limit exceeded"}`},
		"no release":   {http.StatusNotFound, `{"message":"Not Found"}`},
		"prerelease":   {http.StatusOK, `{"tag_name":"v0.8.0-rc.1","prerelease":true}`},
		"garbage":      {http.StatusOK, `<html>`},
	} {
		srv, _ := github(t, c.status, c.body)
		s, err := Check(context.Background(), srv.Client(), "v0.6.0", before, time.Now())
		if err == nil || s != before {
			t.Errorf("%s: %v, state %+v", name, err, s)
		}
	}

	// A server that does not answer is given up on.
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hang.Close()
	Endpoint = hang.URL
	start := time.Now()
	if _, err := Check(context.Background(), hang.Client(), "v0.6.0", before, time.Now()); err == nil {
		t.Error("no error from a server that never answers")
	}
	if waited := time.Since(start); waited > Timeout+time.Second {
		t.Errorf("waited %v", waited)
	}
}
