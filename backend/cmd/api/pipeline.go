package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errPipelineAlreadyRunning = errors.New("pipeline already running")

type pipelineConfig struct {
	CameraCount       int
	SampleInterval    time.Duration
	BufferDuration    time.Duration
	PreEventDuration  time.Duration
	PostEventDuration time.Duration
}

func (c pipelineConfig) withDefaults() pipelineConfig {
	if c.CameraCount < 5 {
		c.CameraCount = 10
	}
	if c.CameraCount > 25 {
		c.CameraCount = 25
	}
	if c.SampleInterval < time.Second {
		c.SampleInterval = 3 * time.Second
	}
	if c.BufferDuration < 30*time.Second || c.BufferDuration > 90*time.Second {
		c.BufferDuration = 90 * time.Second
	}
	if c.PreEventDuration <= 0 {
		c.PreEventDuration = 30 * time.Second
	}
	if c.PostEventDuration <= 0 {
		c.PostEventDuration = 45 * time.Second
	}
	return c
}

type startPipelineRequest struct {
	CameraCount       int `json:"camera_count"`
	SampleIntervalSec int `json:"sample_interval_sec"`
	BufferSeconds     int `json:"buffer_seconds"`
	PreEventSeconds   int `json:"pre_event_seconds"`
	PostEventSeconds  int `json:"post_event_seconds"`
}

func (r startPipelineRequest) toConfig() pipelineConfig {
	cfg := pipelineConfig{
		CameraCount:       r.CameraCount,
		SampleInterval:    time.Duration(r.SampleIntervalSec) * time.Second,
		BufferDuration:    time.Duration(r.BufferSeconds) * time.Second,
		PreEventDuration:  time.Duration(r.PreEventSeconds) * time.Second,
		PostEventDuration: time.Duration(r.PostEventSeconds) * time.Second,
	}
	return cfg.withDefaults()
}

type frameMetrics struct {
	At        time.Time `json:"at"`
	Motion    float64   `json:"motion"`
	Occupancy float64   `json:"occupancy"`
}

type frameSnapshot struct {
	At      time.Time
	Raw     []byte
	Metrics frameMetrics
}

type pendingCapture struct {
	EventType string
	Score     float64
	Reason    string
	StartedAt time.Time
	PostUntil time.Time
	Frames    []frameSnapshot
}

type cameraState struct {
	Meta         scoredCamera
	Frames       []frameSnapshot
	PrevSmall    []uint8
	Failures     int
	LastError    string
	LastAlertAt  time.Time
	Pending      *pendingCapture
	OfflineAlert bool
}

type cameraLiveView struct {
	CameraID          int       `json:"camera_id"`
	Location          string    `json:"location"`
	Roadway           string    `json:"roadway"`
	Direction         string    `json:"direction"`
	StreamURL         string    `json:"stream_url"`
	SourceImageURL    string    `json:"source_image_url,omitempty"`
	LiveImageURL      string    `json:"live_image_url"`
	ProcessedImageURL string    `json:"processed_image_url"`
	LastFrameAt       time.Time `json:"last_frame_at,omitempty"`
	Motion            float64   `json:"motion"`
	Occupancy         float64   `json:"occupancy"`
	Failures          int       `json:"failures"`
	LastError         string    `json:"last_error,omitempty"`
}

type incidentAlert struct {
	ID             string    `json:"id"`
	EventType      string    `json:"event_type"`
	Location       string    `json:"location"`
	Roadway        string    `json:"roadway"`
	Direction      string    `json:"direction"`
	CameraID       int       `json:"camera_id"`
	Timestamp      time.Time `json:"timestamp"`
	DurationSec    int       `json:"duration_sec"`
	Confidence     float64   `json:"confidence"`
	Score          float64   `json:"score"`
	Reason         string    `json:"reason"`
	BeforeImageURL string    `json:"before_image_url,omitempty"`
	AfterImageURL  string    `json:"after_image_url,omitempty"`
	ClipURL        string    `json:"clip_url,omitempty"`
	WebhookSent    bool      `json:"webhook_sent"`
}

