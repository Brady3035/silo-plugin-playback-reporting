package report

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/Brady3035/silo-plugin-playback-reporting/internal/silo"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func ptr[T any](v T) *T { return &v }

func entry(id string, ended time.Time, watched float64, played *float64) silo.HistoryEntry {
	return silo.HistoryEntry{SessionID: id, UserID: "1", Username: "laura", MediaItemID: "m-" + id, MediaTitle: "Title " + id,
		MediaType: "movie", PlayMethod: "direct", StartedAt: ended.Add(-10 * time.Minute), EndedAt: ended,
		WatchedSeconds: watched, PlayedSeconds: played}
}

func TestWatchedSeconds(t *testing.T) {
	ended := time.Date(2026, 3, 1, 20, 0, 0, 0, time.UTC)
	cases := []struct {
		name          string
		e             silo.HistoryEntry
		want          float64
		wantEstimated bool
	}{
		// Resumed at 1:30:00 and watched two minutes: the measurement wins.
		{"measured", entry("a", ended, 5520, ptr(120.0)), 120, false},
		// Older row: the end position, capped at the ten-minute session.
		{"estimate capped at session length", entry("b", ended, 5520, nil), 600, true},
		{"estimate below session length", entry("c", ended, 300, nil), 300, true},
	}
	for _, tc := range cases {
		got, estimated := WatchedSeconds(tc.e)
		if got != tc.want || estimated != tc.wantEstimated {
			t.Errorf("%s: got %v (estimated %v), want %v (%v)", tc.name, got, estimated, tc.want, tc.wantEstimated)
		}
	}
}

// fakeHistory serves pages of two entries, newest ended first, honouring
// ended_after, as Silo's endpoint does.
type fakeHistory struct {
	entries []silo.HistoryEntry
	calls   []time.Time
	err     error
}

func (f *fakeHistory) HistoryPage(_ context.Context, endedAfter time.Time, cursor string) ([]silo.HistoryEntry, string, error) {
	f.calls = append(f.calls, endedAfter)
	if f.err != nil {
		return nil, "", f.err
	}
	var match []silo.HistoryEntry
	for _, e := range f.entries {
		if endedAfter.IsZero() || !e.EndedAt.Before(endedAfter) {
			match = append(match, e)
		}
	}
	start := 0
	fmt.Sscan(cursor, &start)
	end := min(start+2, len(match))
	next := ""
	if end < len(match) {
		next = fmt.Sprint(end)
	}
	return match[start:end], next, nil
}

func TestSyncerFullThenIncremental(t *testing.T) {
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	f := &fakeHistory{}
	for i := range 5 {
		f.entries = append([]silo.HistoryEntry{entry(fmt.Sprint(i), base.Add(time.Duration(i)*time.Hour), 60, ptr(60.0))}, f.entries...)
	}
	store := NewStore()
	s := NewSyncer(f, store, quiet)
	now := base.Add(10 * time.Hour)
	s.now = func() time.Time { return now }

	if err := s.syncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if store.Len() != 5 || !s.Status().Ready || !f.calls[0].IsZero() || len(f.calls) != 3 {
		t.Fatalf("full sync: %d plays, ready %v, calls %v", store.Len(), s.Status().Ready, f.calls)
	}

	// A new play, and the newest one finalized again with more watched: the
	// incremental read overlaps the newest play held and replaces it.
	f.entries = append([]silo.HistoryEntry{entry("5", base.Add(5*time.Hour), 60, ptr(60.0))}, f.entries...)
	f.entries[1] = entry("4", base.Add(4*time.Hour), 90, ptr(90.0))
	f.calls = nil
	if err := s.syncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if want := base.Add(4*time.Hour - incrementalOverlap); !f.calls[0].Equal(want) {
		t.Fatalf("incremental read since %v, want %v", f.calls[0], want)
	}
	plays, _ := store.Snapshot()
	if len(plays) != 6 || plays[0].SessionID != "5" || plays[1].Watched != 90 {
		t.Fatalf("after incremental: %+v", plays[:2])
	}

	// A full read later drops a play Silo no longer has.
	f.entries = f.entries[:len(f.entries)-1]
	now = now.Add(fullEvery)
	if err := s.syncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if store.Len() != 5 {
		t.Fatalf("full resync kept %d plays, want 5", store.Len())
	}

	// A failure keeps the data and reports the reason.
	f.err = &silo.StatusError{Status: 401}
	if err := s.syncOnce(t.Context()); err == nil || store.Len() != 5 || !strings.Contains(s.Status().LastError, "API key") {
		t.Fatalf("failed sync: err %v, plays %d, status %+v", err, store.Len(), s.Status())
	}
}

