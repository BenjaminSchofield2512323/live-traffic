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

type detectorClient struct {
	baseURL string
	http    *http.Client
}

type detectQuery struct {
	StreamID                string
	ImgSize                 int
	Conf                    float64
	IOU                     float64
	ROI                     string
	Lanes                   string
	Direction               string
	MovingSpeedThresholdPxS float64
	SmoothingWindowSec      float64
	DebugOverlay            *bool
	ExtraQuery              url.Values
}

type detectorPayload map[string]any

func newDetectorClient(baseURL string) *detectorClient {
	return &detectorClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		http: &http.Client{
			Timeout: 8 * time.Second,
		},
	}
}

func (d *detectorClient) Detect(ctx context.Context, imageBytes []byte, q detectQuery) (detectorPayload, error) {
	if len(imageBytes) == 0 {
		return nil, fmt.Errorf("empty image payload")
	}
	if d.baseURL == "" {
		return nil, fmt.Errorf("detector base URL is empty")
	}

	endpoint, err := url.Parse(d.baseURL + "/internal/detect")
	if err != nil {
		return nil, fmt.Errorf("invalid detector URL: %w", err)
	}
	params := endpoint.Query()
	if strings.TrimSpace(q.StreamID) != "" {
		params.Set("stream_id", strings.TrimSpace(q.StreamID))
	}
	if q.ImgSize > 0 {
		params.Set("imgsz", fmt.Sprintf("%d", q.ImgSize))
	}
	if q.Conf > 0 {
		params.Set("conf", fmt.Sprintf("%.4f", q.Conf))
	}
	if q.IOU > 0 {
		params.Set("iou", fmt.Sprintf("%.4f", q.IOU))
	}
	if v := strings.TrimSpace(q.ROI); v != "" {
		params.Set("roi", v)
	}
	if v := strings.TrimSpace(q.Lanes); v != "" {
		params.Set("lanes", v)
	}
	if v := strings.TrimSpace(q.Direction); v != "" {
		params.Set("direction", v)
	}
	if q.MovingSpeedThresholdPxS > 0 {
		params.Set("moving_speed_threshold_px_s", fmt.Sprintf("%.4f", q.MovingSpeedThresholdPxS))
	}
	if q.SmoothingWindowSec > 0 {
		params.Set("smoothing_window_sec", fmt.Sprintf("%.4f", q.SmoothingWindowSec))
	}
	if q.DebugOverlay != nil {
		if *q.DebugOverlay {
			params.Set("debug_overlay", "true")
		} else {
			params.Set("debug_overlay", "false")
		}
	}
	// Backward-compatible query passthrough for clients using generic params.
	if q.ExtraQuery != nil {
		for key, vals := range q.ExtraQuery {
			k := strings.TrimSpace(key)
			if k == "" || len(vals) == 0 {
				continue
			}
			v := strings.TrimSpace(vals[len(vals)-1])
			if v == "" {
				continue
			}
			params.Set(k, v)
		}
	}
	endpoint.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(imageBytes))
	if err != nil {
		return nil, fmt.Errorf("build detector request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("detector request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return nil, fmt.Errorf("detector status %d: %s", resp.StatusCode, msg)
	}

	var payload detectorPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode detector payload: %w", err)
	}
	return payload, nil
}
