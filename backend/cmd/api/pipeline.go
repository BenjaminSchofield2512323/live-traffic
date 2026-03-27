package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/color/palette"
	"image/draw"
	"image/gif"
	"image/jpeg"
	_ "image/jpeg"
	_ "image/png"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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
	if c.CameraCount < 5 || c.CameraCount > 10 {
		c.CameraCount = 10
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

	if sType == streamTypeLive {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return
	}

	metrics := frameMetrics{At: time.Now().UTC()}
	if grid, gErr := sampleSmallGray(raw, 64, 36); gErr == nil {
		metrics.Motion, metrics.Occupancy = deriveMetrics(nil, grid, 64, 36)
	}
	out, pErr := buildProcessedFrame(raw, metrics)
	if pErr != nil || len(out) == 0 {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
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

func (m *pipelineManager) tickOnce(ctx context.Context) {
	m.mu.RLock()
	cameras := make([]*cameraState, len(m.cameras))
	copy(cameras, m.cameras)
	cfg := m.config
	m.mu.RUnlock()

	now := time.Now().UTC()
	for _, cam := range cameras {
		m.processCamera(ctx, cam, cfg, now)
	}

	m.mu.Lock()
	m.lastTick = now
	m.mu.Unlock()
}

func (m *pipelineManager) processCamera(ctx context.Context, cam *cameraState, cfg pipelineConfig, now time.Time) {
	if len(cam.Meta.Images) == 0 {
		return
	}
	feed := cam.Meta.Images[0]
	imgRaw, err := m.client.fetchCameraImageFresh(ctx, feed.ImageURL)
	if err != nil {
		cam.Failures++
		cam.LastError = err.Error()
		m.updateCameraView(cam, frameMetrics{At: now}, "", "", err)
		if cam.Failures >= 3 && !cam.OfflineAlert && now.Sub(cam.LastAlertAt) > 60*time.Second {
			cam.OfflineAlert = true
			alert := m.emitAlert(cam, "camera_offline", 85, "camera fetch failed 3+ consecutive times", nil, now, cfg)
			m.publishAlert(alert)
		}
		return
	}
	cam.Failures = 0
	cam.OfflineAlert = false
	cam.LastError = ""

	grid, err := sampleSmallGray(imgRaw, 64, 36)
	if err != nil {
		return
	}
	metrics := frameMetrics{At: now}
	metrics.Motion, metrics.Occupancy = deriveMetrics(cam.PrevSmall, grid, 64, 36)
	cam.PrevSmall = grid

	frame := frameSnapshot{At: now, Raw: imgRaw, Metrics: metrics}
	cam.Frames = append(cam.Frames, frame)
	cam.Frames = trimFrames(cam.Frames, cfg.BufferDuration, now)

	processedRaw, err := buildProcessedFrame(imgRaw, metrics)
	if err == nil {
		liveURL, processedURL, persistErr := m.persistLiveFrames(cam.Meta.ID, imgRaw, processedRaw)
		if persistErr == nil {
			m.updateCameraView(cam, metrics, liveURL, processedURL, nil)
		} else {
			m.updateCameraView(cam, metrics, "", "", persistErr)
		}
	} else {
		m.updateCameraView(cam, metrics, "", "", err)
	}

	if cam.Pending != nil {
		cam.Pending.Frames = append(cam.Pending.Frames, frame)
		if !now.Before(cam.Pending.PostUntil) {
			alert := m.finalizePendingCapture(cam, cfg)
			if alert != nil {
				m.publishAlert(*alert)
			}
		}
		return
	}

	eventType, score, reason, ok := evaluateSignals(cam.Frames)
	if !ok {
		return
	}
	if score < 75 || now.Sub(cam.LastAlertAt) < 75*time.Second {
		return
	}
	pre := framesSince(cam.Frames, now.Add(-cfg.PreEventDuration))
	cam.Pending = &pendingCapture{
		EventType: eventType,
		Score:     score,
		Reason:    reason,
		StartedAt: now,
		PostUntil: now.Add(cfg.PostEventDuration),
		Frames:    append([]frameSnapshot{}, pre...),
	}
}

func (m *pipelineManager) updateCameraView(cam *cameraState, metrics frameMetrics, liveURL, processedURL string, viewErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	view, ok := m.views[cam.Meta.ID]
	if !ok {
		view = cameraLiveView{
			CameraID:  cam.Meta.ID,
			Location:  cam.Meta.Location,
			Roadway:   cam.Meta.Roadway,
			Direction: cam.Meta.Direction,
		}
		if len(cam.Meta.Images) > 0 {
			view.StreamURL = cam.Meta.Images[0].VideoURL
			view.SourceImageURL = cam.Meta.Images[0].ImageURL
		}
	}
	if view.SourceImageURL == "" && len(cam.Meta.Images) > 0 {
		view.SourceImageURL = cam.Meta.Images[0].ImageURL
	}
	view.LastFrameAt = metrics.At
	view.Motion = metrics.Motion
	view.Occupancy = metrics.Occupancy
	view.Failures = cam.Failures
	if liveURL != "" {
		view.LiveImageURL = liveURL
	}
	if processedURL != "" {
		view.ProcessedImageURL = processedURL
	}
	if viewErr != nil {
		view.LastError = viewErr.Error()
	} else {
		view.LastError = ""
	}
	m.views[cam.Meta.ID] = view
}

func (m *pipelineManager) persistLiveFrames(cameraID int, raw, processed []byte) (liveURL, processedURL string, err error) {
	dir := filepath.Join(m.artifactDir, "live", fmt.Sprintf("cam-%d", cameraID))
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	livePath := filepath.Join(dir, "latest.jpg")
	procPath := filepath.Join(dir, "processed.jpg")
	if err = os.WriteFile(livePath, raw, 0o644); err != nil {
		return "", "", err
	}
	if err = os.WriteFile(procPath, processed, 0o644); err != nil {
		return "", "", err
	}
	return fmt.Sprintf("/artifacts/live/cam-%d/latest.jpg", cameraID), fmt.Sprintf("/artifacts/live/cam-%d/processed.jpg", cameraID), nil
}

func (m *pipelineManager) finalizePendingCapture(cam *cameraState, cfg pipelineConfig) *incidentAlert {
	if cam.Pending == nil || len(cam.Pending.Frames) == 0 {
		cam.Pending = nil
		return nil
	}
	p := cam.Pending
	cam.Pending = nil
	alert := m.emitAlert(cam, p.EventType, p.Score, p.Reason, p.Frames, p.StartedAt, cfg)
	return &alert
}

func (m *pipelineManager) emitAlert(cam *cameraState, eventType string, score float64, reason string, frames []frameSnapshot, ts time.Time, cfg pipelineConfig) incidentAlert {
	id := fmt.Sprintf("cam-%d-%d", cam.Meta.ID, ts.UnixNano())
	conf := score / 100.0
	if conf > 1 {
		conf = 1
	}
	alert := incidentAlert{
		ID:          id,
		EventType:   eventType,
		Location:    cam.Meta.Location,
		Roadway:     cam.Meta.Roadway,
		Direction:   cam.Meta.Direction,
		CameraID:    cam.Meta.ID,
		Timestamp:   ts,
		DurationSec: int((cfg.PreEventDuration + cfg.PostEventDuration).Seconds()),
		Confidence:  conf,
		Score:       score,
		Reason:      reason,
	}
	if len(frames) > 0 {
		clip, before, after, err := m.persistArtifacts(id, frames)
		if err == nil {
			alert.ClipURL = clip
			alert.BeforeImageURL = before
			alert.AfterImageURL = after
		}
	}
	cam.LastAlertAt = ts
	return alert
}

func (m *pipelineManager) persistArtifacts(alertID string, frames []frameSnapshot) (clipURL, beforeURL, afterURL string, err error) {
	dir := filepath.Join(m.artifactDir, alertID)
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return "", "", "", err
	}
	beforePath := filepath.Join(dir, "before.jpg")
	afterPath := filepath.Join(dir, "after.jpg")
	if err = os.WriteFile(beforePath, frames[0].Raw, 0o644); err != nil {
		return "", "", "", err
	}
	if err = os.WriteFile(afterPath, frames[len(frames)-1].Raw, 0o644); err != nil {
		return "", "", "", err
	}
	gifPath := filepath.Join(dir, "event.gif")
	if err = buildGIF(gifPath, frames); err != nil {
		return "", "", "", err
	}
	return "/artifacts/" + alertID + "/event.gif", "/artifacts/" + alertID + "/before.jpg", "/artifacts/" + alertID + "/after.jpg", nil
}

func (m *pipelineManager) publishAlert(alert incidentAlert) {
	m.mu.Lock()
	m.alerts = append([]incidentAlert{alert}, m.alerts...)
	if len(m.alerts) > 500 {
		m.alerts = m.alerts[:500]
	}
	m.mu.Unlock()

	if m.webhookURL == "" {
		return
	}
	go func(a incidentAlert) {
		payload, _ := json.Marshal(a)
		req, err := http.NewRequest(http.MethodPost, m.webhookURL, bytes.NewReader(payload))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := m.httpClient.Do(req)
		if err != nil {
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			m.mu.Lock()
			for i := range m.alerts {
				if m.alerts[i].ID == a.ID {
					m.alerts[i].WebhookSent = true
					break
				}
			}
			m.mu.Unlock()
		}
	}(alert)
}

func trimFrames(frames []frameSnapshot, maxAge time.Duration, now time.Time) []frameSnapshot {
	cutoff := now.Add(-maxAge)
	idx := 0
	for idx < len(frames) && frames[idx].At.Before(cutoff) {
		idx++
	}
	if idx == 0 {
		return frames
	}
	return append([]frameSnapshot{}, frames[idx:]...)
}

func framesSince(frames []frameSnapshot, from time.Time) []frameSnapshot {
	out := make([]frameSnapshot, 0, len(frames))
	for _, f := range frames {
		if !f.At.Before(from) {
			out = append(out, f)
		}
	}
	return out
}

func evaluateSignals(frames []frameSnapshot) (eventType string, score float64, reason string, ok bool) {
	if len(frames) < 8 {
		return "", 0, "", false
	}
	window := frames
	if len(window) > 15 {
		window = window[len(window)-15:]
	}
	var motionVals, occVals []float64
	for _, f := range window {
		motionVals = append(motionVals, f.Metrics.Motion)
		occVals = append(occVals, f.Metrics.Occupancy)
	}
	avgMotion := meanFloat(motionVals)
	avgOcc := meanFloat(occVals)
	current := window[len(window)-1].Metrics
	lowMotionFrames := countWhere(window, func(f frameSnapshot) bool {
		return f.Metrics.Motion < maxFloat(0.01, avgMotion*0.45)
	})
	highOccFrames := countWhere(window, func(f frameSnapshot) bool {
		return f.Metrics.Occupancy > avgOcc+0.05
	})
	occSlope := occVals[len(occVals)-1] - occVals[0]

	stoppedScore := 0.0
	if lowMotionFrames >= 6 {
		stoppedScore += 45
	}
	if highOccFrames >= 6 {
		stoppedScore += 35
	}
	if current.Motion < 0.02 {
		stoppedScore += 15
	}

	congestionScore := 0.0
	if current.Occupancy > avgOcc+0.08 {
		congestionScore += 45
	}
	if occSlope > 0.10 {
		congestionScore += 30
	}
	if current.Motion < avgMotion*0.8 {
		congestionScore += 20
	}

	queueScore := 0.0
	if occSlope > 0.14 {
		queueScore += 45
	}
	if highOccFrames >= 7 {
		queueScore += 30
	}
	if lowMotionFrames >= 5 {
		queueScore += 20
	}

	type candidate struct {
		name  string
		score float64
	}
	candidates := []candidate{
		{name: "stopped_vehicle", score: stoppedScore},
		{name: "congestion_spike", score: congestionScore},
		{name: "queue_growth", score: queueScore},
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })
	best := candidates[0]
	if best.score < 55 {
		return "", 0, "", false
	}
	reason = fmt.Sprintf("motion=%.3f avg_motion=%.3f occupancy=%.3f avg_occupancy=%.3f occ_slope=%.3f", current.Motion, avgMotion, current.Occupancy, avgOcc, occSlope)
	return best.name, best.score, reason, true
}

