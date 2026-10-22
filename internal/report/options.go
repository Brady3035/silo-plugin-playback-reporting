package report

import (
	"fmt"
	"regexp"
	"strings"
)

// Options are the admin's report settings. They hide plays from the reports;
// Silo's own history is never changed.
type Options struct {
	// MinSeconds hides plays watched for less than this many seconds.
	MinSeconds int
	// Ignore hides whole accounts or single profiles.
	Ignore []IgnoreRule
	// Colors maps a chart role (direct_play, transcode, movies, ...) to a
	// #rrggbb color; Palette colors users in order. Both are already valid.
	Colors  map[string]string
	Palette []string
}

// IgnoreRule matches a username, or one profile of it when Profile is set.
// Matching ignores case.
type IgnoreRule struct {
	User    string
	Profile string
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// ColorKeys are the chart roles a color can be set for.
var ColorKeys = []string{"direct_play", "direct_stream", "transcode", "movies", "tv", "audiobooks"}

// ParseIgnore reads one rule per line: "username" or "username/profile".
// Blank lines and lines starting with # are skipped.
func ParseIgnore(text string) []IgnoreRule {
	var rules []IgnoreRule
	seen := map[IgnoreRule]bool{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		user, profile, _ := strings.Cut(line, "/")
		r := IgnoreRule{User: strings.ToLower(strings.TrimSpace(user)), Profile: strings.ToLower(strings.TrimSpace(profile))}
		if r.User == "" || seen[r] {
			continue
		}
		seen[r] = true
		rules = append(rules, r)
	}
	return rules
}

// ParseColors keeps the valid #rrggbb values for the known roles and a
// comma-separated palette of up to 16 colors. Invalid values fall back to
// the page's defaults.
func ParseColors(values map[string]any) (map[string]string, []string) {
	colors := map[string]string{}
	for _, k := range ColorKeys {
		if s, _ := values[k].(string); hexColor.MatchString(strings.TrimSpace(s)) {
			colors[k] = strings.ToLower(strings.TrimSpace(s))
		}
	}
	var palette []string
	if s, _ := values["palette"].(string); s != "" {
		for _, c := range strings.Split(s, ",") {
			if c = strings.TrimSpace(c); hexColor.MatchString(c) && len(palette) < 16 {
				palette = append(palette, strings.ToLower(c))
			}
		}
	}
	return colors, palette
}

// keep reports whether a play belongs in the reports.
func (o *Options) keep(p *Play) bool {
	if o == nil {
		return true
	}
	if o.MinSeconds > 0 && p.Watched < float64(o.MinSeconds) {
		return false
	}
	if len(o.Ignore) > 0 {
		user, profile := strings.ToLower(p.User), strings.ToLower(p.Profile)
		for _, r := range o.Ignore {
			if r.User == user && (r.Profile == "" || r.Profile == profile) {
				return false
			}
		}
	}
	return true
}

// apply returns the plays the reports show and how many were hidden.
func (o *Options) apply(plays []Play) ([]Play, int) {
	if o == nil || (o.MinSeconds <= 0 && len(o.Ignore) == 0) {
		return plays, 0
	}
	kept := make([]Play, 0, len(plays))
	for i := range plays {
		if o.keep(&plays[i]) {
			kept = append(kept, plays[i])
		}
	}
	return kept, len(plays) - len(kept)
}

// key identifies the options for snapshot reuse.
func (o *Options) key() string {
	if o == nil {
		return ""
	}
	return fmt.Sprintf("%d/%v/%v/%v", o.MinSeconds, o.Ignore, o.Colors, o.Palette)
}
