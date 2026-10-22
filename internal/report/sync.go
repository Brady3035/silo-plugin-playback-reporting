package report

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Brady3035/silo-plugin-playback-reporting/internal/silo"
)

// Sync timing. A play is written when it stops, and can be written again
// (another replica finalizing its copy), so incremental reads overlap the
// newest play held by incrementalOverlap. A full read now and then drops
// plays Silo deleted (an account removed) and picks up renamed titles.
const (
	incrementalEvery   = 2 * time.Minute
	fullEvery          = 6 * time.Hour
	incrementalOverlap = 30 * time.Minute
	retryAfterError    = 1 * time.Minute
	// maxPages bounds one read so a broken cursor cannot loop forever:
	// 200 plays a page is 10 million plays.
	maxPages = 50000
)

// HistoryReader is the part of silo.Client the syncer uses.
type HistoryReader interface {
	HistoryPage(ctx context.Context, endedAfter time.Time, cursor string) ([]silo.HistoryEntry, string, error)
}

// Status is what the page shows about the last sync.
type Status struct {
	Ready      bool      `json:"ready"` // a full read has completed
	Syncing    bool      `json:"syncing"`
	LastSync   time.Time `json:"last_sync,omitzero"`
	LastError  string    `json:"last_error,omitempty"`
	Plays      int       `json:"plays"`
	Configured bool      `json:"configured"`
}

// Syncer keeps a Store in step with Silo's playback history.
type Syncer struct {
	reader HistoryReader
	store  *Store
	logger *slog.Logger
	now    func() time.Time

	mu       sync.Mutex
	status   Status
	lastFull time.Time
	kick     chan struct{}
}

// NewSyncer returns a syncer; Run starts it.
func NewSyncer(reader HistoryReader, store *Store, logger *slog.Logger) *Syncer {
	return &Syncer{reader: reader, store: store, logger: logger, now: time.Now,
		kick: make(chan struct{}, 1), status: Status{Configured: reader != nil}}
}

// Status reports the last sync.
func (s *Syncer) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status
	st.Plays = s.store.Len()
	return st
}

// Kick asks for an incremental read soon, for example when the page opens.
func (s *Syncer) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run syncs until ctx ends.
func (s *Syncer) Run(ctx context.Context) {
	if s.reader == nil {
		return
	}
	for {
		wait := incrementalEvery
		if err := s.syncOnce(ctx); err != nil {
			wait = retryAfterError
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-s.kick:
			timer.Stop()
		}
	}
}

func (s *Syncer) syncOnce(ctx context.Context) error {
	s.mu.Lock()
	full := !s.status.Ready || s.now().Sub(s.lastFull) >= fullEvery
	s.status.Syncing = true
	s.mu.Unlock()

	var since time.Time
	if !full {
		if latest := s.store.Latest(); !latest.IsZero() {
			since = latest.Add(-incrementalOverlap)
		}
	}
	plays, err := s.read(ctx, since)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Syncing = false
	if err != nil {
		s.status.LastError = silo.Explain(err)
		if ctx.Err() == nil {
			s.logger.Warn("playback history sync failed", "full", full, "error", err)
		}
		return err
	}
	if full {
		s.store.Replace(plays)
		s.lastFull = s.now()
		s.status.Ready = true
	} else {
		s.store.Upsert(plays)
	}
	s.status.LastSync = s.now()
	s.status.LastError = ""
	return nil
}

func (s *Syncer) read(ctx context.Context, since time.Time) ([]Play, error) {
	var plays []Play
	cursor := ""
	for range maxPages {
		entries, next, err := s.reader.HistoryPage(ctx, since, cursor)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			plays = append(plays, PlayFrom(e))
		}
		if next == "" || next == cursor {
			return plays, nil
		}
		cursor = next
	}
	return plays, nil
}
