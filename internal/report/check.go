package report

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Brady3035/silo-plugin-playback-reporting/internal/silo"
)

// The connection check tries, in order, everything the plugin needs from
// Silo with the configured key, and says which step failed and why. It runs
// when the plugin starts and again whenever an admin asks on the page.

// CheckStep is one line of the result.
type CheckStep struct {
	Name   string `json:"name"`
	State  string `json:"state"` // ok, fail, warn or skip
	Detail string `json:"detail,omitempty"`
}

// CheckResult is a whole check.
type CheckResult struct {
	At      time.Time   `json:"at"`
	OK      bool        `json:"ok"`
	Server  string      `json:"server,omitempty"`
	Account string      `json:"account,omitempty"`
	Steps   []CheckStep `json:"steps"`
}

// Prober is what the check calls; main's lazy client implements it.
type Prober interface {
	Client(ctx context.Context) (Probe, error)
}

// Probe is a connected Silo client.
type Probe interface {
	Base() string
	Account(ctx context.Context) (silo.Account, error)
	ProbeHistory(ctx context.Context) error
	ProbeSessions(ctx context.Context) error
}

const (
	stepOK   = "ok"
	stepFail = "fail"
	stepWarn = "warn"
	stepSkip = "skip"
)

// RunCheck checks the connection and the key. trackClients adds the live
// sessions step.
func RunCheck(ctx context.Context, p Prober, trackClients bool, now time.Time) CheckResult {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r := CheckResult{At: now, OK: true}
	add := func(name, state, detail string) {
		r.Steps = append(r.Steps, CheckStep{Name: name, State: state, Detail: detail})
		if state == stepFail {
			r.OK = false
		}
	}
	skipRest := func(names ...string) {
		for _, n := range names {
			add(n, stepSkip, "Skipped until the step above works.")
		}
	}
	const (
		find     = "Find Silo's address"
		reach    = "Connect to Silo"
		key      = "API key accepted"
		admin    = "Key belongs to an admin"
		history  = "Read playback history"
		sessions = "Read live sessions"
	)
	later := []string{reach, key, admin, history}
	if trackClients {
		later = append(later, sessions)
	}

	client, err := p.Client(ctx)
	if err != nil {
		add(find, stepFail, err.Error())
		skipRest(later...)
		return r
	}
	r.Server = client.Base()
	add(find, stepOK, r.Server)

	acct, err := client.Account(ctx)
	var status *silo.StatusError
	var network *silo.NetworkError
	switch {
	case errors.As(err, &network):
		add(reach, stepFail, fmt.Sprintf("No answer from %s: %v. If the plugin runs somewhere that can't reach Silo directly, set Server address under Silo connection.", r.Server, network.Err))
		skipRest(later[1:]...)
		return r
	case err != nil && errors.As(err, &status) && status.Status == http.StatusUnauthorized:
		add(reach, stepOK, "")
		add(key, stepFail, "Silo rejected the key. It may be mistyped, revoked or expired; create a new one under Settings → API keys.")
		skipRest(later[2:]...)
		return r
	case err != nil && errors.As(err, &status) && status.Status == http.StatusNotFound:
		add(reach, stepOK, "")
		add(key, stepFail, "This Silo server has no v2 API at "+r.Server+". Update Silo, or check Server address.")
		skipRest(later[2:]...)
		return r
	case err != nil && errors.As(err, &status):
		// The key works but can't read its own account: a scoped key, most
		// likely. The history step below says whether that matters.
		add(reach, stepOK, "")
		add(key, stepOK, "")
		add(admin, stepWarn, fmt.Sprintf("Silo answered %d when asked whose key this is. Scoped keys can't read admin history; use a key with no scopes.", status.Status))
	case err != nil:
		add(reach, stepFail, err.Error())
		skipRest(later[1:]...)
		return r
	default:
		add(reach, stepOK, "")
		r.Account = acct.Username
		add(key, stepOK, "Belongs to "+acct.Username+".")
		if strings.EqualFold(acct.Role, "admin") {
			add(admin, stepOK, "")
		} else {
			add(admin, stepFail, acct.Username+" isn't an admin. Create the key on an admin account.")
		}
	}

	if err := client.ProbeHistory(ctx); err != nil {
		add(history, stepFail, silo.Explain(err))
	} else {
		add(history, stepOK, "")
	}
	if trackClients {
		if err := client.ProbeSessions(ctx); err != nil {
			msg := silo.Explain(err)
			if errors.As(err, &status) && status.Status == http.StatusForbidden {
				msg = "The key's account can't read live sessions, so Track clients can't work. Use a key with no scopes on an admin account."
			}
			add(sessions, stepFail, msg)
		} else {
			add(sessions, stepOK, "")
		}
	}
	return r
}

// FirstProblem is the first failing step, for logs and the page banner.
func (r CheckResult) FirstProblem() string {
	for _, s := range r.Steps {
		if s.State == stepFail {
			if s.Detail == "" {
				return s.Name + " failed."
			}
			return s.Name + ": " + s.Detail
		}
	}
	return ""
}