func sampleSmallGray(raw []byte, w, h int) ([]uint8, error) {
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return nil, errors.New("empty image bounds")
	}
	out := make([]uint8, w*h)
	for y := 0; y < h; y++ {
		srcY := b.Min.Y + (y*(b.Dy()-1))/maxInt(1, h-1)
		for x := 0; x < w; x++ {
			srcX := b.Min.X + (x*(b.Dx()-1))/maxInt(1, w-1)
			r, g, bCol, _ := img.At(srcX, srcY).RGBA()
			gray := uint8(((299*r + 587*g + 114*bCol) / 1000) >> 8)
			out[y*w+x] = gray
		}
	}
	return out, nil
}

func deriveMetrics(prev, curr []uint8, w, h int) (motion, occupancy float64) {
	if len(curr) == 0 {
		return 0, 0
	}
	if len(prev) != len(curr) {
		return 0.05, edgeDensity(curr, w, h)
	}
	var totalDiff float64
	for i := range curr {
		d := int(curr[i]) - int(prev[i])
		if d < 0 {
			d = -d
		}
		totalDiff += float64(d) / 255.0
	}
	motion = totalDiff / float64(len(curr))
	occupancy = edgeDensity(curr, w, h)
	return motion, occupancy
}

func edgeDensity(gray []uint8, w, h int) float64 {
	if len(gray) != w*h || w < 3 || h < 3 {
		return 0
	}
	startY := h / 3
	edges := 0
	pixels := 0
	for y := startY + 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			i := y*w + x
			gx := int(gray[i+1]) - int(gray[i-1])
			if gx < 0 {
				gx = -gx
			}
			gy := int(gray[i+w]) - int(gray[i-w])
			if gy < 0 {
				gy = -gy
			}
			if gx+gy > 42 {
				edges++
			}
			pixels++
		}
	}
	if pixels == 0 {
		return 0
	}
	return float64(edges) / float64(pixels)
}