type pipelineStatus struct {
	Running        bool      `json:"running"`
	StartedAt      time.Time `json:"started_at,omitempty"`
	LastTick       time.Time `json:"last_tick,omitempty"`
	CameraCount    int       `json:"camera_count"`
	AlertsCount    int       `json:"alerts_count"`
	ArtifactDir    string    `json:"artifact_dir"`
	SampleInterval string    `json:"sample_interval"`
	BufferSeconds  int       `json:"buffer_seconds"`
	UptimeSec      int       `json:"uptime_sec"`
}

type pipelineManager struct {
	logger      *slog.Logger
	client      *ny511Client
	artifactDir string
	webhookURL  string
	httpClient  *http.Client

	mu       sync.RWMutex
	running  bool
	started  time.Time
	lastTick time.Time
	config   pipelineConfig
	cameras  []*cameraState
	views    map[int]cameraLiveView
	alerts   []incidentAlert
	cancel   context.CancelFunc
}

type mjpegStreamType string

const (
	streamTypeLive      mjpegStreamType = "live"
	streamTypeProcessed mjpegStreamType = "processed"
)

func newPipelineManager(logger *slog.Logger, client *ny511Client, artifactDir, webhookURL string) (*pipelineManager, error) {
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		return nil, fmt.Errorf("create artifact directory: %w", err)
	}
	return &pipelineManager{
		logger:      logger,
		client:      client,
		artifactDir: artifactDir,
		webhookURL:  strings.TrimSpace(webhookURL),
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		alerts:      make([]incidentAlert, 0, 200),
		views:       make(map[int]cameraLiveView, 64),
	}, nil
}

func (m *pipelineManager) Start(ctx context.Context, cfg pipelineConfig) error {
	cfg = cfg.withDefaults()
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return errPipelineAlreadyRunning
	}
	m.mu.Unlock()

	cams, err := m.fetchPipelineCameras(ctx, cfg.CameraCount)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return errPipelineAlreadyRunning
	}
	m.running = true
	m.started = time.Now().UTC()
	m.lastTick = time.Time{}
	m.config = cfg
	m.cameras = cams
	m.views = make(map[int]cameraLiveView, len(cams))
	for _, cam := range cams {
		streamURL := ""
		srcImg := ""
		if len(cam.Meta.Images) > 0 {
			streamURL = cam.Meta.Images[0].VideoURL
			srcImg = cam.Meta.Images[0].ImageURL
		}
		m.views[cam.Meta.ID] = cameraLiveView{
			CameraID:       cam.Meta.ID,
			Location:       cam.Meta.Location,
			Roadway:        cam.Meta.Roadway,
			Direction:      cam.Meta.Direction,
			StreamURL:      streamURL,
			SourceImageURL: srcImg,
			Failures:       0,
			LastError:      "",
			LastFrameAt:    time.Time{},
		}
	}
	runCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	go m.run(runCtx)
	return nil
}

func (m *pipelineManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running {
		return
	}
	m.running = false
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
}

func (m *pipelineManager) Status() pipelineStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	uptime := 0
	if m.running && !m.started.IsZero() {
		uptime = int(time.Since(m.started).Seconds())
	}
	return pipelineStatus{
		Running:        m.running,
		StartedAt:      m.started,
		LastTick:       m.lastTick,
		CameraCount:    len(m.cameras),
		AlertsCount:    len(m.alerts),
		ArtifactDir:    m.artifactDir,
		SampleInterval: m.config.SampleInterval.String(),
		BufferSeconds:  int(m.config.BufferDuration.Seconds()),
		UptimeSec:      uptime,
	}
}

func (m *pipelineManager) ListAlerts(limit int) []incidentAlert {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if limit > len(m.alerts) {
		limit = len(m.alerts)
	}
	out := make([]incidentAlert, limit)
	copy(out, m.alerts[:limit])
	return out
}

