package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

type pipelineRuntime struct {
	client *ny511Client
	logger *slog.Logger

	mu     sync.RWMutex
	active []scoredCamera
}

func newPipelineRuntime(client *ny511Client, logger *slog.Logger) *pipelineRuntime {
	return &pipelineRuntime{
		client: client,
		logger: logger,
	}
}

func (p *pipelineRuntime) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	requestedCount := intQuery(r, "camera_count", defaultPipelineCameraCount)
	cameraCount := normalizePipelineCameraCount(requestedCount)

	payload, err := p.client.listCameras(ctx, 0, 250)
	if err != nil {
		p.logger.Error("list cameras for pipeline start failed", "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	ranked := recommendCameras(payload.Data, maxRecommendedCameraCount)
	selected, liveProbePass := selectRecommendedCameras(ctx, p.client, ranked, cameraCount)

	p.mu.Lock()
	p.active = append([]scoredCamera(nil), selected...)
	p.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                     true,
		"requested_camera_count": requestedCount,
		"camera_count":           cameraCount,
		"selected_count":         len(selected),
		"live_stream_probe_pass": liveProbePass,
		"data":                   selected,
	})
}

func (p *pipelineRuntime) handleFocusStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	mode := normalizeFocusMode(strings.TrimSpace(r.URL.Query().Get("mode")))

	p.mu.RLock()
	active := append([]scoredCamera(nil), p.active...)
	p.mu.RUnlock()

	if len(active) == 0 {
		http.Error(w, "pipeline has no active cameras; start pipeline first", http.StatusNotFound)
		return
	}

	target := active[0]
	feed := target.Images[0]
	sourceURL := buildAbsoluteURL(feed.ImageURL)
	if sourceURL == "" {
		http.Error(w, "active camera has no snapshot source", http.StatusBadGateway)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, sourceURL, nil)
	if err != nil {
		http.Error(w, "failed to build snapshot request", http.StatusBadGateway)
		return
	}
	req.Header.Set("User-Agent", p.client.userAgent)

	resp, err := p.client.httpClient.Do(req)
	if err != nil {
		http.Error(w, "failed to fetch focus stream source", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		http.Error(w, "focus source unavailable", http.StatusBadGateway)
		return
	}

	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		contentType = "image/jpeg"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Focus-Mode", mode)
	if mode == "processed" {
		// Placeholder until overlay rendering is implemented server-side.
		w.Header().Set("X-Focus-Overlay", "pending")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 8*1024*1024))
}

func normalizePipelineCameraCount(requested int) int {
	if requested < minPipelineCameraCount || requested > maxPipelineCameraCount {
		return defaultPipelineCameraCount
	}
	return requested
}

func normalizeFocusMode(mode string) string {
	switch strings.ToLower(mode) {
	case "live":
		return "live"
	case "processed", "raw":
		return "processed"
	default:
		return "processed"
	}
}
