package report

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Brady3035/silo-plugin-playback-reporting/internal/silo"
)

// fakeSilo answers the endpoints the check calls with fixed statuses.
type fakeSilo struct {
	key            string
	role           string
	accountStatus  int
	historyStatus  int
	sessionsStatus int
}

func (f fakeSilo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.key {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	status := map[string]int{"/api/v2/account/me": f.accountStatus, "/api/v2/admin/playback-history": f.historyStatus,
		"/api/v2/admin/sessions": f.sessionsStatus}[r.URL.Path]
	if status == 0 {
		status = http.StatusOK
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"detail":"nope"}`))
		return
	}
	if r.URL.Path == "/api/v2/account/me" {
		_ = json.NewEncoder(w).Encode(map[string]string{"username": "brady", "role": f.role})
		return
	}
	_, _ = w.Write([]byte(`{"items":[],"page":{"has_more":false}}`))
}

type staticProber struct {
	client *silo.Client
	err    error
}

func (s staticProber) Client(context.Context) (Probe, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.client, nil
}

func states(r CheckResult) string {
	var parts []string
	for _, s := range r.Steps {
		parts = append(parts, s.State)
	}
	return strings.Join(parts, ",")
}

func TestConnectionCheck(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	run := func(f fakeSilo, key string, track bool) CheckResult {
		srv := httptest.NewServer(f)
		defer srv.Close()
		return RunCheck(t.Context(), staticProber{client: silo.New(srv.URL, key, nil)}, track, now)
	}
	for _, tc := range []struct {
		name  string
		f     fakeSilo
		key   string
		track bool
		want  string
		ok    bool
		about string
	}{
		{"working admin key", fakeSilo{key: "sa_ok", role: "admin"}, "sa_ok", true, "ok,ok,ok,ok,ok,ok", true, ""},
		{"tracking off skips sessions", fakeSilo{key: "sa_ok", role: "admin"}, "sa_ok", false, "ok,ok,ok,ok,ok", true, ""},
		{"wrong key", fakeSilo{key: "sa_ok", role: "admin"}, "sa_typo", true, "ok,ok,fail,skip,skip,skip", false, "rejected"},
		{"non-admin key", fakeSilo{key: "sa_ok", role: "user", historyStatus: 403, sessionsStatus: 403}, "sa_ok", false, "ok,ok,ok,fail,fail", false, "isn't an admin"},
		{"scoped key", fakeSilo{key: "sa_ok", role: "admin", accountStatus: 403, historyStatus: 403}, "sa_ok", false, "ok,ok,ok,warn,fail", false, "no scopes"},
		{"sessions forbidden", fakeSilo{key: "sa_ok", role: "admin", sessionsStatus: 403}, "sa_ok", true, "ok,ok,ok,ok,ok,fail", false, "Track clients"},
		{"old Silo", fakeSilo{key: "sa_ok", role: "admin", accountStatus: 404}, "sa_ok", false, "ok,ok,fail,skip,skip", false, "Update Silo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := run(tc.f, tc.key, tc.track)
			if states(r) != tc.want || r.OK != tc.ok || (tc.about != "" && !strings.Contains(r.FirstProblem()+states(r)+stepDetails(r), tc.about)) {
				t.Fatalf("got %s ok=%v problem=%q details=%s", states(r), r.OK, r.FirstProblem(), stepDetails(r))
			}
		})
	}

	// Nothing listening at the address.
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close()
	r := RunCheck(t.Context(), staticProber{client: silo.New(addr, "sa_ok", nil)}, false, now)
	if states(r) != "ok,fail,skip,skip,skip" || !strings.Contains(r.FirstProblem(), "No answer from") {
		t.Fatalf("unreachable: %s %q", states(r), r.FirstProblem())
	}
	// No address to call at all.
	r = RunCheck(t.Context(), staticProber{err: errors.New("Silo reported no address")}, false, now)
	if states(r) != "fail,skip,skip,skip,skip" || r.OK {
		t.Fatalf("no address: %s", states(r))
	}
}

func stepDetails(r CheckResult) string {
	var parts []string
	for _, s := range r.Steps {
		parts = append(parts, s.Detail)
	}
	return strings.Join(parts, " | ")
}

func TestCheckRoute(t *testing.T) {
	h := &Handler{Store: NewStore()}
	h.Configure(nil, nil, "")
	if resp, _ := h.Handle(t.Context(), request("/check", "admin", nil)); resp.GetStatusCode() != 503 {
		t.Fatalf("no checker: %d", resp.GetStatusCode())
	}
	runs := 0
	h.SetChecker(func(context.Context) CheckResult {
		runs++
		return CheckResult{At: time.Now(), OK: true, Steps: []CheckStep{{Name: "x", State: stepOK}}}
	})
	for range 3 {
		resp, _ := h.Handle(t.Context(), request("/check", "admin", nil))
		var r CheckResult
		if err := json.Unmarshal(resp.GetBody(), &r); err != nil || !r.OK {
			t.Fatalf("check route: %s", resp.GetBody())
		}
	}
	if runs != 1 {
		t.Fatalf("ran %d times in a burst, want 1", runs)
	}
	page, _ := h.Handle(t.Context(), request("/", "admin", nil))
	if !strings.Contains(string(page.GetBody()), `"steps":[{"name":"x","state":"ok"}]`) {
		t.Fatalf("page lacks the last check")
	}
}
