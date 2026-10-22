// Package silo reads the Silo v2 admin API the reports are built from.
package silo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PageLimit is the largest page the v2 collections serve.
const PageLimit = 200

// maxBody bounds one API response.
const maxBody = 16 << 20

// Client calls Silo's v2 API with an admin API key.
type Client struct {
	base string
	key  string
	http *http.Client
}

// New returns a client for the server at base (scheme and host, no /api).
func New(base, apiKey string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{base: strings.TrimRight(base, "/"), key: apiKey, http: httpClient}
}

// HistoryEntry is one finalized play from GET /api/v2/admin/playback-history.
type HistoryEntry struct {
	SessionID            string    `json:"session_id"`
	UserID               string    `json:"user_id"`
	Username             string    `json:"username"`
	ProfileID            string    `json:"profile_id"`
	ProfileName          string    `json:"profile_name"`
	MediaItemID          string    `json:"media_item_id"`
	MediaTitle           string    `json:"media_title"`
	MediaType            string    `json:"media_type"`
	PlayMethod           string    `json:"play_method"`
	StartedAt            time.Time `json:"started_at"`
	EndedAt              time.Time `json:"ended_at"`
	WatchedSeconds       float64   `json:"watched_seconds"`
	DurationSeconds      *float64  `json:"duration_seconds"`
	Completed            bool      `json:"completed"`
	StartPositionSeconds *float64  `json:"start_position_seconds"`
	PlayedSeconds        *float64  `json:"played_seconds"`
	PausedSeconds        *float64  `json:"paused_seconds"`
	SeriesTitle          string    `json:"series_title"`
	SeasonNumber         *int      `json:"season_number"`
	EpisodeNumber        *int      `json:"episode_number"`
}

// LiveSession is the part of GET /api/v2/admin/sessions the client report uses.
type LiveSession struct {
	SessionID           string `json:"session_id"`
	ClientName          string `json:"client_name"`
	ClientLabel         string `json:"client_label"`
	StreamLocation      string `json:"stream_location"`
	EffectivePlayMethod string `json:"effective_play_method"`
}

type page[T any] struct {
	Items []T `json:"items"`
	Page  *struct {
		NextCursor string `json:"next_cursor"`
		HasMore    bool   `json:"has_more"`
	} `json:"page"`
}

// HistoryPage reads one page of playback history, newest ended first. A
// non-zero endedAfter keeps plays that ended at or after it. next is empty on
// the last page.
func (c *Client) HistoryPage(ctx context.Context, endedAfter time.Time, cursor string) (items []HistoryEntry, next string, err error) {
	q := url.Values{"limit": {fmt.Sprint(PageLimit)}}
	if !endedAfter.IsZero() {
		q.Set("ended_after", endedAfter.UTC().Format(time.RFC3339Nano))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var out page[HistoryEntry]
	if err := c.get(ctx, "/api/v2/admin/playback-history", q, &out); err != nil {
		return nil, "", err
	}
	if out.Page != nil && out.Page.HasMore {
		next = out.Page.NextCursor
	}
	return out.Items, next, nil
}

// SessionsPage reads one page of live playback sessions.
func (c *Client) SessionsPage(ctx context.Context, cursor string) (items []LiveSession, next string, err error) {
	q := url.Values{"limit": {fmt.Sprint(PageLimit)}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var out page[LiveSession]
	if err := c.get(ctx, "/api/v2/admin/sessions", q, &out); err != nil {
		return nil, "", err
	}
	if out.Page != nil && out.Page.HasMore {
		next = out.Page.NextCursor
	}
	return out.Items, next, nil
}

// Account is the part of GET /api/v2/account/me the connection check uses:
// whose key this is.
type Account struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

// Account reads the account the API key belongs to.
func (c *Client) Account(ctx context.Context) (Account, error) {
	var out Account
	err := c.get(ctx, "/api/v2/account/me", url.Values{}, &out)
	return out, err
}

// ProbeHistory reads one history entry, to check the key can.
func (c *Client) ProbeHistory(ctx context.Context) error {
	var out page[json.RawMessage]
	return c.get(ctx, "/api/v2/admin/playback-history", url.Values{"limit": {"1"}}, &out)
}

// ProbeSessions reads one live session, to check the key can.
func (c *Client) ProbeSessions(ctx context.Context) error {
	var out page[json.RawMessage]
	return c.get(ctx, "/api/v2/admin/sessions", url.Values{"limit": {"1"}}, &out)
}

// Base is the address the client calls.
func (c *Client) Base() string { return c.base }

// NetworkError is a request that got no HTTP answer from Silo.
type NetworkError struct{ Err error }

func (e *NetworkError) Error() string { return e.Err.Error() }
func (e *NetworkError) Unwrap() error { return e.Err }

// StatusError is a non-2xx answer from Silo.
type StatusError struct {
	Status int
	Detail string
}

func (e *StatusError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("silo answered %d: %s", e.Status, e.Detail)
	}
	return fmt.Sprintf("silo answered %d", e.Status)
}

// Explain turns an error from the client into a sentence for the admin page.
func Explain(err error) string {
	var status *StatusError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &status) && status.Status == http.StatusUnauthorized:
		return "Silo rejected the API key. Check the key in the plugin's settings."
	case errors.As(err, &status) && status.Status == http.StatusForbidden:
		return "The API key's account cannot read admin playback history. Use a key without scopes on an admin account."
	case errors.As(err, &status) && status.Status == http.StatusNotFound:
		return "This Silo server has no v2 playback history API. Update Silo."
	default:
		return "Could not read playback history from Silo: " + err.Error()
	}
}

func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	if c.key == "" {
		return errors.New("no API key is configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return &NetworkError{Err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return err
	}
	if len(body) > maxBody {
		return fmt.Errorf("response from %s is larger than %d bytes", path, maxBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var problem struct {
			Detail string `json:"detail"`
			Title  string `json:"title"`
		}
		_ = json.Unmarshal(body, &problem)
		detail := problem.Detail
		if detail == "" {
			detail = problem.Title
		}
		return &StatusError{Status: resp.StatusCode, Detail: detail}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}