func buildGIF(path string, frames []frameSnapshot) error {
	if len(frames) == 0 {
		return errors.New("no frames")
	}
	step := 1
	if len(frames) > 24 {
		step = len(frames) / 24
	}
	anim := &gif.GIF{Image: make([]*image.Paletted, 0, len(frames)/step+1), Delay: make([]int, 0, len(frames)/step+1)}
	for i := 0; i < len(frames); i += step {
		img, _, err := image.Decode(bytes.NewReader(frames[i].Raw))
		if err != nil {
			continue
		}
		b := img.Bounds()
		if b.Dx() <= 0 || b.Dy() <= 0 {
			continue
		}
		paletted := image.NewPaletted(b, palette.Plan9)
		draw.FloydSteinberg.Draw(paletted, b, img, image.Point{})
		anim.Image = append(anim.Image, paletted)
		anim.Delay = append(anim.Delay, 6) // ~60ms * 6 => ~360ms/frame
	}
	if len(anim.Image) == 0 {
		return errors.New("failed to build gif frames")
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return gif.EncodeAll(file, anim)
}

func buildProcessedFrame(raw []byte, metrics frameMetrics) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	rgba := toRGBA(img)
	b := rgba.Bounds()
	if b.Dx() < 20 || b.Dy() < 20 {
		return nil, errors.New("image too small")
	}

	overlayH := maxInt(26, b.Dy()/9)
	fillRect(rgba, b.Min.X, b.Min.Y, b.Dx(), overlayH, color.RGBA{R: 0, G: 0, B: 0, A: 180})

	motionRatio := clamp(metrics.Motion*8, 0, 1)
	occRatio := clamp(metrics.Occupancy*2.2, 0, 1)

	barW := maxInt(50, b.Dx()/3)
	barH := maxInt(6, overlayH/4)
	padding := maxInt(8, b.Dx()/60)
	top := b.Min.Y + maxInt(6, overlayH/5)

	// motion bar (green)
	fillRect(rgba, b.Min.X+padding, top, barW, barH, color.RGBA{R: 70, G: 70, B: 70, A: 220})
	fillRect(rgba, b.Min.X+padding, top, int(float64(barW)*motionRatio), barH, color.RGBA{R: 45, G: 210, B: 80, A: 255})

	// occupancy bar (orange/red)
	top2 := top + barH + maxInt(4, overlayH/8)
	fillRect(rgba, b.Min.X+padding, top2, barW, barH, color.RGBA{R: 70, G: 70, B: 70, A: 220})
	fillRect(rgba, b.Min.X+padding, top2, int(float64(barW)*occRatio), barH, color.RGBA{R: 240, G: 120, B: 20, A: 255})

	// frame border turns red for high occupancy+low motion (incident-like visual)
	if metrics.Occupancy > 0.24 && metrics.Motion < 0.03 {
		drawBorder(rgba, b, 4, color.RGBA{R: 230, G: 35, B: 35, A: 255})
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, rgba, &jpeg.Options{Quality: 78}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func toRGBA(img image.Image) *image.RGBA {
	b := img.Bounds()
	dst := image.NewRGBA(b)
	draw.Draw(dst, b, img, b.Min, draw.Src)
	return dst
}

func fillRect(img *image.RGBA, x, y, w, h int, c color.RGBA) {
	b := img.Bounds()
	if w <= 0 || h <= 0 {
		return
	}
	x0 := maxInt(x, b.Min.X)
	y0 := maxInt(y, b.Min.Y)
	x1 := minInt(x+w, b.Max.X)
	y1 := minInt(y+h, b.Max.Y)
	for yy := y0; yy < y1; yy++ {
		for xx := x0; xx < x1; xx++ {
			img.SetRGBA(xx, yy, c)
		}
	}
}

func drawBorder(img *image.RGBA, b image.Rectangle, thickness int, c color.RGBA) {
	if thickness <= 0 {
		return
	}
	fillRect(img, b.Min.X, b.Min.Y, b.Dx(), thickness, c)
	fillRect(img, b.Min.X, b.Max.Y-thickness, b.Dx(), thickness, c)
	fillRect(img, b.Min.X, b.Min.Y, thickness, b.Dy(), c)
	fillRect(img, b.Max.X-thickness, b.Min.Y, thickness, b.Dy(), c)
}

func meanFloat(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

func countWhere(frames []frameSnapshot, fn func(frameSnapshot) bool) int {
	n := 0
	for _, f := range frames {
		if fn(f) {
			n++
		}
	}
	return n
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