func (m *pipelineManager) ListCameraViews() []cameraLiveView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]cameraLiveView, 0, len(m.views))
	for _, v := range m.views {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastFrameAt.Equal(out[j].LastFrameAt) {
			return out[i].CameraID < out[j].CameraID
		}
		return out[i].LastFrameAt.After(out[j].LastFrameAt)
	})
	return out
}

func (m *pipelineManager) HasCamera(cameraID int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.views[cameraID]
	return ok
}

func (m *pipelineManager) StreamFocus(w http.ResponseWriter, r *http.Request, cameraID int, streamType string, fps int) {
	st := strings.ToLower(strings.TrimSpace(streamType))
	if st == "raw" {
		st = "live"
	}
	sType := mjpegStreamType(st)
	if sType != streamTypeLive && sType != streamTypeProcessed {
		sType = streamTypeProcessed
	}
	m.serveMJPEG(w, r, cameraID, sType, fps)
}

// WriteFocusSnapshot returns one JPEG for UI polling when <img> cannot animate MJPEG
// (Chrome). Both modes fetch the latest 511 snapshot and, for processed, draw the overlay.
func (m *pipelineManager) WriteFocusSnapshot(ctx context.Context, w http.ResponseWriter, cameraID int, mode string) {
	st := strings.ToLower(strings.TrimSpace(mode))
	if st == "raw" {
		st = "live"
	}
	sType := mjpegStreamType(st)
	if sType != streamTypeLive && sType != streamTypeProcessed {
		sType = streamTypeProcessed
	}

	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	url := m.snapshotImageURLForCamera(cameraID)
	if url == "" {
		http.Error(w, "unknown camera", http.StatusNotFound)
		return
	}
	raw, err := m.client.fetchCameraImageFresh(ctx, url)
	if err != nil || len(raw) == 0 {
		http.Error(w, "snapshot fetch failed", http.StatusBadGateway)
		return
	}

	if sType == streamTypeProcessed {
		metrics := frameMetrics{At: time.Now().UTC()}
		var prevSmall []uint8
		if grid, sErr := sampleSmallGray(raw, 64, 36); sErr == nil {
			metrics.Motion, metrics.Occupancy = deriveMetrics(prevSmall, grid, 64, 36)
			prevSmall = grid
		}
		if processed, pErr := buildProcessedFrame(raw, metrics); pErr == nil && len(processed) > 0 {
			raw = processed
		}
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
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

func (m *pipelineManager) serveMJPEG(w http.ResponseWriter, r *http.Request, cameraID int, streamType mjpegStreamType, fps int) {
	if fps < 1 {
		fps = 1
	}
	if fps > 30 {
		fps = 30
	}
	interval := time.Second / time.Duration(fps)
	if interval <= 0 {
		interval = 33 * time.Millisecond
	}
	// Snapshot endpoints are often updated slower than 30fps, so fetch at a bounded
	// rate and duplicate latest frame to keep a smooth output cadence.
	fetchInterval := interval
	if fetchInterval < 100*time.Millisecond {
		fetchInterval = 100 * time.Millisecond
	}
	if fetchInterval > 1500*time.Millisecond {
		fetchInterval = 1500 * time.Millisecond
	}
	imageURL := m.snapshotImageURLForCamera(cameraID)

	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	ctx := r.Context()

	var (
		lastSentAt time.Time
		lastBytes  []byte
		lastFetch  time.Time
		prevSmall  []uint8
	)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().UTC()

			shouldFetch := len(lastBytes) == 0 || now.Sub(lastFetch) >= fetchInterval
			if shouldFetch && imageURL != "" {
				fetchTimeout := 2 * time.Second
				if interval > 200*time.Millisecond {
					fetchTimeout = 3 * time.Second
				}
				fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
				raw, err := m.client.fetchCameraImageFresh(fetchCtx, imageURL)
				cancel()
				if err == nil && len(raw) > 0 {
					lastFetch = now
					lastSentAt = now
					if streamType == streamTypeProcessed {
						metrics := frameMetrics{At: now}
						if grid, sErr := sampleSmallGray(raw, 64, 36); sErr == nil {
							metrics.Motion, metrics.Occupancy = deriveMetrics(prevSmall, grid, 64, 36)
							prevSmall = grid
						}
						if processed, pErr := buildProcessedFrame(raw, metrics); pErr == nil && len(processed) > 0 {
							lastBytes = processed
						} else {
							lastBytes = raw
						}
					} else {
						lastBytes = raw
					}
				}
			}

			currentBytes := lastBytes
			currentAt := lastSentAt
			if len(currentBytes) == 0 {
				currentBytes, currentAt = m.latestFrameFor(cameraID, streamType)
			}
			if len(currentBytes) == 0 {
				continue
			}

			if _, err := fmt.Fprintf(w, "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(currentBytes)); err != nil {
				return
			}
			if _, err := w.Write(currentBytes); err != nil {
				return
			}
			if _, err := w.Write([]byte("\r\n")); err != nil {
				return
			}
			flusher.Flush()
			lastSentAt = currentAt
			lastBytes = currentBytes
		}
	}
}

