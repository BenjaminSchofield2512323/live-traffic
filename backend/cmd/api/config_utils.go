package main

import (
	"net/http"
	"strconv"
	"strings"
)

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

func boundedIntQuery(r *http.Request, key string, fallback, minValue, maxValue int) int {
	v := intQuery(r, key, fallback)
	if v < minValue {
		return minValue
	}
	if v > maxValue {
		return maxValue
	}
	return v
}

func corridorForRoadway(roadway string) string {
	for _, c := range targetCorridors {
		if strings.Contains(roadway, c) {
			return c
		}
	}
	return ""
}
