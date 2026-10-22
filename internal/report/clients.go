package report

import (
	"bytes"
	"compress/gzip"
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
// for good: history never gains it, so the report joins on these notes.
//
// The notes live in the host's per-installation state store: at most 256
// keys of 256 KiB, and keys can't be deleted. Sessions are grouped by the
// month they were first seen, each month gzipped into one or more of a fixed
// pool of slot keys (clients/0 … clients/239), and clients/index maps months
// to slots. A save rewrites only the months that changed (normally the
// current one) into free slots, then the index, so a crash mid-save leaves
// the previous copy intact. When the pool is full, the oldest months go.
const (
	clientPollEvery = 15 * time.Second
	clientSaveEvery = 5 * time.Minute

	stateKeyIndex  = "clients/index"
	stateKeyPrefix = "clients/" // + slot number
	// stateKeyCount is the old layout's chunk count (plain JSON chunks in
	// clients/0…n-1); it's read once to migrate and then set to 0.
	stateKeyCount = "clients/count"
	// maxSlots leaves room in the host's 256 keys for the index, the old
	// count key and the page settings.
	maxSlots = 240
)

// maxSlotBytes is the most a gzipped slot may hold (a var for tests).
var maxSlotBytes = 250 << 10

// ClientInfo is what the tracker learned about one session.
type ClientInfo struct {
	Client   string `json:"c,omitempty"`
	Location string `json:"l,omitempty"` // local or remote
	// Method is the live session's effective play method, which tells a
	// direct stream (video copied, audio converted) from a remux.
	Method string `json:"m,omitempty"`
	Seen   int64  `json:"s"`           // unix seconds the session was last observed
	First  int64  `json:"f,omitempty"` // unix seconds it was first observed; picks its month
}

// month is the UTC month a session belongs to, from when it was first seen.
func (c ClientInfo) month() string {
	t := c.First
	if t == 0 {
		t = c.Seen
	}
	return time.Unix(t, 0).UTC().Format("2006-01")
}

// slotIndex is clients/index: which slots hold each month.
type slotIndex struct {
	Version int              `json:"v"`
	Months  map[string][]int `json:"months"`
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
	// dirty lists months changed since the last save; index is what the
	// state store holds now.
	dirty map[string]bool
	index slotIndex
	// migrating is the old layout's chunk count until the first save.
	migrating int
	lastErr   string
	// strs shares the few distinct client, location and method strings
	// between sessions, which keeps millions of sessions small in memory.
	strs map[string]string
	// changes counts updates, so cached page data can tell it is stale.
	changes uint64
}

// NewTracker returns a tracker; Run starts it.
func NewTracker(reader SessionReader, state StateStore, logger *slog.Logger) *Tracker {
	return &Tracker{reader: reader, state: state, logger: logger, now: time.Now, sessions: map[string]ClientInfo{},
		dirty: map[string]bool{}, index: slotIndex{Version: 2, Months: map[string][]int{}}, strs: map[string]string{}}
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
			seen[s.SessionID] = ClientInfo{Client: client, Location: s.StreamLocation, Method: s.EffectivePlayMethod, Seen: now, First: now}
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
			info.First = old.First
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
		info = t.intern(info)
		if !ok || old.Client != info.Client || old.Location != info.Location || old.Method != info.Method {
			// Only real changes need saving; the last-seen time alone doesn't.
			t.changes++
			t.dirty[info.month()] = true
		}
		t.sessions[id] = info
	}
}

