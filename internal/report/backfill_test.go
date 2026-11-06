package report

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Brady3035/silo-plugin-playback-reporting/internal/silo"
)

// Tests don't wait between catalog requests.
func init() { backfillEvery = time.Millisecond }

type fakeCatalog struct {
	profiles []silo.Profile
	versions map[string][]silo.FileVersion
	err      error
	asked    []string
	as       string
	limited  int // answer this many requests with 429 first
	mu       sync.Mutex
}

func (f *fakeCatalog) Profiles(context.Context) ([]silo.Profile, error) { return f.profiles, nil }

func (f *fakeCatalog) ItemVersions(_ context.Context, profileID, itemID string) ([]silo.FileVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked, f.as = append(f.asked, itemID), profileID
	if f.limited > 0 {
		f.limited--
		return nil, &silo.StatusError{Status: http.StatusTooManyRequests, RetryAfter: time.Millisecond}
	}
	if f.err != nil {
		return nil, f.err
	}
	if v, ok := f.versions[itemID]; ok {
		return v, nil
	}
	return nil, &silo.StatusError{Status: http.StatusNotFound}
}

func waitBackfill(t *testing.T, tr *Tracker) BackfillStatus {
	t.Helper()
	for range 200 {
		if s := tr.Backfill(); !s.Running {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("backfill never finished")
	return BackfillStatus{}
}

func TestPickProfile(t *testing.T) {
	kid := silo.Profile{ID: "kid", IsPrimary: true, IsChild: true}
	rated := silo.Profile{ID: "rated", MaxContentRating: "PG"}
	open := silo.Profile{ID: "open"}
	if id, ok := pickProfile([]silo.Profile{kid, rated, open}); !ok || id != "open" {
		t.Fatalf("picked %q %v, want the unrestricted profile", id, ok)
	}
	if id, ok := pickProfile([]silo.Profile{open, {ID: "main", IsPrimary: true}}); !ok || id != "main" {
		t.Fatalf("picked %q %v, want the unrestricted primary profile", id, ok)
	}
	if _, ok := pickProfile([]silo.Profile{kid, rated}); ok {
		t.Fatal("picked a restricted profile")
	}
}

func TestBackfillFillsUnknownFilesOnly(t *testing.T) {
	tr := NewTracker(nil, memState{}, quiet)
	tr.files["1"] = FileFormat{Resolution: "live"}
	plays := []Play{{ItemID: "a", FileID: "1"}, {ItemID: "b", FileID: "2"}, {ItemID: "b", FileID: "2"},
		{ItemID: "gone", FileID: "3"}, {ItemID: "c"}}
	src := &fakeCatalog{profiles: []silo.Profile{{ID: "p1", IsPrimary: true}}, versions: map[string][]silo.FileVersion{
		"b": {{FileID: "2", Resolution: "2160p", CodecVideo: "hevc", CodecAudio: "truehd"},
			{FileID: "1", Resolution: "1080p"}},
	}}
	src.versions["b"][0].AudioTracks = append(src.versions["b"][0].AudioTracks,
		struct {
			Codec    string `json:"codec"`
			Channels int    `json:"channels"`
			Default  bool   `json:"default"`
		}{Codec: "truehd", Channels: 8, Default: true})

	tr.StartBackfill(src, plays)
	s := waitBackfill(t, tr)
	if s.Error != "" || s.Items != 2 || s.Done != 2 || s.Filled != 1 || !s.Finished {
		t.Fatalf("status %+v", s)
	}
	if src.as != "p1" || len(src.asked) != 2 {
		t.Fatalf("asked %v as %q", src.asked, src.as)
	}
	if got, _ := tr.Format("2"); got != (FileFormat{Resolution: "2160p", Video: "hevc", Audio: "truehd", Channels: 8}) {
		t.Fatalf("file 2 = %+v", got)
	}
	if got, _ := tr.Format("1"); got.Resolution != "live" {
		t.Fatalf("backfill overwrote a live format: %+v", got)
	}
	if restored := NewTracker(nil, tr.state, quiet); !restored.load(t.Context()) {
		t.Fatal("load failed")
	} else if _, ok := restored.Format("2"); !ok {
		t.Fatal("backfilled format was not saved")
	}
}

func TestBackfillStopsWhenSiloRefuses(t *testing.T) {
	tr := NewTracker(nil, memState{}, quiet)
	src := &fakeCatalog{profiles: []silo.Profile{{ID: "p1"}}, err: &silo.StatusError{Status: http.StatusForbidden}}
	tr.StartBackfill(src, []Play{{ItemID: "a", FileID: "1"}, {ItemID: "b", FileID: "2"}})
	if s := waitBackfill(t, tr); s.Error == "" || len(src.asked) != 1 {
		t.Fatalf("status %+v after %v", s, src.asked)
	}
}

func TestBackfillWaitsOutRateLimits(t *testing.T) {
	tr := NewTracker(nil, memState{}, quiet)
	src := &fakeCatalog{profiles: []silo.Profile{{ID: "p1"}}, limited: 3, versions: map[string][]silo.FileVersion{
		"a": {{FileID: "1", Resolution: "1080p"}},
	}}
	tr.StartBackfill(src, []Play{{ItemID: "a", FileID: "1"}})
	s := waitBackfill(t, tr)
	if s.Error != "" || s.Filled != 1 || s.Missing != 0 || len(src.asked) != 4 {
		t.Fatalf("status %+v after %d requests", s, len(src.asked))
	}
}
