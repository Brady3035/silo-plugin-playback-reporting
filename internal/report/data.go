package report

import (
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"
)

// The page loads every play once, in chunks, and computes all reports in the
// browser. The host proxy authorizes plugin requests with a launch cookie that
// lasts five minutes and is not renewed, so a page that kept asking the plugin
// for each report would stop working five minutes after it opened. The host
// also reads at most 4 MiB per plugin response, which bounds a chunk.
const (
	playsPerChunk = 25000
	snapshotTTL   = 10 * time.Minute
	keptSnapshots = 4
)

// Row columns, in order, as the page reads them (app.js mirrors this list).
var rowColumns = []string{
	"ended", "started", "user", "profile", "title", "series", "season", "episode",
	"type", "method", "watched", "duration", "flags", "client", "location", "live_method",
}

// Row flags.
const (
	flagCompleted = 1 << iota
	flagEstimated
)

// chunk is one /data response.
type chunk struct {
	Snapshot    string    `json:"snapshot"`
	Index       int       `json:"chunk"`
	Count       int       `json:"chunks"`
	GeneratedAt time.Time `json:"generated_at"`
	Status      Status    `json:"status"`
	Features    features  `json:"features"`
	Columns     []string  `json:"columns"`
	Dict        dict      `json:"dict"`
	Plays       [][]any   `json:"plays"`
}

type features struct {
	Clients      bool   `json:"clients"`
	ClientsError string `json:"clients_error,omitempty"`
	// Report settings, so the page can say what it leaves out.
	MinSeconds  int               `json:"min_seconds"`
	IgnoreRules int               `json:"ignore_rules"`
	Hidden      int               `json:"hidden"`
	Colors      map[string]string `json:"colors"`
	Palette     []string          `json:"palette"`
}

// dict holds each chunk's repeated strings once; rows refer to them by index.
type dict struct {
	Users     [][2]string `json:"users"` // [id, name]
	Profiles  []string    `json:"profiles"`
	Titles    [][2]string `json:"titles"` // [item id, title]
	Series    []string    `json:"series"`
	Types     []string    `json:"types"`
	Methods   []string    `json:"methods"`
	Clients   []string    `json:"clients"`
	Locations []string    `json:"locations"`
}

type interner struct {
	index map[string]int
}

func (in *interner) add(list *[]string, value string) int {
	if i, ok := in.index[value]; ok {
		return i
	}
	in.index[value] = len(*list)
	*list = append(*list, value)
	return in.index[value]
}

