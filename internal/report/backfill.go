package report

import (
	"context"
	"errors"
	"net/http"
	"sort"

	"github.com/Brady3035/silo-plugin-playback-reporting/internal/silo"
)

// The format backfill fills in the formats of files the tracker never saw
// played, by asking Silo's catalog for the files of each item in the
// history. It runs once when an admin asks for it; the tracker keeps the
// formats up to date from then on.

// FormatSource is the part of silo.Client the backfill uses.
type FormatSource interface {
	Profiles(ctx context.Context) ([]silo.Profile, error)
	ItemVersions(ctx context.Context, profileID, itemID string) ([]silo.FileVersion, error)
}

// BackfillStatus is how far the backfill got.
type BackfillStatus struct {
	Running  bool   `json:"running"`
	Finished bool   `json:"finished"`
	Items    int    `json:"items"`  // items to look up
	Done     int    `json:"done"`   // items looked up so far
	Filled   int    `json:"filled"` // files whose format it learned
	Missing  int    `json:"missing"`
	Error    string `json:"error,omitempty"`
}

// maxFailuresInARow stops a backfill when Silo keeps failing.
const maxFailuresInARow = 20

// unrestricted reports whether a profile sees the whole catalog.
func unrestricted(p silo.Profile) bool {
	return !p.IsChild && p.MaxContentRating == "" && p.MaxAdvisoryAge == nil && !p.LibraryRestrictionsEnabled
}

// pickProfile picks the profile the catalog is asked as: the account's
// primary profile when it is unrestricted, else any unrestricted one.
func pickProfile(profiles []silo.Profile) (string, bool) {
	for _, p := range profiles {
		if p.IsPrimary && unrestricted(p) {
			return p.ID, true
		}
	}
	for _, p := range profiles {
		if unrestricted(p) {
			return p.ID, true
		}
	}
	return "", false
}

// formatOf is a catalog file's format: the default audio track's channels,
// else those of the first track in the file's audio codec.
func formatOf(v silo.FileVersion) FileFormat {
	f := FileFormat{Resolution: v.Resolution, Video: v.CodecVideo, Audio: v.CodecAudio}
	for _, t := range v.AudioTracks {
		if t.Default {
			f.Channels = t.Channels
			break
		}
	}
	if f.Channels == 0 {
		for _, t := range v.AudioTracks {
			if t.Codec == v.CodecAudio {
				f.Channels = t.Channels
				break
			}
		}
	}
	return f
}

// missingItems lists the items with a played file of unknown format.
func (t *Tracker) missingItems(plays []Play) []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	seen := map[string]bool{}
	var items []string
	for _, p := range plays {
		if p.ItemID == "" || p.FileID == "" || seen[p.ItemID] {
			continue
		}
		if _, ok := t.files[p.FileID]; !ok {
			seen[p.ItemID] = true
			items = append(items, p.ItemID)
		}
	}
	sort.Strings(items)
	return items
}

// Backfill reports the running or last backfill.
func (t *Tracker) Backfill() BackfillStatus {
	if t == nil {
		return BackfillStatus{}
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.backfill
}

// StartBackfill starts a backfill over plays unless one is running, and
// returns its status.
func (t *Tracker) StartBackfill(src FormatSource, plays []Play) BackfillStatus {
	t.mu.Lock()
	if t.backfill.Running {
		defer t.mu.Unlock()
		return t.backfill
	}
	t.backfill = BackfillStatus{Running: true}
	t.mu.Unlock()
	items := t.missingItems(plays)
	t.mu.Lock()
	t.backfill.Items = len(items)
	status := t.backfill
	t.mu.Unlock()
	go t.runBackfill(context.Background(), src, items)
	return status
}

func (t *Tracker) runBackfill(ctx context.Context, src FormatSource, items []string) {
	finish := func(msg string) {
		// Save first, so a finished backfill is a saved one.
		if t.state != nil {
			t.saveFormats(ctx)
		}
		t.mu.Lock()
		t.backfill.Running, t.backfill.Finished, t.backfill.Error = false, true, msg
		t.mu.Unlock()
		if msg != "" {
			t.logger.Warn("filling in file formats stopped", "error", msg)
		}
	}
	if len(items) == 0 {
		finish("")
		return
	}
	profiles, err := src.Profiles(ctx)
	if err != nil {
		finish("Could not list the API key's profiles: " + err.Error())
		return
	}
	profile, ok := pickProfile(profiles)
	if !ok {
		finish("The API key's account has no profile without parental or library limits to read the catalog as.")
		return
	}
	failures := 0
	for _, item := range items {
		versions, err := src.ItemVersions(ctx, profile, item)
		var status *silo.StatusError
		switch {
		case errors.As(err, &status) && status.Status == http.StatusNotFound:
			// The item has left the catalog; its plays keep no format.
			err = nil
		case errors.As(err, &status) && (status.Status == http.StatusUnauthorized || status.Status == http.StatusForbidden ||
			status.Status == http.StatusBadRequest):
			finish("Silo would not show the catalog to the plugin: " + err.Error())
			return
		}
		t.mu.Lock()
		t.backfill.Done++
		if err != nil {
			t.backfill.Missing++
			failures++
		} else {
			failures = 0
			for _, v := range versions {
				f := formatOf(v)
				if _, known := t.files[v.FileID]; v.FileID == "" || known || f == (FileFormat{}) {
					continue
				}
				t.files[v.FileID] = f
				t.filesDirty = true
				t.changes++
				t.backfill.Filled++
			}
		}
		t.mu.Unlock()
		if failures >= maxFailuresInARow {
			finish("Silo kept failing to answer: " + err.Error())
			return
		}
		if ctx.Err() != nil {
			finish("stopped")
			return
		}
	}
	finish("")
}
