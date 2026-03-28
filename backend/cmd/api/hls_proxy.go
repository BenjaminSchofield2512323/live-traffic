package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Matches URI="..." in HLS tags (#EXT-X-KEY, #EXT-X-MAP, etc.).
var hlsTagURIAttr = regexp.MustCompile(`URI="([^"]+)"`)

// parseAllowedEarthCamProxyURL validates u for GET proxying: HTTPS only, earthcam.com host.
func parseAllowedEarthCamProxyURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("invalid url")
	}
	host := strings.ToLower(u.Hostname())
	if !strings.HasSuffix(host, "earthcam.com") {
		return nil, fmt.Errorf("host not allowed")
	}
	return u, nil
}

func isEarthCamHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return strings.HasSuffix(host, "earthcam.com")
}

// proxyPathForEarthCamURL returns a same-origin path the browser can load; the loader forwards to EarthCam.
func proxyPathForEarthCamURL(abs *url.URL) string {
	return "/api/v1/stream/hls-proxy?u=" + url.QueryEscape(abs.String())
}

func resolveAgainstPlaylistBase(base *url.URL, ref string) (*url.URL, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("empty ref")
	}
	refU, err := url.Parse(ref)
	if err != nil {
		return nil, err
	}
	return base.ResolveReference(refU), nil
}

// rewriteEarthCamM3U8Body rewrites playlist lines so relative URIs do not resolve against our proxy path
// (which would produce bogus /api/v1/stream/chunklist_*.m3u8 URLs). Each resource points at hls-proxy?u=<upstream>.
func rewriteEarthCamM3U8Body(body string, playlistURL *url.URL) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "#") {
			if strings.Contains(line, `URI="`) {
				line = hlsTagURIAttr.ReplaceAllStringFunc(line, func(m string) string {
					sm := hlsTagURIAttr.FindStringSubmatch(m)
					if len(sm) < 2 {
						return m
					}
					resolved, err := resolveAgainstPlaylistBase(playlistURL, sm[1])
					if err != nil || !isEarthCamHost(resolved.Hostname()) {
						return m
					}
					return `URI="` + proxyPathForEarthCamURL(resolved) + `"`
				})
			}
			lines[i] = line
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		resolved, err := resolveAgainstPlaylistBase(playlistURL, trimmed)
		if err != nil || !isEarthCamHost(resolved.Hostname()) {
			continue
		}
		lines[i] = proxyPathForEarthCamURL(resolved)
	}
	return strings.Join(lines, "\n")
}

func looksLikeHLSPlaylist(body []byte, ct string) bool {
	ct = strings.ToLower(ct)
	if strings.Contains(ct, "mpegurl") || strings.Contains(ct, "m3u8") {
		return true
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 8 && strings.HasPrefix(s, "#EXTM3U") {
		return true
	}
	return false
}

// handleEarthCamHLSProxy forwards HLS playlists and segments to EarthCam with a browser-like Referer.
// Browsers cannot set Referer on XHR/fetch, so the app loads same-origin URLs that proxy here.
func handleEarthCamHLSProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "use GET"})
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("u"))
	if raw == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing u"})
		return
	}
	target, err := parseAllowedEarthCamProxyURL(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request"})
		return
	}
	req.Header.Set("Referer", "https://www.earthcam.com/")
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "*/*")
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	if ir := r.Header.Get("If-Range"); ir != "" {
		req.Header.Set("If-Range", ir)
	}

	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	bodyBytes, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": readErr.Error()})
		return
	}

	ct := resp.Header.Get("Content-Type")
	out := bodyBytes
	if resp.StatusCode == http.StatusOK && looksLikeHLSPlaylist(bodyBytes, ct) {
		out = []byte(rewriteEarthCamM3U8Body(string(bodyBytes), target))
	}

	for _, k := range []string{
		"Content-Range",
		"Accept-Ranges",
		"Cache-Control",
		"Last-Modified",
		"ETag",
	} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	} else if resp.StatusCode == http.StatusOK && looksLikeHLSPlaylist(bodyBytes, "") {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}
