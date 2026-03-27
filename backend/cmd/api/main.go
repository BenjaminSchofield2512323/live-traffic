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
	"strconv"
	"strings"
	"time"
)

const (
	defaultPort      = "8080"
	defaultBaseURL   = "https://511ny.org"
	defaultUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36"

	defaultRecommendedCameraCount = 5
	minRecommendedCameraCount     = 1
	maxRecommendedCameraCount     = 10

	defaultPipelineCameraCount = 5
	minPipelineCameraCount     = 5
	maxPipelineCameraCount     = 10
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	port := envOrDefault("PORT", defaultPort)
	baseURL := envOrDefault("NY511_BASE_URL", defaultBaseURL)

	client, err := newNY511Client(baseURL)
	if err != nil {
		logger.Error("failed to create 511 client", "error", err)
		os.Exit(1)
	}
	pipeline := newPipelineRuntime(client, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", withCORS(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"service": "live-traffic-api",
			"time":    time.Now().UTC(),
		})
	}))

	mux.HandleFunc("/api/v1/cameras", withCORS(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		start := intQuery(r, "start", 0)
		length := intQuery(r, "length", 25)
		if length < 1 || length > 100 {
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

		count := intQuery(r, "count", defaultRecommendedCameraCount)
		if count < minRecommendedCameraCount || count > maxRecommendedCameraCount {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "count must be between 1 and 10"})
			return
		}

		payload, err := client.listCameras(ctx, 0, 250)
		if err != nil {
			logger.Error("list cameras for recommendation failed", "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}

		ranked := recommendCameras(payload.Data, maxRecommendedCameraCount)
		recommended, liveProbePass := selectRecommendedCameras(ctx, client, ranked, count)
		writeJSON(w, http.StatusOK, map[string]any{
			"count":                  len(recommended),
			"live_stream_probe_pass": liveProbePass,
			"targets":                []string{"I-95", "I-87", "I-278", "I-495", "I-678", "I-290", "I-81", "I-490", "I-787", "I-90"},
			"data":                   recommended,
		})
	}))

	mux.HandleFunc("/api/v1/pipeline/start", withCORS(pipeline.handleStart))
	mux.HandleFunc("/api/v1/pipeline/focus/stream", withCORS(pipeline.handleFocusStream))

	mux.HandleFunc("/api/v1/analysis/plan", withCORS(func(w http.ResponseWriter, _ *http.Request) {
		// This endpoint keeps model choice explicit while we stand up inference workers.
		writeJSON(w, http.StatusOK, map[string]any{
			"inference_runtime":       "python-sidecar",
			"detection_model":         "yolo (latest family, tuned for vehicles)",
			"tracking_model":          "bytetrack",
			"recommended_fps_per_cam": "2-5",
			"latency_target_p95_sec":  3,
			"alerts": []string{
				"congestion_score_spike",
				"stopped_vehicle",
				"queue_growth",
				"camera_offline",
			},
		})
	}))

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	logger.Info("api listening", "port", port, "baseURL", baseURL)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped unexpectedly", "error", err)
		os.Exit(1)
	}
}

type ny511Client struct {
	baseURL    string
	userAgent  string
	httpClient *http.Client
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
		eligibleFeedIndex := firstVideoEligibleFeedIndex(c.Images)
		if eligibleFeedIndex < 0 {
			continue
		}
		if eligibleFeedIndex > 0 {
			// Keep the selected video-capable feed at index 0 so downstream consumers
			// can safely use images[0] for video/snapshot links.
			images := make([]cameraFeed, 0, len(c.Images))
			images = append(images, c.Images[eligibleFeedIndex])
			images = append(images, c.Images[:eligibleFeedIndex]...)
			images = append(images, c.Images[eligibleFeedIndex+1:]...)
			c.Images = images
		}

		roadway := strings.ToUpper(c.Roadway)
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
			Why:    reason,
			camera: c,
		})
	}

	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})

	if len(scored) > count {
		return scored[:count]
	}
	return scored
}

func firstVideoEligibleFeedIndex(feeds []cameraFeed) int {
	for i, feed := range feeds {
		if isVideoEligibleFeed(feed) {
			return i
		}
	}
	return -1
}

func isVideoEligibleFeed(feed cameraFeed) bool {
	return strings.TrimSpace(feed.VideoURL) != "" &&
		!feed.VideoDisabled &&
		!feed.Disabled &&
		!feed.Blocked &&
		!feed.IsVideoAuthRequired
}

func buildAbsoluteURL(raw string) string {
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return raw
	}
	return "https://511ny.org" + raw
}

func envOrDefault(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}

func intQuery(r *http.Request, key string, fallback int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}

func withCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}
