package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func selectRecommendedCameras(ctx context.Context, client *ny511Client, ranked []scoredCamera, count int) ([]scoredCamera, int) {
	if count <= 0 || len(ranked) == 0 {
		return nil, 0
	}

	selected := make([]scoredCamera, 0, count)
	selectedIDs := make(map[int]struct{}, count)
	liveProbePass := 0

	// Phase 1: keep only streams that pass a quick HLS liveness probe.
	for _, cam := range ranked {
		if len(selected) >= count {
			break
		}
		if len(cam.Images) == 0 {
			continue
		}
		if !streamLooksLive(ctx, client, cam.Images[0].VideoURL) {
			continue
		}
		selected = append(selected, cam)
		selectedIDs[cam.ID] = struct{}{}
		liveProbePass++
	}

	// Phase 2: fill remaining slots from the same ranking without probing.
	for _, cam := range ranked {
		if len(selected) >= count {
			break
		}
		if _, exists := selectedIDs[cam.ID]; exists {
			continue
		}
		selected = append(selected, cam)
		selectedIDs[cam.ID] = struct{}{}
	}

	return selected, liveProbePass
}

func streamLooksLive(ctx context.Context, client *ny511Client, streamURL string) bool {
	streamURL = strings.TrimSpace(streamURL)
	if streamURL == "" || !looksLikeHLSURL(streamURL) {
		return false
	}

	probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, absoluteUpstreamURL(client.baseURL, streamURL), nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", client.userAgent)
	req.Header.Set("Accept", "application/vnd.apple.mpegurl,application/x-mpegURL,text/plain,*/*")

	resp, err := client.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return false
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return false
	}

	playlist := strings.ToUpper(string(body))
	if !strings.Contains(playlist, "#EXTM3U") {
		return false
	}

	return strings.Contains(playlist, "#EXTINF") || strings.Contains(playlist, "#EXT-X-STREAM-INF")
}

func looksLikeHLSURL(raw string) bool {
	raw = strings.ToLower(strings.TrimSpace(raw))
	return strings.Contains(raw, ".m3u8")
}

func absoluteUpstreamURL(baseURL, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return raw
	}

	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return raw
	}
	relative, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return base.ResolveReference(relative).String()
}
