package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type pipelineRuntime struct {
	client   *ny511Client
	detector *detectorClient
	logger   *slog.Logger

	mu      sync.RWMutex
	active  []scoredCamera
	detect  detectWorkflowState
	detectM sync.Mutex
}

type detectWorkflowState struct {
	InProgress          bool
	LastStartedAt       time.Time
	LastCompletedAt     time.Time
	LastSuccessAt       time.Time
	NextAllowedAt       time.Time
	ConsecutiveFailures int
	LastError           string
	LastDurationMS      int64
}

type detectWorkflowView struct {
	Phase               string    `json:"phase"`
	InProgress          bool      `json:"in_progress"`
	LastStartedAt       time.Time `json:"last_started_at,omitempty"`
	LastCompletedAt     time.Time `json:"last_completed_at,omitempty"`
	LastSuccessAt       time.Time `json:"last_success_at,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	LastError           string    `json:"last_error,omitempty"`
	LastDurationMS      int64     `json:"last_duration_ms"`
	RetryAfterMS        int64     `json:"retry_after_ms"`
}

type focusSnapshot struct {
	Camera      scoredCamera
	Feed        cameraFeed
	ImageBytes  []byte
	ContentType string
}

func newPipelineRuntime(client *ny511Client, detector *detectorClient, logger *slog.Logger) *pipelineRuntime {
	return &pipelineRuntime{
		client:   client,
		detector: detector,
		logger:   logger,
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

	p.detectM.Lock()
	p.detect = detectWorkflowState{}
	p.detectM.Unlock()

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
	snapshot, err := p.fetchFocusSnapshot(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", snapshot.ContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Focus-Mode", mode)
	if mode == "processed" {
		// Placeholder until overlay rendering is implemented server-side.
		w.Header().Set("X-Focus-Overlay", "pending")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(snapshot.ImageBytes)
}

func (p *pipelineRuntime) handleFocusDetect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	now := time.Now().UTC()
	p.detectM.Lock()
	if p.detect.InProgress {
		workflow := p.detectWorkflowViewLocked(now)
		p.detectM.Unlock()
		writeJSON(w, http.StatusAccepted, map[string]any{
			"ok":       false,
			"workflow": workflow,
			"error":    "focus detection already in progress",
		})
		return
	}
	if now.Before(p.detect.NextAllowedAt) {
		workflow := p.detectWorkflowViewLocked(now)
		p.detectM.Unlock()
		writeJSON(w, http.StatusAccepted, map[string]any{
			"ok":       false,
			"workflow": workflow,
			"error":    "focus detection cooldown active",
		})
		return
	}
	p.detect.InProgress = true
	p.detect.LastStartedAt = now
	p.detect.LastError = ""
	p.detectM.Unlock()

	snapshot, err := p.fetchFocusSnapshot(r.Context())
	if err != nil {
		workflow := p.completeFocusDetectFailure(err)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       false,
			"workflow": workflow,
			"error":    err.Error(),
		})
		return
	}

	q := parseDetectQuery(r)
	if q.StreamID == "" {
		q.StreamID = fmt.Sprintf("cam-%d", snapshot.Camera.ID)
	}

	detectCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	detectorPayload, err := p.detector.Detect(detectCtx, snapshot.ImageBytes, q)
	if err != nil {
		workflow := p.completeFocusDetectFailure(err)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":                 false,
			"workflow":           workflow,
			"error":              err.Error(),
			"detector_available": false,
			"camera_id":          snapshot.Camera.ID,
			"stream_id":          q.StreamID,
		})
		return
	}

	workflow := p.completeFocusDetectSuccess()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                 true,
		"workflow":           workflow,
		"detector_available": true,
		"camera_id":          snapshot.Camera.ID,
		"stream_id":          q.StreamID,
		"detector":           detectorPayload,
	})
}

func (p *pipelineRuntime) fetchFocusSnapshot(ctx context.Context) (*focusSnapshot, error) {
	p.mu.RLock()
	active := append([]scoredCamera(nil), p.active...)
	p.mu.RUnlock()
	if len(active) == 0 {
		return nil, fmt.Errorf("pipeline has no active cameras; start pipeline first")
	}

	target := active[0]
	if len(target.Images) == 0 {
		return nil, fmt.Errorf("active camera has no image feed")
	}
	feed := target.Images[0]
	sourceURL := buildAbsoluteURL(feed.ImageURL)
	if sourceURL == "" {
		return nil, fmt.Errorf("active camera has no snapshot source")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build snapshot request: %w", err)
	}
	req.Header.Set("User-Agent", p.client.userAgent)

	resp, err := p.client.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch snapshot source: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("focus source unavailable (status %d)", resp.StatusCode)
	}

	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		contentType = "image/jpeg"
	}
	imageBytes, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read snapshot response: %w", err)
	}
	if len(imageBytes) == 0 {
		return nil, fmt.Errorf("focus snapshot was empty")
	}

	return &focusSnapshot{
		Camera:      target,
		Feed:        feed,
		ImageBytes:  imageBytes,
		ContentType: contentType,
	}, nil
}

func (p *pipelineRuntime) completeFocusDetectSuccess() detectWorkflowView {
	now := time.Now().UTC()
	p.detectM.Lock()
	defer p.detectM.Unlock()

	p.detect.InProgress = false
	p.detect.LastCompletedAt = now
	p.detect.LastSuccessAt = now
	p.detect.ConsecutiveFailures = 0
	p.detect.LastError = ""
	p.detect.NextAllowedAt = now.Add(500 * time.Millisecond)
	if !p.detect.LastStartedAt.IsZero() {
		p.detect.LastDurationMS = now.Sub(p.detect.LastStartedAt).Milliseconds()
	}

	return p.detectWorkflowViewLocked(now)
}

func (p *pipelineRuntime) completeFocusDetectFailure(err error) detectWorkflowView {
	now := time.Now().UTC()
	p.detectM.Lock()
	defer p.detectM.Unlock()

	p.detect.InProgress = false
	p.detect.LastCompletedAt = now
	p.detect.ConsecutiveFailures++
	p.detect.LastError = strings.TrimSpace(err.Error())
	backoff := failureBackoffDuration(p.detect.ConsecutiveFailures)
	p.detect.NextAllowedAt = now.Add(backoff)
	if !p.detect.LastStartedAt.IsZero() {
		p.detect.LastDurationMS = now.Sub(p.detect.LastStartedAt).Milliseconds()
	}

	return p.detectWorkflowViewLocked(now)
}

func (p *pipelineRuntime) detectWorkflowViewLocked(now time.Time) detectWorkflowView {
	phase := "ready"
	retryAfter := int64(0)
	if p.detect.InProgress {
		phase = "running"
	} else if now.Before(p.detect.NextAllowedAt) {
		phase = "cooldown"
		retryAfter = p.detect.NextAllowedAt.Sub(now).Milliseconds()
		if retryAfter < 0 {
			retryAfter = 0
		}
	} else if p.detect.LastCompletedAt.IsZero() {
		phase = "idle"
	}

	return detectWorkflowView{
		Phase:               phase,
		InProgress:          p.detect.InProgress,
		LastStartedAt:       p.detect.LastStartedAt,
		LastCompletedAt:     p.detect.LastCompletedAt,
		LastSuccessAt:       p.detect.LastSuccessAt,
		ConsecutiveFailures: p.detect.ConsecutiveFailures,
		LastError:           p.detect.LastError,
		LastDurationMS:      p.detect.LastDurationMS,
		RetryAfterMS:        retryAfter,
	}
}

func failureBackoffDuration(consecutiveFailures int) time.Duration {
	if consecutiveFailures <= 0 {
		return 0
	}
	// 1s, 2s, 4s, 8s ... capped at 30s to avoid detector floods.
	seconds := 1 << minInt(consecutiveFailures-1, 5)
	backoff := time.Duration(seconds) * time.Second
	if backoff > 30*time.Second {
		return 30 * time.Second
	}
	return backoff
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func parseDetectQuery(r *http.Request) detectQuery {
	var debugOverlay *bool
	if raw := strings.TrimSpace(r.URL.Query().Get("debug_overlay")); raw != "" {
		v := strings.EqualFold(raw, "1") || strings.EqualFold(raw, "true") || strings.EqualFold(raw, "yes")
		debugOverlay = &v
	}

	return detectQuery{
		StreamID:                strings.TrimSpace(r.URL.Query().Get("stream_id")),
		ImgSize:                 intQuery(r, "imgsz", 640),
		Conf:                    floatQuery(r, "conf", 0.25),
		IOU:                     floatQuery(r, "iou", 0.45),
		ROI:                     strings.TrimSpace(r.URL.Query().Get("roi")),
		Lanes:                   strings.TrimSpace(r.URL.Query().Get("lanes")),
		Direction:               strings.TrimSpace(r.URL.Query().Get("direction")),
		MovingSpeedThresholdPxS: floatQuery(r, "moving_speed_threshold_px_s", 12.0),
		SmoothingWindowSec:      floatQuery(r, "smoothing_window_sec", 0.0),
		DebugOverlay:            debugOverlay,
		ExtraQuery:              r.URL.Query(),
	}
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

func floatQuery(r *http.Request, key string, fallback float64) float64 {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}
	return v
}
