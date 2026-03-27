package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultPort            = "8080"
	defaultBaseURL         = "https://511ny.org"
	defaultUserAgent       = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36"
	defaultDetectorBaseURL = "http://localhost:8090"

	defaultRecommendedCameraCount = 10
	minRecommendedCameraCount     = 5
	maxRecommendedCameraCount     = 10

	defaultFocusFPS = 30
	minFocusFPS     = 1
	maxFocusFPS     = 30

	defaultListLength = 25
	minListLength     = 1
	maxListLength     = 100

	defaultAlertListLimit = 50
	minAlertListLimit     = 1
	maxAlertListLimit     = 200
)

var targetCorridors = []string{"I-95", "I-87", "I-278", "I-495", "I-678", "I-290", "I-81", "I-490", "I-787", "I-90"}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	port := envOrDefault("PORT", defaultPort)
	baseURL := envOrDefault("NY511_BASE_URL", defaultBaseURL)
	detectorBaseURL := envOrDefault("DETECTOR_BASE_URL", defaultDetectorBaseURL)

	client, err := newNY511Client(baseURL)
	if err != nil {
		logger.Error("failed to create 511 client", "error", err)
		os.Exit(1)
	}
	detector := newDetectorClient(detectorBaseURL)
	pipeline := newPipelineRuntime(client, detector, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", withCORS(func(w http.ResponseWriter, _ *http.Request) {
		status := pipeline.Status()
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":               true,
			"service":          "live-traffic-api",
			"time":             time.Now().UTC(),
			"pipeline_running": status.Running,
		})
	}))

	mux.HandleFunc("/api/v1/cameras", withCORS(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		start := intQuery(r, "start", 0)
		length := boundedIntQuery(r, "length", defaultListLength, minListLength, maxListLength)
		if length < minListLength || length > maxListLength {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "length must be between 1 and 100"})
			return
		}

		payload, err := client.listCameras(ctx, start, length)
		if err != nil {
			logger.Error("list cameras failed", "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}

		writeJSON(w, http.StatusOK, payload)
	}))

	mux.HandleFunc("/api/v1/cameras/recommended", withCORS(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()

		count := boundedIntQuery(r, "count", defaultRecommendedCameraCount, minRecommendedCameraCount, maxRecommendedCameraCount)
		if count < minRecommendedCameraCount || count > maxRecommendedCameraCount {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "count must be between 5 and 10"})
			return
		}

		allCameras, err := fetchAllCameras(ctx, client, 200, 2500)
		if err != nil {
			logger.Error("list cameras for recommendation failed", "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}

		recommended := selectLiveRecommended(ctx, client, allCameras, count)
		writeJSON(w, http.StatusOK, map[string]any{
			"count":           len(recommended),
			"requested_count": count,
			"live_validated":  true,
			"targets":         targetCorridors,
			"data":            recommended,
		})
	}))

	mux.HandleFunc("/api/v1/pipeline/start", withCORS(pipeline.handleStart))
	mux.HandleFunc("/api/v1/pipeline/focus/stream", withCORS(pipeline.handleFocusStream))
	mux.HandleFunc("/api/v1/pipeline/focus/detect", withCORS(pipeline.handleFocusDetect))

	mux.HandleFunc("/api/v1/analysis/plan", withCORS(func(w http.ResponseWriter, _ *http.Request) {
		// V1 keeps detection lightweight and explainable before heavier CV models.
		writeJSON(w, http.StatusOK, map[string]any{
			"inference_runtime":       "go-heuristics-v1",
			"detection_model":         "rolling-buffer + motion/occupancy heuristics",
			"tracking_model":          "none (v1)",
			"recommended_fps_per_cam": "2-5",
			"latency_target_p95_sec":  3,
			"alerts": []string{
				"congestion_score_spike",
				"stopped_vehicle",
				"queue_growth",
				"camera_offline",
			},
			"pipeline_steps": []string{
				"ingest",
				"rolling_buffer",
				"detection",
				"event_scoring",
				"trigger_and_clip",
				"alert_output_and_webhook",
			},
		})
	}))

	mux.HandleFunc("/api/v1/pipeline/start", withCORS(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "use POST"})
			return
		}
		var req startPipelineRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		cfg := req.toConfig()
		if err := pipeline.Start(r.Context(), cfg); err != nil {
			if errors.Is(err, errPipelineAlreadyRunning) {
				writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"status": pipeline.Status(),
		})
	}))

	mux.HandleFunc("/api/v1/pipeline/stop", withCORS(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "use POST"})
			return
		}
		pipeline.Stop()
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"status": pipeline.Status(),
		})
	}))

	mux.HandleFunc("/api/v1/pipeline/status", withCORS(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, pipeline.Status())
	}))

	mux.HandleFunc("/api/v1/pipeline/cameras", withCORS(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"count": len(pipeline.ListCameraViews()),
			"data":  pipeline.ListCameraViews(),
		})
	}))

	mux.HandleFunc("/api/v1/pipeline/focus/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "use GET"})
			return
		}

		cameraID := intQuery(r, "camera_id", 0)
		if cameraID <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "camera_id is required"})
			return
		}

		mode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mode")))
		if mode == "" {
			mode = "processed"
		}

		if !pipeline.HasCamera(cameraID) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not active in current pipeline"})
			return
		}

		fps := boundedIntQuery(r, "fps", defaultFocusFPS, minFocusFPS, maxFocusFPS)
		pipeline.StreamFocus(w, r, cameraID, mode, fps)
	})

	mux.HandleFunc("/api/v1/pipeline/focus/snapshot", withCORS(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "use GET"})
			return
		}
		cameraID := intQuery(r, "camera_id", 0)
		if cameraID <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "camera_id is required"})
			return
		}
		mode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mode")))
		if mode == "" {
			mode = "processed"
		}
		pipeline.WriteFocusSnapshot(r.Context(), w, cameraID, mode)
	}))

	mux.HandleFunc("/api/v1/pipeline/focus/detect", withCORS(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "use POST"})
			return
		}

		cameraID := intQuery(r, "camera_id", 0)
		if cameraID <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "camera_id is required"})
			return
		}
		if !pipeline.HasCamera(cameraID) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not active in current pipeline"})
			return
		}

		imageBytes, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read image bytes"})
			return
		}
		if len(imageBytes) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "request body must contain image bytes"})
			return
		}

		q := r.URL.Query()
		if strings.TrimSpace(q.Get("stream_id")) == "" {
			q.Set("stream_id", fmt.Sprintf("cam-%d", cameraID))
		}
		if strings.TrimSpace(q.Get("imgsz")) == "" {
			q.Set("imgsz", "640")
		}
		if strings.TrimSpace(q.Get("conf")) == "" {
			q.Set("conf", "0.25")
		}

		detectCtx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		defer cancel()
		payload, err := detector.Detect(detectCtx, imageBytes, q)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, payload)
	}))

	mux.HandleFunc("/api/v1/alerts", withCORS(func(w http.ResponseWriter, r *http.Request) {
		limit := boundedIntQuery(r, "limit", defaultAlertListLimit, minAlertListLimit, maxAlertListLimit)
		alerts := pipeline.ListAlerts(limit)
		writeJSON(w, http.StatusOK, map[string]any{
			"count": len(alerts),
			"data":  alerts,
		})
	}))

	mux.Handle("/artifacts/", withCORSHandler(http.StripPrefix("/artifacts/", http.FileServer(http.Dir(artifactDir)))))

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	logger.Info("api listening", "port", port, "baseURL", baseURL, "detectorBaseURL", detectorBaseURL)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped unexpectedly", "error", err)
		os.Exit(1)
	}
}