// encodeChunks splits plays into encoded /data responses. clients may be nil.
func encodeChunks(id string, plays []Play, status Status, feat features, clients *Tracker, now time.Time) ([][]byte, error) {
	n := max(1, int(math.Ceil(float64(len(plays))/playsPerChunk)))
	out := make([][]byte, 0, n)
	for i := range n {
		part := plays[min(i*playsPerChunk, len(plays)):min((i+1)*playsPerChunk, len(plays))]
		c := chunk{Snapshot: id, Index: i, Count: n, GeneratedAt: now, Status: status, Features: feat,
			Columns: rowColumns, Plays: make([][]any, 0, len(part))}
		users := map[string]int{}
		titles := map[[2]string]int{}
		profiles := &interner{map[string]int{}}
		series := &interner{map[string]int{}}
		types := &interner{map[string]int{}}
		methods := &interner{map[string]int{}}
		clientNames := &interner{map[string]int{}}
		locations := &interner{map[string]int{}}
		for _, p := range part {
			u, ok := users[p.UserID]
			if !ok {
				u = len(c.Dict.Users)
				users[p.UserID] = u
				c.Dict.Users = append(c.Dict.Users, [2]string{p.UserID, p.User})
			}
			tkey := [2]string{p.ItemID, p.Title}
			t, ok := titles[tkey]
			if !ok {
				t = len(c.Dict.Titles)
				titles[tkey] = t
				c.Dict.Titles = append(c.Dict.Titles, tkey)
			}
			flags := 0
			if p.Completed {
				flags |= flagCompleted
			}
			if p.Estimated {
				flags |= flagEstimated
			}
			client, location, liveMethod := -1, -1, -1
			if info, ok := clients.Lookup(p.SessionID); ok {
				if info.Client != "" {
					client = clientNames.add(&c.Dict.Clients, info.Client)
				}
				if info.Location != "" {
					location = locations.add(&c.Dict.Locations, info.Location)
				}
				if info.Method != "" {
					liveMethod = methods.add(&c.Dict.Methods, info.Method)
				}
			}
			c.Plays = append(c.Plays, []any{
				p.Ended.Unix(), p.Started.Unix(), u, profiles.add(&c.Dict.Profiles, p.Profile), t,
				series.add(&c.Dict.Series, p.Series), p.Season, p.Episode,
				types.add(&c.Dict.Types, p.Type), methods.add(&c.Dict.Methods, p.Method),
				int64(math.Round(p.Watched)), int64(math.Round(p.Duration)), flags, client, location, liveMethod,
			})
		}
		normalize(&c.Dict)
		b, err := json.Marshal(c)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// normalize replaces nil slices so the page always gets arrays.
func normalize(d *dict) {
	for _, s := range []*[]string{&d.Profiles, &d.Series, &d.Types, &d.Methods, &d.Clients, &d.Locations} {
		if *s == nil {
			*s = []string{}
		}
	}
	if d.Users == nil {
		d.Users = [][2]string{}
	}
	if d.Titles == nil {
		d.Titles = [][2]string{}
	}
}

// snapshots keeps encoded datasets so a page reads all its chunks from one
// consistent snapshot, even when a sync lands while it loads.
type snapshots struct {
	mu     sync.Mutex
	byID   map[string]*snapshot
	latest *snapshot
}

type snapshot struct {
	id         string
	generation uint64
	clientsKey string
	created    time.Time
	chunks     [][]byte
}

// current returns a snapshot of the store, reusing the latest one while the
// store and the client tracking it was built from are unchanged.
func (s *snapshots) current(store *Store, status Status, feat features, clients *Tracker, opts *Options, now time.Time) (*snapshot, error) {
	plays, generation := store.Snapshot()
	clientsKey := fmt.Sprintf("%t/%d/%s", feat.Clients, clients.version(), opts.key())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(now)
	if l := s.latest; l != nil && l.generation == generation && l.clientsKey == clientsKey &&
		now.Sub(l.created) < time.Minute {
		return l, nil
	}
	id := fmt.Sprintf("%d-%d", generation, now.UnixNano())
	plays, feat.Hidden = opts.apply(plays)
	if opts != nil {
		feat.MinSeconds, feat.IgnoreRules = opts.MinSeconds, len(opts.Ignore)
		feat.Colors, feat.Palette = opts.Colors, opts.Palette
	}
	if feat.Colors == nil {
		feat.Colors = map[string]string{}
	}
	if feat.Palette == nil {
		feat.Palette = []string{}
	}
	chunks, err := encodeChunks(id, plays, status, feat, clients, now)
	if err != nil {
		return nil, err
	}
	snap := &snapshot{id: id, generation: generation, clientsKey: clientsKey, created: now, chunks: chunks}
	if s.byID == nil {
		s.byID = map[string]*snapshot{}
	}
	s.byID[id] = snap
	s.latest = snap
	for len(s.byID) > keptSnapshots {
		var oldest *snapshot
		for _, v := range s.byID {
			if oldest == nil || v.created.Before(oldest.created) {
				oldest = v
			}
		}
		delete(s.byID, oldest.id)
	}
	return snap, nil
}

func (s *snapshots) get(id string, now time.Time) *snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(now)
	return s.byID[id]
}

func (s *snapshots) expireLocked(now time.Time) {
	for id, v := range s.byID {
		if now.Sub(v.created) > snapshotTTL {
			delete(s.byID, id)
			if s.latest == v {
				s.latest = nil
			}
		}
	}
}

// version changes whenever the tracker learns something; zero without one.
func (t *Tracker) version() uint64 {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.changes
}
