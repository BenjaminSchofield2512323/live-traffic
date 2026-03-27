package main

import (
	"bytes"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
)

// sampleSmallGray decodes image bytes and downsamples to a small grayscale grid.
func sampleSmallGray(imageBytes []byte, outW, outH int) ([]uint8, error) {
	if outW <= 0 || outH <= 0 {
		return nil, fmt.Errorf("invalid dimensions")
	}
	img, _, err := image.Decode(bytes.NewReader(imageBytes))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	if srcW <= 0 || srcH <= 0 {
		return nil, fmt.Errorf("empty image bounds")
	}
	out := make([]uint8, outW*outH)
	for y := 0; y < outH; y++ {
		for x := 0; x < outW; x++ {
			sx := b.Min.X + (x*srcW)/outW
			sy := b.Min.Y + (y*srcH)/outH
			r, g, bl, _ := img.At(sx, sy).RGBA()
			gray := (19595*r + 38470*g + 7471*bl + 1<<15) >> 24
			out[y*outW+x] = uint8(gray)
		}
	}
	return out, nil
}

// deriveMetrics compares consecutive grayscale samples (same length) for motion; occupancy is mean intensity.
func deriveMetrics(prev, grid []uint8, w, h int) (float64, float64) {
	_ = w
	_ = h
	if len(grid) == 0 {
		return 0, 0
	}
	var occ float64
	for _, v := range grid {
		occ += float64(v)
	}
	occupancy := occ / float64(len(grid)*255)
	if len(prev) != len(grid) {
		return 0, occupancy
	}
	var diff float64
	for i := range grid {
		d := int(grid[i]) - int(prev[i])
		if d < 0 {
			d = -d
		}
		diff += float64(d)
	}
	motion := diff / float64(len(grid)*255)
	return motion, occupancy
}

// buildProcessedFrame may draw overlays; for now returns the input (still JPEG bytes).
func buildProcessedFrame(raw []byte, m frameMetrics) ([]byte, error) {
	_ = m
	return raw, nil
}
