package main

import (
	"strings"
	"testing"
)

func TestExtractEarthCamHLSURL_JSONEscapedSlashes(t *testing.T) {
	const snippet = `"stream":"https:\/\/videos-3.earthcam.com\/fecnetwork\/23505.flv\/playlist.m3u8?t=abc&td=1"`
	got := extractEarthCamHLSURL(snippet)
	if got == "" {
		t.Fatal("expected non-empty m3u8 URL")
	}
	want := "https://videos-3.earthcam.com/fecnetwork/23505.flv/playlist.m3u8"
	if !strings.HasPrefix(got, want) {
		t.Fatalf("unexpected URL: %q", got)
	}
}

func TestExtractEarthCamHLSURL_PrefersFecnetwork(t *testing.T) {
	const snippet = `backup":"https:\/\/video2archives.earthcam.com\/x\/playlist.m3u8","live":"https:\/\/videos-3.earthcam.com\/fecnetwork\/23505.flv\/playlist.m3u8?t=1"`
	got := extractEarthCamHLSURL(snippet)
	if !strings.Contains(got, "fecnetwork") {
		t.Fatalf("expected fecnetwork URL, got %q", got)
	}
}
