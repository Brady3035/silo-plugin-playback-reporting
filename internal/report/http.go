package report

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

//go:embed web/index.html web/app.js web/app.css
var webFiles embed.FS

var pageTemplate = template.Must(template.ParseFS(webFiles, "web/index.html"))

// assetVersion busts browser caches when the plugin is updated.
var assetVersion = strconv.FormatInt(time.Now().Unix(), 36)

// Handler serves the Playback Reporting page and its data (http_routes.v1).
type Handler struct {
	pluginv1.UnimplementedHttpRoutesServer

	Store *Store
	now   func() time.Time
	snaps snapshots

	mu      sync.RWMutex
	syncer  *Syncer
	tracker *Tracker // nil unless client tracking is on
	// problem, when set, is shown instead of the reports (no API key).
	problem string
	options *Options

	// State stores saved chart colors; nil in tests that don't need it.
	State       StateStore
	colorMu     sync.Mutex
	savedColors *ColorSettings
}

// SetOptions sets the report settings (hidden plays, colors).
func (h *Handler) SetOptions(o Options) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.options = &o
}

// Configure sets what the handler serves. The host configures the plugin
// before it routes requests, but a later Configure is safe too.
func (h *Handler) Configure(syncer *Syncer, tracker *Tracker, problem string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.syncer, h.tracker, h.problem = syncer, tracker, problem
}

func (h *Handler) parts() (*Syncer, *Tracker, string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.syncer, h.tracker, h.problem
}

var _ pluginv1.HttpRoutesServer = (*Handler)(nil)

// Handle answers one proxied request. The host already restricts every route
// to administrators; the role header is checked again in case the plugin is
// reached some other way.
func (h *Handler) Handle(ctx context.Context, req *pluginv1.HandleHTTPRequest) (*pluginv1.HandleHTTPResponse, error) {
	if !strings.EqualFold(header(req, "X-Silo-User-Role"), "admin") {
		return text(http.StatusForbidden, "Playback Reporting is for administrators."), nil
	}
	if req.GetPath() == "/settings" {
		return h.handleColors(ctx, req), nil
	}
	if req.GetMethod() != http.MethodGet && req.GetMethod() != http.MethodHead {
		return text(http.StatusMethodNotAllowed, "method not allowed"), nil
	}
	switch p := req.GetPath(); {
	case p == "/" || p == "":
		return h.page(ctx, req), nil
	case p == "/data":
		return h.data(req), nil
	case strings.HasPrefix(p, "/assets/"):
		return asset(strings.TrimPrefix(p, "/assets/")), nil
	}
	return text(http.StatusNotFound, "not found"), nil
}

func (h *Handler) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

func (h *Handler) page(ctx context.Context, req *pluginv1.HandleHTTPRequest) *pluginv1.HandleHTTPResponse {
	syncer, _, problem := h.parts()
	if syncer != nil {
		syncer.Kick()
	}
	theme := strings.ToLower(header(req, "X-Silo-Theme"))
	if theme != "light" && theme != "dark" {
		theme = ""
	}
	var body bytes.Buffer
	err := pageTemplate.Execute(&body, map[string]any{
		"Theme":   theme,
		"Version": assetVersion,
		"Viewer":  header(req, "X-Silo-User-Name"),
		"Problem": problem,
		"Colors":  h.colors(ctx),
	})
	if err != nil {
		return text(http.StatusInternalServerError, "page failed to render")
	}
	return &pluginv1.HandleHTTPResponse{StatusCode: http.StatusOK, Body: body.Bytes(), Headers: map[string]string{
		"Content-Type":  "text/html; charset=utf-8",
		"Cache-Control": "no-store",
	}}
}

