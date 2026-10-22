package report

import (
	"bytes"
	"compress/gzip"
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

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

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
		if err := json.Unmarshal(gunzip(t, b), &c); err != nil {
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
	_ = json.Unmarshal(gunzip(t, chunks[0]), &c)
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
	if err := json.Unmarshal(gunzip(t, resp.GetBody()), &first); err != nil || first.Snapshot == "" {
		t.Fatalf("first chunk: %s", resp.GetBody())
	}
	// A sync lands while the page loads; the snapshot it started from still
	// answers, unchanged.
	store.Upsert([]Play{PlayFrom(entry("b", base.Add(time.Minute), 60, ptr(60.0)))})
	resp, _ = h.Handle(t.Context(), request("/data", "admin", map[string]any{"chunk": "0", "snapshot": first.Snapshot}))
	var again chunk
	_ = json.Unmarshal(gunzip(t, resp.GetBody()), &again)
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

}

// bulkSessions adds n sessions first seen in the given month.
func bulkSessions(tr *Tracker, month time.Time, n int, prefix string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for i := range n {
		info := ClientInfo{Client: "Some long client name 1.2.3", Location: "local", Method: "direct",
			Seen: month.Unix() + int64(i), First: month.Unix() + int64(i)}
		tr.sessions[fmt.Sprintf("%s-%07d-%x", prefix, i, i*2654435761)] = info
		tr.dirty[info.month()] = true
	}
}

// countingState records which keys each save writes.
type countingState struct {
	memState
	writes map[string]int
}

func (c *countingState) WriteInstanceState(ctx context.Context, key string, value []byte) error {
	c.writes[key]++
	return c.memState.WriteInstanceState(ctx, key, value)
}

func TestTrackerSavesMonthsIntoSlots(t *testing.T) {
	defer func(old int) { maxSlotBytes = old }(maxSlotBytes)
	maxSlotBytes = 16 << 10 // small slots, so a month needs several
	state := &countingState{memState: memState{}, writes: map[string]int{}}
	tr := NewTracker(&fakeSessions{pages: [][]silo.LiveSession{{}}}, state, quiet)
	sep := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	oct := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	bulkSessions(tr, sep, 3000, "sep")
	bulkSessions(tr, oct, 3000, "oct")
	tr.save(t.Context())
	if len(tr.index.Months) != 2 || len(tr.index.Months["2026-09"]) < 2 {
		t.Fatalf("index %+v", tr.index.Months)
	}
	for k, v := range state.memState {
		if len(v) > maxSlotBytes && k != stateKeyIndex {
			t.Fatalf("%s is %d bytes", k, len(v))
		}
	}

	// A change in October rewrites October's slots only, into fresh slots.
	octSlots := append([]int(nil), tr.index.Months["2026-10"]...)
	sepSlots := append([]int(nil), tr.index.Months["2026-09"]...)
	state.writes = map[string]int{}
	bulkSessions(tr, oct, 10, "late")
	tr.save(t.Context())
	for _, s := range sepSlots {
		if state.writes[slotKey(s)] != 0 {
			t.Fatalf("September slot %d was rewritten", s)
		}
	}
	for _, s := range tr.index.Months["2026-10"] {
		for _, old := range octSlots {
			if s == old {
				t.Fatalf("October reused slot %d before its new copy was indexed", s)
			}
		}
	}

	restored := NewTracker(nil, state, quiet)
	if !restored.load(t.Context()) || len(restored.sessions) != len(tr.sessions) {
		t.Fatalf("restored %d sessions, want %d", len(restored.sessions), len(tr.sessions))
	}
	if got, ok := restored.Lookup("sep-0000007-" + fmt.Sprintf("%x", 7*2654435761)); !ok || got.Client != "Some long client name 1.2.3" {
		t.Fatalf("lookup after restore: %+v %v", got, ok)
	}
}

func TestTrackerMigratesTheOldLayout(t *testing.T) {
	state := memState{}
	old := map[string]ClientInfo{"a": {Client: "Infuse", Location: "local", Method: "remux", Seen: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).Unix()},
		"b": {Client: "Silo Web", Location: "remote", Method: "transcode", Seen: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Unix()}}
	raw, _ := json.Marshal(old)
	state[stateKeyCount] = []byte("1")
	state[slotKey(0)] = raw

	tr := NewTracker(nil, state, quiet)
	if !tr.load(t.Context()) || len(tr.sessions) != 2 || tr.migrating != 1 {
		t.Fatalf("old layout: %d sessions, migrating %d", len(tr.sessions), tr.migrating)
	}
	tr.save(t.Context())
	if string(state[stateKeyCount]) != "0" || len(tr.index.Months) != 2 {
		t.Fatalf("after migration: count %q index %+v", state[stateKeyCount], tr.index.Months)
	}
	for _, slots := range tr.index.Months {
		for _, s := range slots {
			if s == 0 {
				t.Fatal("the old chunk's key was overwritten before the new index was saved")
			}
		}
	}
	again := NewTracker(nil, state, quiet)
	if !again.load(t.Context()) || len(again.sessions) != 2 {
		t.Fatalf("reloaded %d sessions", len(again.sessions))
	}
	if got, _ := again.Lookup("b"); got.Method != "transcode" || got.month() != "2026-10" {
		t.Fatalf("b = %+v", got)
	}
}

