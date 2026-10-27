package report

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// ColorSettings are the report page's own settings, picked under Customize:
// chart colors and whether audiobooks are part of the reports, plus the
// order of the Breakdown tiles. Missing roles, an empty palette and an
// empty order mean the page's defaults.
type ColorSettings struct {
	Colors         map[string]string `json:"colors"`
	Palette        []string          `json:"palette"`
	ShowAudiobooks bool              `json:"show_audiobooks"`
	BreakdownOrder []string          `json:"breakdown_order"`
}

// BreakdownTiles are the tiles the Breakdown tab can order.
var BreakdownTiles = []string{"users", "stream", "clients", "movies", "tv", "audiobooks", "other"}

const (
	colorsStateKey = "settings/colors"
	maxPalette     = 16
)

// clean keeps known roles with valid #rrggbb values, up to 16 palette
// colors, lowercased, and each known Breakdown tile once.
func (c ColorSettings) clean() ColorSettings {
	out := ColorSettings{Colors: map[string]string{}, Palette: []string{}, ShowAudiobooks: c.ShowAudiobooks, BreakdownOrder: []string{}}
	for _, k := range ColorKeys {
		if v := strings.ToLower(strings.TrimSpace(c.Colors[k])); hexColor.MatchString(v) {
			out.Colors[k] = v
		}
	}
	for _, v := range c.Palette {
		if v = strings.ToLower(strings.TrimSpace(v)); hexColor.MatchString(v) && len(out.Palette) < maxPalette {
			out.Palette = append(out.Palette, v)
		}
	}
	for _, v := range c.BreakdownOrder {
		if slices.Contains(BreakdownTiles, v) && !slices.Contains(out.BreakdownOrder, v) {
			out.BreakdownOrder = append(out.BreakdownOrder, v)
		}
	}
	return out
}

// colors returns the saved colors, read from the plugin's state on first
// use. Colors set in the old settings form are the fallback.
func (h *Handler) colors(ctx context.Context) ColorSettings {
	h.colorMu.Lock()
	defer h.colorMu.Unlock()
	if h.savedColors == nil && h.State != nil {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if raw, ok, err := h.State.ReadInstanceState(ctx, colorsStateKey); err == nil {
			saved := ColorSettings{}
			if ok {
				_ = json.Unmarshal(raw, &saved)
			}
			saved = saved.clean()
			if !ok {
				saved = h.legacyColors()
			}
			h.savedColors = &saved
		}
	}
	if h.savedColors != nil {
		return *h.savedColors
	}
	return h.legacyColors()
}

func (h *Handler) legacyColors() ColorSettings {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.options == nil {
		return ColorSettings{}.clean()
	}
	return ColorSettings{Colors: h.options.Colors, Palette: h.options.Palette}.clean()
}

func (h *Handler) saveColors(ctx context.Context, c ColorSettings) (ColorSettings, error) {
	c = c.clean()
	if h.State == nil {
		return c, errNoState
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return c, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := h.State.WriteInstanceState(ctx, colorsStateKey, raw); err != nil {
		return c, err
	}
	h.colorMu.Lock()
	h.savedColors = &c
	h.colorMu.Unlock()
	return c, nil
}

type stateError string

func (e stateError) Error() string { return string(e) }

const errNoState = stateError("this plugin has no storage to save colors in")
