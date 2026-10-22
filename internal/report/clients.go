package report

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Brady3035/silo-plugin-playback-reporting/internal/silo"
)

// Client tracking (the opt-in "Track clients" setting). Silo's playback
// history does not record which app played or from where, but its live
// sessions do, so the tracker polls them and remembers each session's client
// until the play shows up in history. The memory lives in the host's
// per-installation state store, which caps keys at 256 KiB and 256 keys.
const (
	clientPollEvery    = 15 * time.Second
	clientSaveEvery    = 5 * time.Minute
	maxTrackedSessions = 40000
	stateChunkBytes    = 200 << 10
	stateKeyCount      = "clients/count"
	stateKeyPrefix     = "clients/"
	maxStateChunks     = 200
)

// ClientInfo is what the tracker learned about one session.
type ClientInfo struct {
	Client   string `json:"c,omitempty"`
	Location string `json:"l,omitempty"` // local or remote
	// Method is the live session's effective play method, which tells a
	// direct stream (video copied, audio converted) from a remux.
	Method string `json:"m,omitempty"`
	Seen   int64  `json:"s"` // unix seconds the session was last observed
}

// SessionReader is the part of silo.Client the tracker uses.
type SessionReader interface {
	SessionsPage(ctx context.Context, cursor string) ([]silo.LiveSession, string, error)
}

// StateStore is the host's per-installation state (runtimehost.Client).
type StateStore interface {
	ReadInstanceState(ctx context.Context, key string) ([]byte, bool, error)
	WriteInstanceState(ctx context.Context, key string, value []byte) error
}

// Tracker remembers live sessions' clients.
type Tracker struct {
	reader SessionReader
	state  StateStore
	logger *slog.Logger
	now    func() time.Time

	mu       sync.RWMutex
	sessions map[string]ClientInfo
	dirty    bool
	lastErr  string
	// changes counts updates, so cached page data can tell it is stale.
	changes uint64
}

// NewTracker returns a tracker; Run starts it.
func NewTracker(reader SessionReader, state StateStore, logger *slog.Logger) *Tracker {
	return &Tracker{reader: reader, state: state, logger: logger, now: time.Now, sessions: map[string]ClientInfo{}}
}