func TestTrackerDropsOldestMonthsWhenFull(t *testing.T) {
	defer func(old int) { maxSlotBytes = old }(maxSlotBytes)
	maxSlotBytes = 2 << 10
	state := memState{}
	tr := NewTracker(nil, state, quiet)
	// Enough months of sessions to need more than the slot pool.
	start := time.Date(2020, 1, 15, 0, 0, 0, 0, time.UTC)
	for m := range 90 {
		bulkSessions(tr, start.AddDate(0, m, 0), 300, fmt.Sprintf("m%02d", m))
	}
	tr.save(t.Context())
	t.Logf("%d months kept of 90", len(tr.index.Months))
	slots := 0
	for _, s := range tr.index.Months {
		slots += len(s)
	}
	if slots > maxSlots || slots == 0 {
		t.Fatalf("%d slots used", slots)
	}
	if _, ok := tr.index.Months["2020-01"]; ok {
		t.Fatal("the oldest month was kept")
	}
	if _, ok := tr.index.Months[start.AddDate(0, 89, 0).Format("2006-01")]; !ok {
		t.Fatal("the newest month was dropped")
	}
	keys := 0
	for k := range state {
		if strings.HasPrefix(k, stateKeyPrefix) {
			keys++
		}
	}
	if keys > maxSlots+2 {
		t.Fatalf("%d keys written", keys)
	}
	for _, info := range tr.sessions {
		if info.month() == "2020-01" {
			t.Fatal("dropped sessions are still in memory")
		}
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
	if err := json.Unmarshal(gunzip(t, resp.GetBody()), &c); err != nil {
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

func TestEncodeChunksSplitsWhenCompressedChunksAreTooBig(t *testing.T) {
	defer func(old int) { maxChunkBytes = old }(maxChunkBytes)
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	plays := make([]Play, 30000)
	for i := range plays {
		// Distinct titles so the chunks don't compress to nothing.
		p := PlayFrom(entry(fmt.Sprint(i), base.Add(-time.Duration(i)*time.Minute), 60, ptr(42.0)))
		p.Title = fmt.Sprintf("Title %x %d", i*7919, i)
		plays[i] = p
	}
	whole, err := encodeChunks("s", plays, Status{}, features{}, nil, base)
	if err != nil || len(whole) != 1 {
		t.Fatalf("one chunk expected: %d %v", len(whole), err)
	}
	maxChunkBytes = len(whole[0]) / 3
	split, err := encodeChunks("s", plays, Status{}, features{}, nil, base)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for i, b := range split {
		if len(b) > maxChunkBytes {
			t.Fatalf("chunk %d still %d bytes, limit %d", i, len(b), maxChunkBytes)
		}
		var c chunk
		_ = json.Unmarshal(gunzip(t, b), &c)
		if c.Count != len(split) || c.Index != i {
			t.Fatalf("chunk %d header %d/%d", i, c.Index, c.Count)
		}
		total += len(c.Plays)
	}
	if len(split) < 3 || total != len(plays) {
		t.Fatalf("%d chunks with %d plays", len(split), total)
	}
}