func (t *Tracker) intern(info ClientInfo) ClientInfo {
	for _, s := range []*string{&info.Client, &info.Location, &info.Method} {
		if v, ok := t.strs[*s]; ok {
			*s = v
		} else {
			t.strs[*s] = *s
		}
	}
	return info
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

// load reads the saved sessions and reports whether the state store
// answered. Missing or undecodable state starts empty; a slot that can't be
// read loses only its own sessions.
func (t *Tracker) load(ctx context.Context) bool {
	if t.state == nil {
		return true
	}
	raw, ok, err := t.state.ReadInstanceState(ctx, stateKeyIndex)
	if err != nil {
		return false
	}
	if !ok || len(raw) == 0 {
		return t.loadOld(ctx)
	}
	var index slotIndex
	if json.Unmarshal(raw, &index) != nil || index.Months == nil {
		return true
	}
	loaded := map[string]ClientInfo{}
	for _, slots := range index.Months {
		for _, slot := range slots {
			chunk, ok, err := t.state.ReadInstanceState(ctx, slotKey(slot))
			if err != nil {
				return false
			}
			if ok {
				_ = decodeSlot(chunk, loaded)
			}
		}
	}
	t.mu.Lock()
	t.index = index
	t.mergeLocked(loaded)
	t.mu.Unlock()
	return true
}

// loadOld reads the first layout (clients/count plain JSON chunks) and marks
// every month dirty so the next save converts it.
func (t *Tracker) loadOld(ctx context.Context) bool {
	raw, ok, err := t.state.ReadInstanceState(ctx, stateKeyCount)
	if err != nil {
		return false
	}
	var count int
	if !ok || json.Unmarshal(raw, &count) != nil || count <= 0 || count > maxSlots {
		return true
	}
	loaded := map[string]ClientInfo{}
	for i := range count {
		chunk, ok, err := t.state.ReadInstanceState(ctx, slotKey(i))
		if err != nil {
			return false
		}
		var part map[string]ClientInfo
		if ok && json.Unmarshal(chunk, &part) == nil {
			for id, info := range part {
				loaded[id] = info
			}
		}
	}
	t.mu.Lock()
	t.mergeLocked(loaded)
	for _, info := range loaded {
		t.dirty[info.month()] = true
	}
	// Those keys now count as slots in use until the new layout's first
	// save takes over the pool.
	t.migrating = count
	t.mu.Unlock()
	return true
}

func (t *Tracker) mergeLocked(loaded map[string]ClientInfo) {
	for id, info := range loaded {
		if _, live := t.sessions[id]; !live {
			if info.First == 0 {
				info.First = info.Seen
			}
			t.sessions[id] = t.intern(info)
		}
	}
	t.changes++
}

func slotKey(slot int) string { return fmt.Sprintf("%s%d", stateKeyPrefix, slot) }

// decodeSlot adds one gzipped slot's sessions to into.
func decodeSlot(raw []byte, into map[string]ClientInfo) error {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	var part map[string]ClientInfo
	if err := json.NewDecoder(zr).Decode(&part); err != nil {
		return err
	}
	for id, info := range part {
		into[id] = info
	}
	return nil
}

// encodeMonth gzips one month's sessions into as few slots as fit. It sizes
// the pieces from how well the whole month compresses, then shrinks them by
// a tenth until every piece fits.
func encodeMonth(sessions map[string]ClientInfo, ids []string) ([][]byte, error) {
	encode := func(part []string) ([]byte, error) {
		m := make(map[string]ClientInfo, len(part))
		for _, id := range part {
			m[id] = sessions[id]
		}
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if err := json.NewEncoder(zw).Encode(m); err != nil {
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	whole, err := encode(ids)
	if err != nil || len(whole) <= maxSlotBytes {
		return [][]byte{whole}, err
	}
	per := max(1, int(float64(len(ids))*float64(maxSlotBytes)*0.95/float64(len(whole))))
	for {
		var out [][]byte
		fits := true
		for i := 0; i < len(ids) && fits; i += per {
			part, err := encode(ids[i:min(i+per, len(ids))])
			if err != nil {
				return nil, err
			}
			fits = len(part) <= maxSlotBytes || per == 1
			out = append(out, part)
		}
		if fits {
			return out, nil
		}
		per = max(1, per*9/10)
	}
}

// save writes the changed months and then the index. Each month goes into
// free slots first, so the index only ever names complete copies; if the
// pool runs short, the oldest months are dropped to make room.
func (t *Tracker) save(ctx context.Context) {
	if t.state == nil {
		return
	}
	t.mu.Lock()
	if len(t.dirty) == 0 {
		t.mu.Unlock()
		return
	}
	byMonth := map[string][]string{}
	for id, info := range t.sessions {
		byMonth[info.month()] = append(byMonth[info.month()], id)
	}
	months := make([]string, 0, len(t.dirty))
	for m := range t.dirty {
		months = append(months, m)
	}
	sort.Strings(months)
	encoded := map[string][][]byte{}
	for _, m := range months {
		ids := byMonth[m]
		sort.Strings(ids)
		parts, err := encodeMonth(t.sessions, ids)
		if err != nil {
			t.mu.Unlock()
			t.logger.Warn("encoding tracked clients failed", "error", err)
			return
		}
		encoded[m] = parts
	}
	index := slotIndex{Version: 2, Months: map[string][]int{}}
	for m, slots := range t.index.Months {
		if _, changed := encoded[m]; !changed {
			index.Months[m] = slots
		}
	}
	migrating := t.migrating
	t.dirty = map[string]bool{}
	t.mu.Unlock()

	// Slots the old index uses stay untouched until the new index is saved.
	busy := map[int]bool{}
	for _, slots := range t.index.Months {
		for _, s := range slots {
			busy[s] = true
		}
	}
	for i := range migrating {
		busy[i] = true
	}
	kept := func() int {
		n := 0
		for _, slots := range index.Months {
			n += len(slots)
		}
		return n
	}
	need := 0
	for _, parts := range encoded {
		need += len(parts)
	}
	dropped := []string{}
	for kept()+need > maxSlots || freeSlots(busy, index) < need {
		oldest := oldestMonth(index, encoded)
		if oldest == "" {
			break
		}
		if parts, ok := encoded[oldest]; ok {
			need -= len(parts)
			delete(encoded, oldest)
		} else {
			for _, s := range index.Months[oldest] {
				delete(busy, s)
			}
			delete(index.Months, oldest)
		}
		dropped = append(dropped, oldest)
	}
	if len(dropped) > 0 {
		t.logger.Warn("client tracking storage is full; dropped the oldest months", "months", dropped)
		t.mu.Lock()
		gone := map[string]bool{}
		for _, m := range dropped {
			gone[m] = true
		}
		for id, info := range t.sessions {
			if gone[info.month()] {
				delete(t.sessions, id)
			}
		}
		t.changes++
		t.mu.Unlock()
	}

	used := map[int]bool{}
	for _, slots := range index.Months {
		for _, s := range slots {
			used[s] = true
		}
	}
	next := 0
	newMonths := make([]string, 0, len(encoded))
	for m := range encoded {
		newMonths = append(newMonths, m)
	}
	sort.Strings(newMonths)
	for _, m := range newMonths {
		for _, part := range encoded[m] {
			for busy[next] || used[next] {
				next++
			}
			if err := t.state.WriteInstanceState(ctx, slotKey(next), part); err != nil {
				t.saveFailed(err, newMonths)
				return
			}
			used[next] = true
			index.Months[m] = append(index.Months[m], next)
		}
	}
	raw, _ := json.Marshal(index)
	if err := t.state.WriteInstanceState(ctx, stateKeyIndex, raw); err != nil {
		t.saveFailed(err, newMonths)
		return
	}
	if migrating > 0 {
		// The old layout's chunks now belong to the slot pool.
		_ = t.state.WriteInstanceState(ctx, stateKeyCount, []byte("0"))
	}
	t.mu.Lock()
	t.index = index
	t.migrating = 0
	t.mu.Unlock()
}

// freeSlots counts slots neither in use by the saved index (busy) nor kept
// by the new one.
func freeSlots(busy map[int]bool, index slotIndex) int {
	used := map[int]bool{}
	for s := range busy {
		used[s] = true
	}
	for _, slots := range index.Months {
		for _, s := range slots {
			used[s] = true
		}
	}
	return maxSlots - len(used)
}

func oldestMonth(index slotIndex, encoded map[string][][]byte) string {
	oldest := ""
	for m := range index.Months {
		if oldest == "" || m < oldest {
			oldest = m
		}
	}
	for m := range encoded {
		if oldest == "" || m < oldest {
			oldest = m
		}
	}
	return oldest
}

func (t *Tracker) saveFailed(err error, months []string) {
	t.logger.Warn("saving tracked clients failed", "error", err)
	t.mu.Lock()
	for _, m := range months {
		t.dirty[m] = true
	}
	t.mu.Unlock()
}
