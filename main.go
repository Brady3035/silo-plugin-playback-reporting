// Command plugin is the Playback Reporting plugin for Silo: an admin page with
// reports built from Silo's playback history.
package main

import (
	"context"
	_ "embed"
	"errors"
	"log/slog"
	"os"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"

	"github.com/Brady3035/silo-plugin-playback-reporting/internal/report"
	"github.com/Brady3035/silo-plugin-playback-reporting/internal/silo"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.1.0-dev"

//go:embed manifest.json
var manifestJSON []byte

func main() {
	// go-plugin forwards the plugin's stderr to the host log.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	a := &app{logger: logger, handler: &report.Handler{Store: report.NewStore()}}
	sdkruntime.ServeManifestWithOptions(withPlatform(manifestJSON), version,
		sdkruntime.CapabilityServers{HttpRoutes: a.handler},
		sdkruntime.WithConfigure(a.configure),
	)
}

// app wires settings to the background work. The host calls Configure once
// per process start and restarts the plugin when settings change.
type app struct {
	logger  *slog.Logger
	handler *report.Handler
	once    sync.Once
}

func (a *app) configure(_ context.Context, entries []*pluginv1.ConfigEntry) error {
	cfg := parseConfig(entries)
	a.handler.SetOptions(cfg.Options)
	if cfg.APIKey == "" {
		a.handler.Configure(nil, nil, "Add an admin API key in this plugin's settings (Silo connection) to see reports.")
		return nil
	}
	a.once.Do(func() {
		// Configure must return quickly; finding the server and syncing run
		// in the background for the life of the process.
		lazy := &lazyClient{cfg: cfg, logger: a.logger}
		syncer := report.NewSyncer(lazy, a.handler.Store, a.logger)
		var tracker *report.Tracker
		if cfg.TrackClients {
			tracker = report.NewTracker(lazy, hostState{}, a.logger)
			go tracker.Run(context.Background())
		}
		a.handler.Configure(syncer, tracker, "")
		go syncer.Run(context.Background())
	})
	return nil
}

type config struct {
	APIKey       string
	BaseURL      string
	TrackClients bool
	Options      report.Options
}

func parseConfig(entries []*pluginv1.ConfigEntry) config {
	var cfg config
	for _, e := range entries {
		if e == nil || e.GetValue() == nil {
			continue
		}
		v := e.GetValue().AsMap()
		switch e.GetKey() {
		case "connection":
			cfg.APIKey, _ = v["api_key"].(string)
			cfg.BaseURL, _ = v["base_url"].(string)
		case "features":
			cfg.TrackClients, _ = v["track_clients"].(bool)
		case "reports":
			// Numbers arrive as float64 from structpb.
			if n, ok := v["min_play_seconds"].(float64); ok && n > 0 {
				cfg.Options.MinSeconds = int(min(n, 86400))
			}
			text, _ := v["ignored"].(string)
			cfg.Options.Ignore = report.ParseIgnore(text)
		case "colors":
			cfg.Options.Colors, cfg.Options.Palette = report.ParseColors(v)
		}
	}
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	return cfg
}

// lazyClient finds Silo's address on first use: the configured one, or the
// host's own API listener, which the plugin process can reach on loopback.
type lazyClient struct {
	cfg    config
	logger *slog.Logger
	mu     sync.Mutex
	client *silo.Client
}

func (l *lazyClient) get(ctx context.Context) (*silo.Client, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.client != nil {
		return l.client, nil
	}
	base := l.cfg.BaseURL
	if base == "" {
		host := sdkruntime.Host()
		if host == nil {
			return nil, errors.New("the Silo host connection is not ready yet")
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		info, err := host.GetHostInfo(ctx)
		if err != nil {
			return nil, err
		}
		if base = strings.TrimRight(info.InternalBaseURL, "/"); base == "" {
			base = strings.TrimRight(info.PublicBaseURL, "/")
		}
		if base == "" {
			return nil, errors.New("Silo reported no address to reach its API; set one under Silo connection")
		}
	}
	l.client = silo.New(base, l.cfg.APIKey, nil)
	return l.client, nil
}

func (l *lazyClient) HistoryPage(ctx context.Context, endedAfter time.Time, cursor string) ([]silo.HistoryEntry, string, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, "", err
	}
	return c.HistoryPage(ctx, endedAfter, cursor)
}

func (l *lazyClient) SessionsPage(ctx context.Context, cursor string) ([]silo.LiveSession, string, error) {
	c, err := l.get(ctx)
	if err != nil {
		return nil, "", err
	}
	return c.SessionsPage(ctx, cursor)
}

// hostState is the host's per-installation state store, bound lazily because
// the host connection arrives after the plugin starts.
type hostState struct{}

func (hostState) ReadInstanceState(ctx context.Context, key string) ([]byte, bool, error) {
	host := sdkruntime.Host()
	if host == nil {
		return nil, false, errors.New("host not ready")
	}
	return host.ReadInstanceState(ctx, key)
}

func (hostState) WriteInstanceState(ctx context.Context, key string, value []byte) error {
	host := sdkruntime.Host()
	if host == nil {
		return errors.New("host not ready")
	}
	return host.WriteInstanceState(ctx, key, value)
}

// withPlatform fills in supported_platforms, which the host requires, with
// the platform this binary was built for.
func withPlatform(manifest []byte) []byte {
	platform := `"supported_platforms": [{"os": "` + goruntime.GOOS + `", "arch": "` + goruntime.GOARCH + `"}],`
	return []byte(strings.Replace(string(manifest), `"capabilities": [`, platform+"\n  \"capabilities\": [", 1))
}
