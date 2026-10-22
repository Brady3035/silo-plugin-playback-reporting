// Package report keeps Silo's playback history in memory and serves it to the
// Playback Reporting page.
package report

import (
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Brady3035/silo-plugin-playback-reporting/internal/silo"
)

// Play is one finalized play as the reports use it.
type Play struct {
	SessionID string
	UserID    string
	User      string
	Profile   string
	ItemID    string
	Title     string
	Series    string
	Season    int // 0 when not an episode
	Episode   int
	Type      string // movie, episode, ... as Silo reports it
	Method    string // direct, remux, transcode (Silo's play_method)
	Started   time.Time
	Ended     time.Time
	// Watched is the seconds actually watched: Silo's measurement, or for a
	// play recorded before Silo measured watch time, its end position capped
	// at the session's length (Estimated).
	Watched   float64
	Estimated bool
	Duration  float64 // media length; 0 when unknown
	Completed bool
}

// WatchedSeconds is how long entry was watched, and whether that is an
// estimate. Silo's played_seconds is the measurement; without it the end
// position overstates a resumed play, so it is capped at the wall-clock
// length of the session, as Silo's own totals do.
func WatchedSeconds(entry silo.HistoryEntry) (seconds float64, estimated bool) {
	if entry.PlayedSeconds != nil {
		return math.Max(*entry.PlayedSeconds, 0), false
	}
	elapsed := math.Max(entry.EndedAt.Sub(entry.StartedAt).Seconds(), 0)
	return math.Min(math.Max(entry.WatchedSeconds, 0), elapsed), true
}

// PlayFrom converts a history entry.
func PlayFrom(entry silo.HistoryEntry) Play {
	watched, estimated := WatchedSeconds(entry)
	p := Play{
		SessionID: entry.SessionID,
		UserID:    entry.UserID,
		User:      entry.Username,
		Profile:   entry.ProfileName,
		ItemID:    entry.MediaItemID,
		Title:     entry.MediaTitle,
		Series:    entry.SeriesTitle,
		Type:      entry.MediaType,
		Method:    entry.PlayMethod,
		Started:   entry.StartedAt,
		Ended:     entry.EndedAt,
		Watched:   watched,
		Estimated: estimated,
		Completed: entry.Completed,
	}
	if p.User == "" {
		p.User = "Deleted account " + entry.UserID
	}
	if p.Title == "" {
		p.Title = "Removed item"
	}
	if entry.DurationSeconds != nil {
		p.Duration = math.Max(*entry.DurationSeconds, 0)
	}
	if entry.SeasonNumber != nil {
		p.Season = *entry.SeasonNumber
	}
	if entry.EpisodeNumber != nil {
		p.Episode = *entry.EpisodeNumber
	}
	return p
}

// Store holds every play by session ID, so a play Silo finalizes again
// replaces its earlier copy.
type Store struct {
	mu    sync.RWMutex
	plays map[string]Play
	// generation advances on every change, so cached snapshots can tell
	// they are stale.
	generation uint64
}

// NewStore returns an empty store.
func NewStore() *Store { return &Store{plays: map[string]Play{}} }

// Upsert adds or replaces plays and returns how many changed.
func (s *Store) Upsert(plays []Play) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := 0
	for _, p := range plays {
		if old, ok := s.plays[p.SessionID]; ok && old == p {
			continue
		}
		s.plays[p.SessionID] = p
		changed++
	}
	if changed > 0 {
		s.generation++
	}
	return changed
}

// Replace swaps in a complete history, dropping plays Silo no longer has.
func (s *Store) Replace(plays []Play) {
	next := make(map[string]Play, len(plays))
	for _, p := range plays {
		next[p.SessionID] = p
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.plays = next
	s.generation++
}

// Latest is the newest end time held, zero when empty.
func (s *Store) Latest() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var latest time.Time
	for _, p := range s.plays {
		if p.Ended.After(latest) {
			latest = p.Ended
		}
	}
	return latest
}

// Snapshot returns every play, newest ended first, with the store's
// generation.
func (s *Store) Snapshot() ([]Play, uint64) {
	s.mu.RLock()
	out := make([]Play, 0, len(s.plays))
	for _, p := range s.plays {
		out = append(out, p)
	}
	generation := s.generation
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Ended.Equal(out[j].Ended) {
			return out[i].Ended.After(out[j].Ended)
		}
		return strings.Compare(out[i].SessionID, out[j].SessionID) > 0
	})
	return out, generation
}

// Len is the number of plays held.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.plays)
}
