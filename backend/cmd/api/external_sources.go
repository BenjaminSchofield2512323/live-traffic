package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type externalSourceCamera struct {
	ID            int          `json:"id"`
	Source        string       `json:"source"`
	ExternalCamID string       `json:"external_cam_id"`
	Title         string       `json:"title"`
	Roadway       string       `json:"roadway"`
	Location      string       `json:"location"`
	Direction     string       `json:"direction"`
	City          string       `json:"city"`
	Region        string       `json:"region"`
	StreamURL     string       `json:"stream_url"`
	PageURL       string       `json:"page_url"`
	Why           string       `json:"why"`
	Score         int          `json:"score"`
	Images        []cameraFeed `json:"images"`
}

type earthCamPageResult struct {
	Count       int                  `json:"count"`
	Total       int                  `json:"total"`
	Page        int                  `json:"page"`
	PageSize    int                  `json:"page_size"`
	HasNextPage bool                 `json:"has_next_page"`
	Data        []externalSourceCamera `json:"data"`
}

func (r earthCamPageResult) asScored() []scoredCamera {
	if len(r.Data) == 0 {
		return nil
	}
	out := make([]scoredCamera, 0, len(r.Data))
	for _, ext := range r.Data {
		sc := scoredCamera{
			Score: ext.Score,
			Why:   ext.Why,
			camera: camera{
				RowID:     fmt.Sprintf("external-%d", ext.ID),
				ID:        ext.ID,
				Roadway:   ext.Roadway,
				Direction: ext.Direction,
				Location:  ext.Location,
				Region:    ext.Region,
				City:      ext.City,
				County:    "",
				Source:    ext.Source,
				PageURL:   ext.PageURL,
				Images:    ext.Images,
			},
		}
		out = append(out, sc)
	}
	return out
}

func (c externalSourceCamera) asScoredCamera() scoredCamera {
	return scoredCamera{
		Score: c.Score,
		Why:   c.Why,
		camera: camera{
			RowID:     fmt.Sprintf("external-%d", c.ID),
			ID:        c.ID,
			Roadway:   c.Roadway,
			Direction: c.Direction,
			Location:  c.Location,
			Region:    c.Region,
			City:      c.City,
			County:    "",
			Source:    c.Source,
			PageURL:   c.PageURL,
			Images:    c.Images,
		},
	}
}

type earthCamClient struct {
	http *http.Client
}

func newEarthCamClient() *earthCamClient {
	return &earthCamClient{
		http: &http.Client{
			Timeout: 12 * time.Second,
		},
	}
}

func (c *earthCamClient) heraldSquare(ctx context.Context) (externalSourceCamera, error) {
	const (
		id      = 900001
		pageURL = "https://www.earthcam.com/usa/newyork/heraldsquare/?cam=heraldsquare_nyc"
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return externalSourceCamera{}, fmt.Errorf("build earthcam request: %w", err)
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", "https://www.earthcam.com/")

	resp, err := c.http.Do(req)
	if err != nil {
		return externalSourceCamera{}, fmt.Errorf("earthcam request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return externalSourceCamera{}, fmt.Errorf("earthcam status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return externalSourceCamera{}, fmt.Errorf("earthcam read failed: %w", err)
	}
	html := string(body)
	streamURL := extractEarthCamHLSURL(html)
	if streamURL == "" {
		return externalSourceCamera{}, fmt.Errorf("earthcam stream not found")
	}
	normalizedStream := normalizeEarthCamURL(streamURL)
	return externalSourceCamera{
		ID:            id,
		Source:        "earthcam",
		ExternalCamID: "heraldsquare_nyc",
		Title:         "Herald Square Cam",
		Roadway:       "Herald Square",
		Location:      "Midtown Manhattan, New York City",
		Direction:     "Street-level plaza and traffic",
		City:          "New York",
		Region:        "NYC",
		StreamURL:     normalizedStream,
		PageURL:       pageURL,
		Why:           "EarthCam people + vehicle urban scene",
		Score:         90,
		Images: []cameraFeed{
			{
				ID:           id,
				CameraSiteID: id,
				ImageURL:    "",
				VideoURL:    normalizedStream,
				VideoType:   "HLS",
				Description: "EarthCam Herald Square",
			},
		},
	}, nil
}

// After JSON-style \/ → / normalization, match HLS playlist URLs in the page.
var earthCamHLSPlainPattern = regexp.MustCompile(`https?://[^\s"'<>]+\.m3u8[^\s"'<>]*`)

func extractEarthCamHLSURL(html string) string {
	if strings.TrimSpace(html) == "" {
		return ""
	}
	// EarthCam embeds URLs in JSON with escaped slashes: https:\/\/host\/path\/x.m3u8
	// A regex that forbids backslashes in the path stops at the first \/.
	flat := strings.ReplaceAll(html, `\/`, `/`)
	flat = strings.ReplaceAll(flat, `\u0026`, "&")
	matches := earthCamHLSPlainPattern.FindAllString(flat, -1)
	if len(matches) == 0 {
		return ""
	}
	// Prefer live fecnetwork HLS over archive / backup playlists when both appear.
	for _, m := range matches {
		u := strings.TrimSpace(m)
		if strings.Contains(u, "fecnetwork") && strings.Contains(u, ".m3u8") {
			return u
		}
	}
	return strings.TrimSpace(matches[0])
}

func normalizeEarthCamURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	// Keep query params (e.g. t, td); EarthCam HLS often requires tokenized URLs.
	return u.String()
}

func listExternalCameras(ctx context.Context, includeEarthCam bool) []externalSourceCamera {
	if !includeEarthCam {
		return nil
	}
	client := newEarthCamClient()
	cam, err := client.heraldSquare(ctx)
	if err != nil {
		return nil
	}
	return []externalSourceCamera{cam}
}

var earthCamCatalog = []string{"heraldsquare_nyc"}

func listEarthCamPage(ctx context.Context, page, pageSize int) earthCamPageResult {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 5
	}
	all := listExternalCameras(ctx, true)
	total := len(all)
	start := (page - 1) * pageSize
	if start >= total {
		return earthCamPageResult{
			Count:       0,
			Total:       total,
			Page:        page,
			PageSize:    pageSize,
			HasNextPage: false,
			Data:        []externalSourceCamera{},
		}
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	items := all[start:end]
	return earthCamPageResult{
		Count:       len(items),
		Total:       total,
		Page:        page,
		PageSize:    pageSize,
		HasNextPage: end < total,
		Data:        items,
	}
}