func (m *pipelineManager) latestFrameFor(cameraID int, streamType mjpegStreamType) ([]byte, time.Time) {
	m.mu.RLock()
	view, ok := m.views[cameraID]
	m.mu.RUnlock()
	if !ok {
		return nil, time.Time{}
	}
	var rel string
	if streamType == streamTypeProcessed {
		rel = view.ProcessedImageURL
	} else {
		rel = view.LiveImageURL
	}
	if rel == "" {
		return nil, view.LastFrameAt
	}
	rel = strings.TrimPrefix(rel, "/artifacts/")
	path := filepath.Join(m.artifactDir, rel)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, view.LastFrameAt
	}
	return b, view.LastFrameAt
}

func (m *pipelineManager) cameraImageURLByID(cameraID int) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, cam := range m.cameras {
		if cam.Meta.ID == cameraID && len(cam.Meta.Images) > 0 {
			return cam.Meta.Images[0].ImageURL
		}
	}
	return ""
}

// snapshotImageURLForCamera resolves the 511 snapshot image URL for focus/snapshot.
// It prefers the in-memory pipeline camera list, then the view map (covers races before
// the first artifact write).
func (m *pipelineManager) snapshotImageURLForCamera(cameraID int) string {
	if u := m.cameraImageURLByID(cameraID); u != "" {
		return u
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.views[cameraID]
	if !ok {
		return ""
	}
	return strings.TrimSpace(v.SourceImageURL)
}

func (m *pipelineManager) fetchPipelineCameras(ctx context.Context, count int) ([]*cameraState, error) {
	pageSize := 200
	start := 0
	all := make([]camera, 0, 500)
	for {
		payload, err := m.client.listCameras(ctx, start, pageSize)
		if err != nil {
			return nil, fmt.Errorf("fetch camera page start=%d: %w", start, err)
		}
		all = append(all, payload.Data...)
		start += pageSize
		if start >= payload.RecordsFiltered || len(payload.Data) == 0 || len(all) >= 2500 {
			break
		}
	}
	selected := selectLiveRecommended(ctx, m.client, all, count)
	out := make([]*cameraState, 0, len(selected))
	for _, cam := range selected {
		out = append(out, &cameraState{Meta: cam, Frames: make([]frameSnapshot, 0, 100)})
	}
	return out, nil
}

func (m *pipelineManager) tickOnce(ctx context.Context) {
	_ = ctx
	m.mu.Lock()
	m.lastTick = time.Now().UTC()
	m.mu.Unlock()
	// Stub: full implementation would fetch frames, update views, run heuristics, and append alerts.
}

func (m *pipelineManager) run(ctx context.Context) {
	m.logger.Info("incident pipeline started")
	defer m.logger.Info("incident pipeline stopped")
	ticker := time.NewTicker(m.config.SampleInterval)
	defer ticker.Stop()

	m.tickOnce(ctx) // warm start immediately
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.tickOnce(ctx)
		}
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
