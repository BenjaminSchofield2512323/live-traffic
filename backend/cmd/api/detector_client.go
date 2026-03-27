package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultDetectorBaseURL = "http://localhost:8090"

type detectorClient struct {
	baseURL    string
	httpClient *http.Client
}

func newDetectorClient(baseURL string) (*detectorClient, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = defaultDetectorBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse detector base url: %w", err)
	}
	return &detectorClient{
		baseURL: strings.TrimRight(parsed.String(), "/"),
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}, nil
}

func (c *detectorClient) Detect(ctx context.Context, imageBytes []byte, query url.Values) (map[string]any, error) {
	if len(imageBytes) == 0 {
		return nil, fmt.Errorf("empty image bytes")
	}
	endpoint := c.baseURL + "/internal/detect"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(imageBytes))
	if err != nil {
		return nil, fmt.Errorf("build detector request: %w", err)
	}

	q := req.URL.Query()
	for _, key := range []string{
		"stream_id",
		"conf",
		"iou",
		"imgsz",
		"roi",
		"moving_speed_threshold_px_s",
	} {
		if v := strings.TrimSpace(query.Get(key)); v != "" {
			q.Set(key, v)
		}
	}
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call detector: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("detector status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode detector response: %w", err)
	}
	return payload, nil
}