func (h *Handler) data(req *pluginv1.HandleHTTPRequest) *pluginv1.HandleHTTPResponse {
	syncer, tracker, problem := h.parts()
	if problem != "" {
		return jsonResponse(http.StatusServiceUnavailable, []byte(`{"error":`+strconv.Quote(problem)+`}`))
	}
	now := h.clock()
	q := req.GetQuery().AsMap()
	index, _ := strconv.Atoi(stringOf(q["chunk"]))
	var snap *snapshot
	if id := stringOf(q["snapshot"]); id != "" {
		if snap = h.snaps.get(id, now); snap == nil {
			return jsonResponse(http.StatusGone, []byte(`{"error":"This data snapshot expired. Reload the page."}`))
		}
	} else {
		status := Status{}
		if syncer != nil {
			status = syncer.Status()
		}
		feat := features{Clients: tracker != nil, ClientsError: tracker.LastError()}
		var err error
		h.mu.RLock()
		opts := h.options
		h.mu.RUnlock()
		if snap, err = h.snaps.current(h.Store, status, feat, tracker, opts, now); err != nil {
			return jsonResponse(http.StatusInternalServerError, []byte(`{"error":"Could not build the report data."}`))
		}
	}
	if index < 0 || index >= len(snap.chunks) {
		return jsonResponse(http.StatusNotFound, []byte(`{"error":"No such chunk."}`))
	}
	return &pluginv1.HandleHTTPResponse{StatusCode: http.StatusOK, Body: snap.chunks[index], Headers: map[string]string{
		"Content-Type":  "application/gzip",
		"Cache-Control": "no-store",
	}}
}

// handleColors saves the settings picked under Customize (POST, JSON). The
// JSON content type keeps other sites from posting here: a cross-site form
// can't send it without a preflight, which this route doesn't answer, and
// the host's launch cookie is SameSite=Lax.
func (h *Handler) handleColors(ctx context.Context, req *pluginv1.HandleHTTPRequest) *pluginv1.HandleHTTPResponse {
	if req.GetMethod() != http.MethodPost {
		return text(http.StatusMethodNotAllowed, "method not allowed")
	}
	if ct, _, _ := strings.Cut(header(req, "Content-Type"), ";"); !strings.EqualFold(strings.TrimSpace(ct), "application/json") {
		return jsonResponse(http.StatusUnsupportedMediaType, []byte(`{"error":"Send settings as JSON."}`))
	}
	if len(req.GetBody()) > 16<<10 {
		return jsonResponse(http.StatusRequestEntityTooLarge, []byte(`{"error":"Too many colors."}`))
	}
	var in ColorSettings
	if err := json.Unmarshal(req.GetBody(), &in); err != nil {
		return jsonResponse(http.StatusBadRequest, []byte(`{"error":"Those settings could not be read."}`))
	}
	saved, err := h.saveColors(ctx, in)
	if err != nil {
		return jsonResponse(http.StatusInternalServerError, []byte(`{"error":"Silo could not save the settings. Try again."}`))
	}
	body, _ := json.Marshal(saved)
	return jsonResponse(http.StatusOK, body)
}

func asset(name string) *pluginv1.HandleHTTPResponse {
	if name == "" || strings.Contains(name, "..") || path.Clean("/"+name) != "/"+name {
		return text(http.StatusNotFound, "not found")
	}
	body, err := webFiles.ReadFile("web/" + name)
	if err != nil || name == "index.html" {
		return text(http.StatusNotFound, "not found")
	}
	contentType := "application/octet-stream"
	switch path.Ext(name) {
	case ".js":
		contentType = "text/javascript; charset=utf-8"
	case ".css":
		contentType = "text/css; charset=utf-8"
	}
	return &pluginv1.HandleHTTPResponse{StatusCode: http.StatusOK, Body: body, Headers: map[string]string{
		"Content-Type": contentType,
		// The page references assets with a version query, so they can be
		// cached for long.
		"Cache-Control": "private, max-age=86400",
	}}
}

func header(req *pluginv1.HandleHTTPRequest, name string) string {
	for k, v := range req.GetHeaders() {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

func text(status int, msg string) *pluginv1.HandleHTTPResponse {
	return &pluginv1.HandleHTTPResponse{StatusCode: int32(status), Body: []byte(msg), Headers: map[string]string{
		"Content-Type":  "text/plain; charset=utf-8",
		"Cache-Control": "no-store",
	}}
}

func jsonResponse(status int, body []byte) *pluginv1.HandleHTTPResponse {
	return &pluginv1.HandleHTTPResponse{StatusCode: int32(status), Body: body, Headers: map[string]string{
		"Content-Type":  "application/json",
		"Cache-Control": "no-store",
	}}
}