type ny511Client struct {
	baseURL     string
	userAgent   string
	httpClient  *http.Client
	streamMu    sync.Mutex
	streamCache map[string]streamProbeResult
}

type streamProbeResult struct {
	OK        bool
	CheckedAt time.Time
}

func newNY511Client(baseURL string) (*ny511Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create cookie jar: %w", err)
	}

	return &ny511Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		userAgent: defaultUserAgent,
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
			Jar:     jar,
		},
		streamCache: make(map[string]streamProbeResult, 256),
	}, nil
}

func (c *ny511Client) listCameras(ctx context.Context, start, length int) (*listResponse, error) {
	if err := c.primeSession(ctx); err != nil {
		return nil, err
	}

	queryModel := map[string]any{
		"start":  start,
		"length": length,
		"search": map[string]string{"value": ""},
		"order": []map[string]any{
			{"column": 1, "dir": "asc"},
			{"column": 4, "dir": "asc"},
		},
		"columns": []map[string]any{
			{"data": nil, "name": ""},
			{"name": "sortOrder"},
			{"name": "state", "s": true},
			{"name": "region", "s": true},
			{"name": "county", "s": true},
			{"name": "roadway", "s": true},
			{"data": nil, "name": ""},
		},
	}

	queryBytes, err := json.Marshal(queryModel)
	if err != nil {
		return nil, fmt.Errorf("marshal query: %w", err)
	}

	endpoint := c.baseURL + "/List/GetData/Cameras"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	q := req.URL.Query()
	q.Set("lang", "en")
	q.Set("query", string(queryBytes))
	req.URL.RawQuery = q.Encode()

	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Referer", c.baseURL+"/cctv")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request list cameras: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("list cameras status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed listResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode cameras: %w", err)
	}

	return &parsed, nil
}

