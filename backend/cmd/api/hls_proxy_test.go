package main

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestParseAllowedEarthCamProxyURL(t *testing.T) {
	t.Parallel()
	_, err := parseAllowedEarthCamProxyURL("http://videos-3.earthcam.com/x.m3u8")
	if err == nil {
		t.Fatal("expected error for http")
	}
	_, err = parseAllowedEarthCamProxyURL("https://evil.com/..?u=https://videos-3.earthcam.com/x.m3u8")
	if err == nil {
		t.Fatal("expected error for non-earthcam host")
	}
	u, err := parseAllowedEarthCamProxyURL("https://videos-3.earthcam.com/fecnetwork/1.flv/playlist.m3u8?t=1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Hostname() != "videos-3.earthcam.com" {
		t.Fatalf("host: %s", u.Hostname())
	}
}

func TestRewriteEarthCamM3U8Body_relativeChunklist(t *testing.T) {
	t.Parallel()
	base, err := url.Parse("https://videos-3.earthcam.com/fecnetwork/23505.flv/playlist.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	body := "#EXTM3U\n#EXT-X-VERSION:3\nchunklist_w1127514463.m3u8?t=abc\n"
	out := rewriteEarthCamM3U8Body(body, base)
	if !strings.Contains(out, "/api/v1/stream/hls-proxy?u=") {
		t.Fatalf("expected proxy path: %q", out)
	}
	// A bare relative chunklist line would make hls.js resolve against /api/v1/stream/ (404).
	bareChunklist, _ := regexp.MatchString(`(?m)^chunklist_[^\s]+\.m3u8`, out)
	if bareChunklist {
		t.Fatalf("unexpected bare relative chunklist line: %q", out)
	}
}