// Lookup returns what is known about a session.
func (t *Tracker) Lookup(sessionID string) (ClientInfo, bool) {
	if t == nil {
		return ClientInfo{}, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	info, ok := t.sessions[sessionID]
	return info, ok
}

// LastError is the last poll failure, empty when the last poll worked.
func (t *Tracker) LastError() string {
	if t == nil {
		return ""
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.lastErr
}

// Run loads the saved sessions, then polls until ctx ends, saving on the way
// out.
func (t *Tracker) Run(ctx context.Context) {
	// The host connection can arrive just after the plugin starts; keep
	// trying to read the saved sessions for a while rather than starting
	// empty and later overwriting them.
	for attempt := 0; !t.load(ctx) && attempt < 24; attempt++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	poll := time.NewTicker(clientPollEvery)
	save := time.NewTicker(clientSaveEvery)
	defer poll.Stop()
	defer save.Stop()
	for {
		t.poll(ctx)
		select {
		case <-ctx.Done():
			saveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.save(saveCtx)
			cancel()
			return
		case <-save.C:
			t.save(ctx)
		case <-poll.C:
		}
	}
}

func (t *Tracker) poll(ctx context.Context) {
	now := t.now().Unix()
	seen := map[string]ClientInfo{}
	cursor := ""
	for range 100 {
		items, next, err := t.reader.SessionsPage(ctx, cursor)
		if err != nil {
			t.mu.Lock()
			t.lastErr = silo.Explain(err)
			t.mu.Unlock()
			return
		}
		for _, s := range items {
			if s.SessionID == "" {
				continue
			}
			client := strings.TrimSpace(s.ClientName)
			if client == "" {
				client = strings.TrimSpace(s.ClientLabel)
			}
			seen[s.SessionID] = ClientInfo{Client: client, Location: s.StreamLocation, Method: s.EffectivePlayMethod, Seen: now}
		}
		if next == "" || next == cursor {
			break
		}
		cursor = next
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastErr = ""
	for id, info := range seen {
		old, ok := t.sessions[id]
		// Keep the first non-empty client and the most transcoded method a
		// session reached, so a stream that fell back to transcoding counts
		// as a transcode.
		if ok {
			if info.Client == "" {
				info.Client = old.Client
			}
			if info.Location == "" {
				info.Location = old.Location
			}
			if methodRank(old.Method) > methodRank(info.Method) {
				info.Method = old.Method
			}
		}
		if !ok || old.Client != info.Client || old.Location != info.Location || old.Method != info.Method {
			t.changes++
		}
		t.sessions[id] = info
		t.dirty = true
	}
	t.trimLocked()
}

func methodRank(method string) int {
	switch method {
	case "transcode":
		return 3
	case "direct_stream":
		return 2
	case "remux":
		return 1
	}
	return 0
}

func (t *Tracker) trimLocked() {
	if len(t.sessions) <= maxTrackedSessions {
		return
	}
	type aged struct {
		id   string
		seen int64
	}
	all := make([]aged, 0, len(t.sessions))
	for id, info := range t.sessions {
		all = append(all, aged{id, info.Seen})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].seen < all[j].seen })
	for _, a := range all[:len(all)-maxTrackedSessions] {
		delete(t.sessions, a.id)
	}
}

// load reads the saved sessions and reports whether the state store
// answered. Missing or undecodable state starts empty.
func (t *Tracker) load(ctx context.Context) bool {
	if t.state == nil {
		return true
	}
	raw, ok, err := t.state.ReadInstanceState(ctx, stateKeyCount)
	if err != nil {
		return false
	}
	if !ok {
		return true
	}
	var count int
	if json.Unmarshal(raw, &count) != nil || count < 0 || count > maxStateChunks {
		return true
	}
	loaded := map[string]ClientInfo{}
	for i := range count {
		chunk, ok, err := t.state.ReadInstanceState(ctx, fmt.Sprintf("%s%d", stateKeyPrefix, i))
		if err != nil || !ok {
			continue
		}
		var part map[string]ClientInfo
		if json.Unmarshal(chunk, &part) == nil {
			for id, info := range part {
				loaded[id] = info
			}
		}
	}
	t.mu.Lock()
	for id, info := range loaded {
		if _, live := t.sessions[id]; !live {
			t.sessions[id] = info
		}
	}
	t.changes++
	t.trimLocked()
	t.mu.Unlock()
	return true
}

// save writes the sessions in chunks under the state store's value limit.
func (t *Tracker) save(ctx context.Context) {
	if t.state == nil {
		return
	}
	t.mu.Lock()
	if !t.dirty {
		t.mu.Unlock()
		return
	}
	ids := make([]string, 0, len(t.sessions))
	for id := range t.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var chunks [][]byte
	part := map[string]ClientInfo{}
	size := 2
	flush := func() {
		if len(part) == 0 {
			return
		}
		b, _ := json.Marshal(part)
		chunks = append(chunks, b)
		part, size = map[string]ClientInfo{}, 2
	}
	for _, id := range ids {
		info := t.sessions[id]
		entry, _ := json.Marshal(info)
		n := len(id) + len(entry) + 4
		if size+n > stateChunkBytes {
			flush()
		}
		part[id] = info
		size += n
	}
	flush()
	t.dirty = false
	t.mu.Unlock()

	if len(chunks) > maxStateChunks {
		chunks = chunks[:maxStateChunks]
	}
	for i, chunk := range chunks {
		if err := t.state.WriteInstanceState(ctx, fmt.Sprintf("%s%d", stateKeyPrefix, i), chunk); err != nil {
			t.logger.Warn("saving tracked clients failed", "error", err)
			t.markDirty()
			return
		}
	}
	count, _ := json.Marshal(len(chunks))
	if err := t.state.WriteInstanceState(ctx, stateKeyCount, count); err != nil {
		t.logger.Warn("saving tracked clients failed", "error", err)
		t.markDirty()
	}
}

func (t *Tracker) markDirty() {
	t.mu.Lock()
	t.dirty = true
	t.mu.Unlock()
}
