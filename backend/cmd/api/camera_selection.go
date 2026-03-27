package main

import (
	"context"
	"fmt"
)

const (
	defaultCameraPageSize   = 200
	defaultCameraMaxRecords = 2500
	minRecommendedCount     = 5
	maxRecommendedCount     = 10
)

func fetchAllCameras(ctx context.Context, client *ny511Client, pageSize, maxRecords int) ([]camera, error) {
	if pageSize < 1 {
		pageSize = defaultCameraPageSize
	}
	if maxRecords < pageSize {
		maxRecords = pageSize
	}

	start := 0
	all := make([]camera, 0, pageSize*2)
	for {
		payload, err := client.listCameras(ctx, start, pageSize)
		if err != nil {
			return nil, fmt.Errorf("fetch cameras page start=%d: %w", start, err)
		}
		all = append(all, payload.Data...)
		start += pageSize
		if len(payload.Data) == 0 || start >= payload.RecordsFiltered || len(all) >= maxRecords {
			break
		}
	}
	return all, nil
}

func selectLiveRecommended(ctx context.Context, client *ny511Client, cameras []camera, count int) []scoredCamera {
	if count <= 0 {
		return nil
	}
	ranked := recommendCameras(cameras, len(cameras))
	out := make([]scoredCamera, 0, count)
	seen := make(map[int]struct{}, count)
	for _, cam := range ranked {
		if len(out) >= count {
			break
		}
		if _, exists := seen[cam.ID]; exists {
			continue
		}
		if len(cam.Images) == 0 {
			continue
		}
		feed := cam.Images[0]
		if !client.streamLooksLive(ctx, feed.VideoURL) {
			continue
		}
		out = append(out, cam)
		seen[cam.ID] = struct{}{}
	}
	return out
}