func TestEncodeChunksSplitsAndIndexes(t *testing.T) {
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	plays := make([]Play, playsPerChunk+3)
	for i := range plays {
		plays[i] = PlayFrom(entry(fmt.Sprint(i), base.Add(-time.Duration(i)*time.Minute), 60, ptr(42.0)))
	}
	plays[0].Series, plays[0].Season, plays[0].Episode, plays[0].Type = "Show", 2, 4, "episode"
	tracker := &Tracker{sessions: map[string]ClientInfo{"0": {Client: "Silo for iOS", Location: "remote", Method: "transcode"}}}

	chunks, err := encodeChunks("snap", plays, Status{Ready: true}, features{Clients: true}, tracker, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	var total int
	for i, b := range chunks {
		if len(b) > 3<<20 {
			t.Errorf("chunk %d is %d bytes, over the host's response budget", i, len(b))
		}
		var c chunk
		if err := json.Unmarshal(b, &c); err != nil {
			t.Fatal(err)
		}
		if c.Index != i || c.Count != 2 || c.Snapshot != "snap" || len(c.Columns) != len(rowColumns) {
			t.Fatalf("chunk %d header: %+v", i, c)
		}
		total += len(c.Plays)
	}
	if total != len(plays) {
		t.Fatalf("chunks hold %d plays, want %d", total, len(plays))
	}
	var c chunk
	_ = json.Unmarshal(chunks[0], &c)
	col := map[string]int{}
	for i, name := range c.Columns {
		col[name] = i
	}
	row := c.Plays[0]
	at := func(name string) int { return int(row[col[name]].(float64)) }
	if c.Dict.Series[at("series")] != "Show" || at("season") != 2 || at("episode") != 4 || at("watched") != 42 ||
		c.Dict.Clients[at("client")] != "Silo for iOS" || c.Dict.Methods[at("live_method")] != "transcode" ||
		c.Dict.Locations[at("location")] != "remote" {
		t.Fatalf("first row decodes wrong: %v with %+v", row, c.Dict)
	}
	if second := c.Plays[1]; int(second[col["client"]].(float64)) != -1 {
		t.Fatalf("untracked play has a client: %v", second)
	}
}

func request(path, role string, query map[string]any) *pluginv1.HandleHTTPRequest {
	q, _ := structpb.NewStruct(query)
	return &pluginv1.HandleHTTPRequest{Method: "GET", Path: path, Headers: map[string]string{"X-Silo-User-Role": role}, Query: q}
}

func TestHandlerAccessAndAssets(t *testing.T) {
	h := &Handler{Store: NewStore()}
	h.Configure(nil, nil, "")
	for _, tc := range []struct {
		path, role string
		want       int32
	}{
		{"/", "user", 403},
		{"/data", "", 403},
		{"/", "admin", 200},
		{"/assets/app.js", "admin", 200},
		{"/assets/../manifest.json", "admin", 404},
		{"/assets/index.html", "admin", 404},
		{"/data", "admin", 200},
		{"/nope", "admin", 404},
	} {
		resp, _ := h.Handle(t.Context(), request(tc.path, tc.role, nil))
		if resp.GetStatusCode() != tc.want {
			t.Errorf("GET %s as %q: %d, want %d", tc.path, tc.role, resp.GetStatusCode(), tc.want)
		}
	}
	h.Configure(nil, nil, "Add an API key.")
	if resp, _ := h.Handle(t.Context(), request("/data", "admin", nil)); resp.GetStatusCode() != 503 {
		t.Fatalf("unconfigured data: %d", resp.GetStatusCode())
	}
	if resp, _ := h.Handle(t.Context(), request("/", "admin", nil)); !strings.Contains(string(resp.GetBody()), "Add an API key.") {
		t.Fatal("unconfigured page does not say what is missing")
	}
}

func TestHandlerServesChunksFromOneSnapshot(t *testing.T) {
	store := NewStore()
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	store.Upsert([]Play{PlayFrom(entry("a", base, 60, ptr(60.0)))})
	h := &Handler{Store: store, now: func() time.Time { return base }}
	h.Configure(nil, nil, "")

	resp, _ := h.Handle(t.Context(), request("/data", "admin", map[string]any{"chunk": "0"}))
	var first chunk
	if err := json.Unmarshal(resp.GetBody(), &first); err != nil || first.Snapshot == "" {
		t.Fatalf("first chunk: %s", resp.GetBody())
	}
	// A sync lands while the page loads; the snapshot it started from still
	// answers, unchanged.
	store.Upsert([]Play{PlayFrom(entry("b", base.Add(time.Minute), 60, ptr(60.0)))})
	resp, _ = h.Handle(t.Context(), request("/data", "admin", map[string]any{"chunk": "0", "snapshot": first.Snapshot}))
	var again chunk
	_ = json.Unmarshal(resp.GetBody(), &again)
	if again.Snapshot != first.Snapshot || len(again.Plays) != 1 {
		t.Fatalf("snapshot changed under the page: %s", resp.GetBody())
	}
	h.now = func() time.Time { return base.Add(snapshotTTL + time.Second) }
	if resp, _ = h.Handle(t.Context(), request("/data", "admin", map[string]any{"chunk": "0", "snapshot": first.Snapshot})); resp.GetStatusCode() != 410 {
		t.Fatalf("expired snapshot: %d", resp.GetStatusCode())
	}
}

type fakeSessions struct{ pages [][]silo.LiveSession }

func (f *fakeSessions) SessionsPage(_ context.Context, cursor string) ([]silo.LiveSession, string, error) {
	i := 0
	fmt.Sscan(cursor, &i)
	next := ""
	if i+1 < len(f.pages) {
		next = fmt.Sprint(i + 1)
	}
	return f.pages[i], next, nil
}

type memState map[string][]byte

func (m memState) ReadInstanceState(_ context.Context, key string) ([]byte, bool, error) {
	v, ok := m[key]
	return v, ok, nil
}

func (m memState) WriteInstanceState(_ context.Context, key string, value []byte) error {
	if len(value) > 256<<10 {
		return fmt.Errorf("value for %s is %d bytes", key, len(value))
	}
	m[key] = append([]byte(nil), value...)
	return nil
}

func TestTrackerKeepsClientsAndTheMostTranscodedMethod(t *testing.T) {
	f := &fakeSessions{pages: [][]silo.LiveSession{
		{{SessionID: "s1", ClientName: "Silo for iOS", StreamLocation: "remote", EffectivePlayMethod: "direct"}},
		{{SessionID: "s2", ClientLabel: "Infuse 8", StreamLocation: "local", EffectivePlayMethod: "remux"}},
	}}
	state := memState{}
	tr := NewTracker(f, state, quiet)
	tr.poll(t.Context())
	// s1 falls back to transcoding and its client name disappears from a
	// later observation; the earlier name stays and the transcode counts.
	f.pages = [][]silo.LiveSession{{{SessionID: "s1", EffectivePlayMethod: "transcode"}}}
	tr.poll(t.Context())
	f.pages = [][]silo.LiveSession{{{SessionID: "s1", ClientName: "Silo for iOS", EffectivePlayMethod: "direct"}}}
	tr.poll(t.Context())
	if got, _ := tr.Lookup("s1"); got.Client != "Silo for iOS" || got.Method != "transcode" || got.Location != "remote" {
		t.Fatalf("s1 = %+v", got)
	}
	if got, _ := tr.Lookup("s2"); got.Client != "Infuse 8" {
		t.Fatalf("s2 falls back to the client label: %+v", got)
	}

	// Enough sessions to need several state chunks survive a restart.
	for i := range 6000 {
		tr.sessions[fmt.Sprintf("bulk-%05d-%s", i, strings.Repeat("x", 30))] = ClientInfo{Client: "Some long client name 1.2.3", Location: "local", Method: "direct", Seen: int64(i)}
	}
	tr.dirty = true
	tr.save(t.Context())
	if len(state) < 3 {
		t.Fatalf("saved in %d keys, want several chunks", len(state))
	}
	restored := NewTracker(f, state, quiet)
	if !restored.load(t.Context()) || len(restored.sessions) != len(tr.sessions) {
		t.Fatalf("restored %d sessions, want %d", len(restored.sessions), len(tr.sessions))
	}
}

func TestOptionsHideShortAndIgnoredPlays(t *testing.T) {
	rules := ParseIgnore("  Kids/Guest \n# comment\n\ntestuser\nKIDS/guest\n/nobody\n")
	if len(rules) != 2 || rules[0] != (IgnoreRule{"kids", "guest"}) || rules[1] != (IgnoreRule{"testuser", ""}) {
		t.Fatalf("rules: %+v", rules)
	}
	colors, palette := ParseColors(map[string]any{"transcode": " #AABBCC ", "movies": "red", "unknown": "#000000",
		"palette": "#111111, nope ,#222222"})
	if len(colors) != 1 || colors["transcode"] != "#aabbcc" || len(palette) != 2 || palette[1] != "#222222" {
		t.Fatalf("colors %v palette %v", colors, palette)
	}

	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	mk := func(id, user, profile string, watched float64) Play {
		p := PlayFrom(entry(id, base, watched, ptr(watched)))
		p.User, p.Profile = user, profile
		return p
	}
	store := NewStore()
	store.Upsert([]Play{
		mk("short", "laura", "Laura", 20),
		mk("guest", "kids", "Guest", 600),
		mk("kid", "kids", "Sam", 600),
		mk("test", "TestUser", "TestUser", 600),
		mk("ok", "laura", "Laura", 600),
	})
	h := &Handler{Store: store, now: func() time.Time { return base }}
	h.Configure(nil, nil, "")
	h.SetOptions(Options{MinSeconds: 30, Ignore: rules, Colors: colors, Palette: palette})
	resp, _ := h.Handle(t.Context(), request("/data", "admin", map[string]any{"chunk": "0"}))
	var c chunk
	if err := json.Unmarshal(resp.GetBody(), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Plays) != 2 || c.Features.Hidden != 3 || c.Features.MinSeconds != 30 || c.Features.IgnoreRules != 2 {
		t.Fatalf("got %d plays, features %+v", len(c.Plays), c.Features)
	}
}

func TestColorsSaveAndLoad(t *testing.T) {
	state := memState{}
	h := &Handler{Store: NewStore(), State: state}
	h.Configure(nil, nil, "")
	// The old settings form's colors show until colors are saved here.
	h.SetOptions(Options{Colors: map[string]string{"tv": "#123456"}})
	if got := h.colors(t.Context()); got.Colors["tv"] != "#123456" {
		t.Fatalf("legacy colors: %+v", got)
	}

	post := func(body, contentType string) *pluginv1.HandleHTTPResponse {
		req := request("/settings", "admin", nil)
		req.Method = "POST"
		req.Body = []byte(body)
		req.Headers["Content-Type"] = contentType
		resp, _ := h.Handle(t.Context(), req)
		return resp
	}
	if r := post(`{"colors":{"transcode":"#ff0000"}}`, "text/plain"); r.GetStatusCode() != 415 {
		t.Fatalf("non-JSON post: %d", r.GetStatusCode())
	}
	r := post(`{"colors":{"transcode":"#FF0000","movies":"blue","bogus":"#000000"},"palette":["#111111","x","#222222"],"show_audiobooks":true}`, "application/json; charset=utf-8")
	if r.GetStatusCode() != 200 {
		t.Fatalf("save: %d %s", r.GetStatusCode(), r.GetBody())
	}
	var saved ColorSettings
	_ = json.Unmarshal(r.GetBody(), &saved)
	if len(saved.Colors) != 1 || saved.Colors["transcode"] != "#ff0000" || len(saved.Palette) != 2 || !saved.ShowAudiobooks {
		t.Fatalf("saved %+v", saved)
	}

	// A new process reads them back from the plugin's state.
	again := &Handler{Store: NewStore(), State: state}
	again.Configure(nil, nil, "")
	if got := again.colors(t.Context()); got.Colors["transcode"] != "#ff0000" || got.Colors["tv"] != "" || len(got.Palette) != 2 || !got.ShowAudiobooks {
		t.Fatalf("reloaded %+v", got)
	}
	page, _ := again.Handle(t.Context(), request("/", "admin", nil))
	if !strings.Contains(string(page.GetBody()), `"transcode":"#ff0000"`) {
		t.Fatalf("page lacks saved colors")
	}
	get := request("/settings", "admin", nil)
	if r, _ := again.Handle(t.Context(), get); r.GetStatusCode() != 405 {
		t.Fatalf("GET colors: %d", r.GetStatusCode())
	}
}