func (c *ny511Client) primeSession(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/cctv", nil)
	if err != nil {
		return fmt.Errorf("create prime request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("prime session failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("prime session status: %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	return nil
}

func (c *ny511Client) fetchCameraImage(ctx context.Context, imageURL string) ([]byte, error) {
	if imageURL == "" {
		return nil, errors.New("empty image url")
	}
	targetURL := imageURL
	if strings.HasPrefix(targetURL, "/") {
		targetURL = c.baseURL + targetURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create image request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/*,*/*;q=0.8")
	req.Header.Set("Referer", c.baseURL+"/cctv")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch camera image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("camera image status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("read camera image: %w", err)
	}
	return b, nil
}

// cacheBustURL appends a unique query param so CDNs / reverse proxies don't keep
// serving the same JPEG bytes for identical snapshot URLs.
func cacheBustURL(imageURL string) string {
	sep := "?"
	if strings.Contains(imageURL, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%st=%d", imageURL, sep, time.Now().UnixNano())
}

// fetchCameraImageFresh requests the snapshot with a unique URL and no-cache headers
// so each poll can receive a newly rendered frame from 511.
func (c *ny511Client) fetchCameraImageFresh(ctx context.Context, imageURL string) ([]byte, error) {
	if imageURL == "" {
		return nil, errors.New("empty image url")
	}
	targetURL := imageURL
	if strings.HasPrefix(targetURL, "/") {
		targetURL = c.baseURL + targetURL
	}
	targetURL = cacheBustURL(targetURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create image request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/*,*/*;q=0.8")
	req.Header.Set("Referer", c.baseURL+"/cctv")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch camera image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("camera image status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("read camera image: %w", err)
	}
	return b, nil
}

func (c *ny511Client) streamLooksLive(ctx context.Context, streamURL string) bool {
	if strings.TrimSpace(streamURL) == "" {
		return false
	}

	now := time.Now().UTC()
	c.streamMu.Lock()
	cached, ok := c.streamCache[streamURL]
	if ok && now.Sub(cached.CheckedAt) < 2*time.Minute {
		c.streamMu.Unlock()
		return cached.OK
	}
	c.streamMu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, streamURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/vnd.apple.mpegurl,application/x-mpegURL,text/plain,*/*")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.cacheStreamProbe(streamURL, false)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.cacheStreamProbe(streamURL, false)
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		c.cacheStreamProbe(streamURL, false)
		return false
	}
	text := strings.ToUpper(string(body))
	ok = strings.Contains(text, "#EXTM3U") || strings.Contains(text, "#EXTINF") || strings.Contains(text, "#EXT-X-STREAM-INF")
	c.cacheStreamProbe(streamURL, ok)
	return ok
}

func (c *ny511Client) cacheStreamProbe(streamURL string, ok bool) {
	c.streamMu.Lock()
	c.streamCache[streamURL] = streamProbeResult{OK: ok, CheckedAt: time.Now().UTC()}
	c.streamMu.Unlock()
}

type listResponse struct {
	Draw            int      `json:"draw"`
	RecordsTotal    int      `json:"recordsTotal"`
	RecordsFiltered int      `json:"recordsFiltered"`
	Data            []camera `json:"data"`
}

type camera struct {
	RowID     string       `json:"DT_RowId"`
	ID        int          `json:"id"`
	Roadway   string       `json:"roadway"`
	Direction string       `json:"direction"`
	Location  string       `json:"location"`
	Region    string       `json:"region"`
	County    string       `json:"county"`
	City      string       `json:"city"`
	Images    []cameraFeed `json:"images"`
}

type cameraFeed struct {
	ID                  int    `json:"id"`
	CameraSiteID        int    `json:"cameraSiteId"`
	ImageURL            string `json:"imageUrl"`
	VideoURL            string `json:"videoUrl"`
	VideoType           string `json:"videoType"`
	Description         string `json:"description"`
	IsVideoAuthRequired bool   `json:"isVideoAuthRequired"`
	VideoDisabled       bool   `json:"videoDisabled"`
	Disabled            bool   `json:"disabled"`
	Blocked             bool   `json:"blocked"`
}

type scoredCamera struct {
	Score int    `json:"score"`
	Why   string `json:"why"`
	camera
}

func recommendCameras(cameras []camera, count int) []scoredCamera {
	if count <= 0 {
		return nil
	}
	targetRoadways := []string{"I-95", "I-87", "I-278", "I-495", "I-678", "I-290", "I-81", "I-490", "I-787", "I-90"}
	priority := make(map[string]int, len(targetRoadways))
	for i, roadway := range targetRoadways {
		priority[roadway] = len(targetRoadways) - i
	}

	scored := make([]scoredCamera, 0, len(cameras))
	for _, c := range cameras {
		if len(c.Images) == 0 {
			continue
		}
		f := c.Images[0]
		if f.VideoURL == "" || f.VideoDisabled || f.Disabled || f.Blocked || f.IsVideoAuthRequired {
			continue
		}

		roadway := strings.ToUpper(c.Roadway)
		corridor := corridorForRoadway(roadway)
		score := 1
		reason := "has live video"
		for k, v := range priority {
			if strings.Contains(roadway, k) {
				score += v * 10
				reason = "target corridor " + k
				break
			}
		}
		if strings.Contains(strings.ToUpper(c.Location), "INTERCHANGE") {
			score += 4
			reason += "; interchange view"
		}
		if c.Direction != "" && !strings.EqualFold(c.Direction, "Unknown") {
			score += 2
		}

		scored = append(scored, scoredCamera{
			Score:  score,
			Why:    reason + "; " + corridor,
			camera: c,
		})
	}

	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})

	// Prefer corridor diversity in the first pass, then fill remaining slots.
	selected := make([]scoredCamera, 0, count)
	perCorridor := map[string]int{}
	for _, c := range scored {
		corridor := corridorForRoadway(strings.ToUpper(c.Roadway))
		if corridor == "" {
			corridor = "other"
		}
		if perCorridor[corridor] >= 1 {
			continue
		}
		selected = append(selected, c)
		perCorridor[corridor]++
		if len(selected) >= count {
			return selected
		}
	}
	for _, c := range scored {
		if len(selected) >= count {
			break
		}
		selected = append(selected, c)
	}

	if len(selected) > count {
		return selected[:count]
	}
	return selected
}

func envOrDefault(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}
